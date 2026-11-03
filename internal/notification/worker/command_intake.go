package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func (s *Store) TelegramOffset(ctx context.Context, account string) (offset int64, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := readTelegramRecoveryCursor(tx, account); err != nil {
			return err
		}
		err := tx.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", account).Scan(&offset)
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	})
	return
}

// PersistTelegram commits normalized events or minimal rejection evidence before
// a subsequent poll can acknowledge the provider page. Domain conversion resumes
// separately and cannot lose the request if the process exits after this commit.
func (s *Store) PersistTelegram(ctx context.Context, page provider.IntakePage) error {
	if s.config.Interaction == nil || !protocol.ValidToken(page.Account) || page.Next < 0 || len(page.Events) > 16 {
		return fmt.Errorf("invalid intake page")
	}
	known := false
	for _, d := range s.config.Interaction.Destinations {
		if d.Provider == "telegram" && d.Account == page.Account {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("account not enrolled")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		recovery, err := readTelegramRecoveryCursor(tx, page.Account)
		if err != nil {
			return err
		}
		var offset int64
		err = tx.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", page.Account).Scan(&offset)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if page.Next < offset {
			return fmt.Errorf("intake cursor decreased")
		}
		var pending int
		if err = tx.QueryRow("SELECT count(*) FROM provider_inbox WHERE state='pending'").Scan(&pending); err != nil {
			return err
		}
		expectedNext := offset
		for _, event := range page.Events {
			if event.ID < 0 || event.ID >= page.Next || !safefs.DigestPattern.MatchString(event.Hash) || (event.Inbound == nil) == (event.Rejection == "") {
				return fmt.Errorf("invalid intake event")
			}
			if event.ID >= expectedNext {
				expectedNext = event.ID + 1
			}
			var old string
			err = tx.QueryRow("SELECT hash FROM provider_inbox WHERE account=? AND id=?", page.Account, event.ID).Scan(&old)
			if err == nil {
				if old != event.Hash {
					return fmt.Errorf("provider identity conflict")
				}
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
			if event.ID < offset {
				return fmt.Errorf("missing retained intake evidence")
			}
			body := ""
			state := "rejected"
			if event.Inbound != nil {
				in := event.Inbound
				if in.Provider != "telegram" || in.Account != page.Account || in.NativeID != fmt.Sprintf("update-%d", event.ID) || len(in.Text) > 4096 {
					return fmt.Errorf("inbound identity mismatch")
				}
				authorized := false
				for _, permission := range s.config.Interaction.Permissions {
					if permission.Actor == in.Actor && permission.Conversation == in.Conversation {
						authorized = true
					}
				}
				destinationOK := false
				for _, d := range s.config.Interaction.Destinations {
					if d.ID == in.Conversation && d.Account == in.Account && d.Provider == in.Provider && d.Fingerprint() == in.DestinationFingerprint {
						destinationOK = true
					}
				}
				if (recovery == nil || in.SentAt.After(recovery.NotBefore)) && authorized && destinationOK && !in.SentAt.IsZero() && !in.SentAt.After(s.now().Add(5*time.Second)) && s.now().Sub(in.SentAt) <= 15*time.Minute {
					var actor, conversation int
					if err = tx.QueryRow("SELECT COALESCE(sum(json_extract(body,'$.actor')=?),0),COALESCE(sum(json_extract(body,'$.conversation')=?),0) FROM provider_inbox WHERE account=? AND state='pending'", in.Actor, in.Conversation, page.Account).Scan(&actor, &conversation); err != nil {
						return err
					}
					if pending < 1024 && actor < 16 && conversation < 128 {
						pending++
						raw, _ := json.Marshal(in)
						body = string(raw)
						state = "pending"
					}
				}
			}
			if err = s.capacity(tx, "", len(body)+2*protocol.MaxRequest, false); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,?,?,?,?)", page.Account, event.ID, event.Hash, body, state); err != nil {
				return err
			}
		}
		if expectedNext != page.Next {
			return fmt.Errorf("intake cursor does not match page")
		}
		_, err = tx.Exec("INSERT INTO telegram_cursors VALUES(?,?) ON CONFLICT(account) DO UPDATE SET offset=excluded.offset", page.Account, page.Next)
		return err
	})
}

func (s *Store) RunTelegramIntake(ctx context.Context, p provider.TelegramPoller) error {
	if s.verificationOnly {
		return fmt.Errorf("Telegram intake unavailable in verification mode")
	}
	if err := p.Preflight(ctx); err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		offset, err := s.TelegramOffset(ctx, p.Profile.Account)
		if err != nil {
			return err
		}
		page, err := p.Poll(ctx, offset)
		if err != nil {
			return err
		}
		if err = s.PersistTelegram(ctx, page); err != nil {
			return err
		}
	}
}

// One supervised intake attempt. Preflight is read-only; failure never changes
// webhook configuration or acknowledges unpersisted updates.
func (s *Store) PullTelegram(ctx context.Context, p provider.TelegramPoller) error {
	if s.verificationOnly {
		return fmt.Errorf("Telegram intake unavailable in verification mode")
	}
	if err := p.Preflight(ctx); err != nil {
		return err
	}
	offset, err := s.TelegramOffset(ctx, p.Profile.Account)
	if err != nil {
		return err
	}
	page, err := p.Poll(ctx, offset)
	if err != nil {
		return err
	}
	return s.PersistTelegram(ctx, page)
}

func (s *Store) ConvertIntake(ctx context.Context) error {
	type pending struct {
		account string
		id      int64
		in      interaction.Inbound
	}
	var items []pending
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT account,id,body FROM provider_inbox WHERE state='pending' ORDER BY attempts,rowid LIMIT 32")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p pending
			var body string
			if err = rows.Scan(&p.account, &p.id, &body); err != nil {
				return err
			}
			if err = protocol.Decode([]byte(body), &p.in, 8192); err != nil {
				return err
			}
			items = append(items, p)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, p := range items {
		handled, admitErr := s.AdmitGatewayQuery(ctx, p.in)
		if !handled {
			_, admitErr = s.AdmitCommand(ctx, p.in)
		}
		var rejection *interaction.Rejection
		rejected := errors.As(admitErr, &rejection)
		if rejected {
			if err = s.recordCommandRejection(ctx, p.in, rejection); err != nil {
				return err
			}
		}
		err = s.transaction(ctx, func(tx *sql.Tx) error {
			state := "pending"
			if admitErr == nil {
				state = "converted"
			} else if rejected {
				state = "rejected"
			} else if s.clockSafe() && s.now().Sub(p.in.SentAt) > 15*time.Minute {
				state = "rejected"
			}
			_, err := tx.Exec("UPDATE provider_inbox SET state=?,attempts=attempts+1 WHERE account=? AND id=?", state, p.account, p.id)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
