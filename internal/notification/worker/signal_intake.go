package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func (s *Store) signalCursor(ctx context.Context, p provider.SignalAccount) (cursor interaction.SpoolCursor, err error) {
	cursor = interaction.SpoolCursor{Identity: p.InboxIdentity, Account: p.Account}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := readSignalRecoveryCursor(tx, p.Account); err != nil {
			return err
		}
		var body string
		err := tx.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", p.Account).Scan(&body)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if err = protocol.Decode([]byte(body), &cursor, 4096); err != nil {
			return err
		}
		if cursor.Identity != p.InboxIdentity || cursor.Account != p.Account {
			return fmt.Errorf("Signal relay identity changed; reconciliation required")
		}
		return nil
	})
	return
}

func (s *Store) PersistSignal(ctx context.Context, p provider.SignalAccount, page interaction.SpoolPage) error {
	if p.Validate() != nil || page.Cursor.Identity != p.InboxIdentity || page.Cursor.Account != p.Account || len(page.Events) > 16 || s.config.Interaction == nil {
		return fmt.Errorf("invalid Signal relay page")
	}
	known := false
	for _, d := range s.config.Interaction.Destinations {
		if d.Provider == "signal" && d.Account == p.Account {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("Signal account not enrolled")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		recovered, err := readSignalRecoveryCursor(tx, p.Account)
		if err != nil {
			return err
		}
		cursor := interaction.SpoolCursor{Identity: p.InboxIdentity, Account: p.Account}
		var body string
		err = tx.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", p.Account).Scan(&body)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			if err = protocol.Decode([]byte(body), &cursor, 4096); err != nil {
				return err
			}
		}
		if cursor.Identity != p.InboxIdentity || cursor.Account != p.Account {
			return fmt.Errorf("Signal restore reconciliation required")
		}
		for _, event := range page.Events {
			if recovered != nil && event.Position <= recovered.Handoff.Target.Position {
				return fmt.Errorf("Signal event predates recovery baseline")
			}
			if !protocol.Sequence(event.Position) || !protocol.ValidToken(event.Key) || !safefs.DigestPattern.MatchString(event.Hash) {
				return fmt.Errorf("invalid Signal inbox record")
			}
			var prior string
			err := tx.QueryRow("SELECT hash FROM provider_inbox WHERE account=? AND id=?", p.Account, event.Position).Scan(&prior)
			if err == nil {
				if prior != event.Hash {
					return fmt.Errorf("Signal relay history conflict")
				}
				if event.Position > cursor.Position {
					return fmt.Errorf("Signal cursor behind stored history")
				}
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
			if event.Position != cursor.Position+1 {
				return fmt.Errorf("Signal inbox position gap")
			}
			state := "rejected"
			body := ""
			if in := event.Inbound; in != nil {
				if in.Provider != "signal" || in.Account != p.Account || in.NativeID != event.Key || len(in.Text) > 4096 {
					return fmt.Errorf("Signal request identity mismatch")
				}
				authorized := false
				for _, d := range s.config.Interaction.Destinations {
					if d.Provider == "signal" && d.Account == p.Account && d.ID == in.Conversation && d.Fingerprint() == in.DestinationFingerprint {
						for _, permission := range s.config.Interaction.Permissions {
							if permission.Actor == in.Actor && permission.Conversation == d.ID {
								authorized = true
							}
						}
					}
				}
				if authorized && !in.SentAt.IsZero() && !in.SentAt.After(s.now().Add(5*time.Second)) && s.now().Sub(in.SentAt) <= 15*time.Minute {
					var total, actor, conversation int
					if err = tx.QueryRow("SELECT count(*),COALESCE(sum(account=? AND json_extract(body,'$.actor')=?),0),COALESCE(sum(account=? AND json_extract(body,'$.conversation')=?),0) FROM provider_inbox WHERE state='pending'", p.Account, in.Actor, p.Account, in.Conversation).Scan(&total, &actor, &conversation); err != nil {
						return err
					}
					if total < 1024 && actor < 16 && conversation < 128 {
						raw, _ := json.Marshal(in)
						body = string(raw)
						state = "pending"
					}
				}
			}
			if err = s.capacity(tx, "", len(body)+2*protocol.MaxRequest, false); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,?,?,?,?)", p.Account, event.Position, event.Hash, body, state); err != nil {
				return err
			}
			cursor.Position = event.Position
		}
		if cursor != page.Cursor {
			return fmt.Errorf("Signal page cursor mismatch")
		}
		raw, _ := json.Marshal(cursor)
		_, err = tx.Exec("INSERT INTO signal_imports VALUES(?,?) ON CONFLICT(account) DO UPDATE SET cursor=excluded.cursor", p.Account, string(raw))
		return err
	})
}

// PullSignal acknowledges the relay only after the gateway's inbox and cursor
// have committed. A lost ACK can be repeated using the retained relay identity.
func (s *Store) PullSignal(ctx context.Context, c provider.SignalInboxClient) error {
	if s.verificationOnly {
		return fmt.Errorf("Signal intake unavailable in verification mode")
	}
	cursor, err := s.signalCursor(ctx, c.Profile)
	if err != nil {
		return err
	}
	page, err := c.List(ctx, cursor)
	if err != nil {
		return err
	}
	if err = s.PersistSignal(ctx, c.Profile, page); err != nil {
		return err
	}
	return c.Ack(ctx, page.Cursor)
}
