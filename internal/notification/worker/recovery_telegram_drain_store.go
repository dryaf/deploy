package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func telegramDrainFile(account string) string {
	return "telegram-drain-" + protocol.Hash(account) + ".json"
}
func initialTelegramDrain(binding recoveryBinding, account, history string) telegramDrainState {
	s := telegramDrainState{Binding: binding, Account: account, HistoryHash: history}
	s.Head = protocol.Hash(s)
	return s
}
func (r *recoveryReconciler) openTelegramDrain(ctx context.Context, account string) (s telegramDrainState, err error) {
	known := false
	if r.config.Interaction != nil {
		for _, d := range r.config.Interaction.Destinations {
			if d.Provider == "telegram" && d.Account == account {
				known = true
			}
		}
	}
	if !known {
		return s, fmt.Errorf("Telegram recovery account not enrolled")
	}
	var history telegramRecoverySession
	err = r.transaction(ctx, func(tx *sql.Tx) error { var e error; history, e = readTelegramSession(tx, account); return e })
	if err != nil {
		return
	}
	if history.Summary == nil {
		return s, fmt.Errorf("complete retained Telegram history reconciliation before polling")
	}
	completed, err := r.finishTelegramRecovery(ctx, history.Manifest, nil)
	if err != nil {
		return s, err
	}
	seed := initialTelegramDrain(r.journal.binding(), account, protocol.Hash(completed))
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var version string
		err := tx.QueryRow("SELECT value FROM metadata WHERE key='telegram_drain_schema'").Scan(&version)
		if err == nil {
			if version != "1" {
				return fmt.Errorf("unknown Telegram drain schema")
			}
			for _, table := range []string{"reconciliation_telegram_drains", "reconciliation_telegram_drain_pages"} {
				var n int
				if err = tx.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
					return err
				}
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_telegram_drains(account TEXT PRIMARY KEY,body TEXT NOT NULL);
CREATE TABLE reconciliation_telegram_drain_pages(account TEXT NOT NULL,ordinal INTEGER NOT NULL,previous TEXT NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(account,ordinal));
INSERT INTO metadata VALUES('telegram_drain_schema','1');`)
		return err
	})
	if err != nil {
		return
	}
	var pin telegramDrainState
	pinErr := r.stage.ReadJSON(telegramDrainFile(account), &pin)
	if pinErr != nil && !os.IsNotExist(pinErr) {
		return s, pinErr
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		s, err = readTelegramDrain(tx, account)
		if err == sql.ErrNoRows {
			var orphan int
			if err = tx.QueryRow("SELECT count(*) FROM reconciliation_telegram_drain_pages WHERE account=?", account).Scan(&orphan); err != nil {
				return err
			}
			if orphan != 0 {
				return fmt.Errorf("Telegram drain lost its state")
			}
			if pinErr == nil && protocol.Hash(pin) != protocol.Hash(seed) {
				return fmt.Errorf("Telegram drain database is behind acknowledgement journal")
			}
			if os.IsNotExist(pinErr) {
				if err = r.stage.WriteJSON(telegramDrainFile(account), seed, false); err != nil {
					return err
				}
				pin = seed
				pinErr = nil
			}
			s = seed
			return writeTelegramDrain(tx, s)
		}
		if err != nil {
			return err
		}
		if pinErr != nil {
			return fmt.Errorf("Telegram drain acknowledgement journal missing")
		}
		if s.Binding != seed.Binding || s.Account != account || s.HistoryHash != seed.HistoryHash {
			return fmt.Errorf("Telegram drain authority changed")
		}
		if s.Pages < pin.Pages || s.Pages-pin.Pages > 1 {
			return fmt.Errorf("Telegram drain acknowledgement journal disagrees")
		}
		verified, prefix, err := r.verifyTelegramDrain(tx, s, pin.Pages)
		if err != nil {
			return err
		}
		if protocol.Hash(verified) != protocol.Hash(s) || protocol.Hash(prefix) != protocol.Hash(pin) {
			return fmt.Errorf("Telegram drain history differs from acknowledgement journal")
		}
		return nil
	})
	if err != nil {
		return
	}
	// A committed page whose file publication was interrupted is safe to publish.
	// A database behind this file is never allowed to issue a provider ACK.
	if protocol.Hash(pin) != protocol.Hash(s) {
		err = r.stage.WriteJSON(telegramDrainFile(account), s, true)
	}
	return
}

func (r *recoveryReconciler) quarantineTelegramReplay(tx *sql.Tx, account string, next int64, e provider.IntakeEvent) (v telegramDrainEvent, err error) {
	check := telegramRecoveryRecord{ID: e.ID, Hash: e.Hash, State: "pending", Inbound: e.Inbound}
	if e.Inbound == nil {
		check.State = "rejected"
	}
	if err = check.validate(account, next); err != nil {
		return v, err
	}
	var hash, body string
	err = tx.QueryRow("SELECT hash,body FROM provider_inbox WHERE account=? AND id=?", account, e.ID).Scan(&hash, &body)
	if err == nil {
		if hash != e.Hash {
			return v, fmt.Errorf("Telegram replay conflicts with retained provider hash")
		}
		if body != "" {
			var old interaction.Inbound
			if err = protocol.Decode([]byte(body), &old, 8192); err != nil {
				return v, err
			}
			if e.Inbound == nil || protocol.Hash(old) != protocol.Hash(e.Inbound) {
				return v, fmt.Errorf("Telegram replay changed original request")
			}
		} else {
			e.Inbound = nil
			e.Rejection = "retained_rejection"
		}
	} else if err == sql.ErrNoRows {
		if e.Inbound != nil {
			allowed, destination := false, false
			for _, p := range r.config.Interaction.Permissions {
				if p.Actor == e.Inbound.Actor && p.Conversation == e.Inbound.Conversation {
					allowed = true
				}
			}
			for _, d := range r.config.Interaction.Destinations {
				if d.Provider == "telegram" && d.Account == account && d.ID == e.Inbound.Conversation && d.Fingerprint() == e.Inbound.DestinationFingerprint {
					destination = true
				}
			}
			if !allowed || !destination {
				e.Inbound = nil
				e.Rejection = "recovery_not_enrolled"
			} else {
				raw, _ := json.Marshal(e.Inbound)
				body = string(raw)
			}
		}
		if _, err = tx.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,?,?,?,'recovery_quarantined')", account, e.ID, e.Hash, body); err != nil {
			return v, err
		}
	} else {
		return v, err
	}
	if _, err = tx.Exec("UPDATE provider_inbox SET state='recovery_quarantined' WHERE account=? AND id=?", account, e.ID); err != nil {
		return v, err
	}
	v = telegramDrainEvent{Event: e, Disposition: "rejected"}
	if e.Inbound != nil {
		v.Disposition = "unmapped"
		m, err := readRetainedRequest(tx, *e.Inbound)
		if err != nil {
			return v, err
		}
		if m != nil {
			v.CommandID = m.ID
			v.Disposition = "mapped"
			if m.Query != nil && m.Query.Kind == "rejected" {
				v.Disposition = "rejected"
			}
		}
	}
	return v, nil
}

func (r *recoveryReconciler) verifyTelegramDrain(tx *sql.Tx, want telegramDrainState, prefixPages uint64) (state, prefix telegramDrainState, err error) {
	state = initialTelegramDrain(want.Binding, want.Account, want.HistoryHash)
	prefix = state
	if want.Pages > 4096 || prefixPages > want.Pages {
		return state, prefix, fmt.Errorf("invalid Telegram drain page count")
	}
	rows, err := tx.Query("SELECT ordinal,previous,hash,body FROM reconciliation_telegram_drain_pages WHERE account=? ORDER BY ordinal", want.Account)
	if err != nil {
		return
	}
	defer rows.Close()
	summary := TelegramDrainSummary{Operation: want.Binding.Operation, Identity: want.Binding.After, Account: want.Account, HistoryHash: want.HistoryHash}
	for rows.Next() {
		var ordinal uint64
		var previous, hash, body string
		var p telegramDrainPage
		if err = rows.Scan(&ordinal, &previous, &hash, &body); err != nil {
			return
		}
		if err = protocol.Decode([]byte(body), &p, protocol.MaxRequest); err != nil {
			return
		}
		if state.Summary != nil || ordinal != state.Pages+1 || ordinal > want.Pages || previous != state.Head || hash != telegramDrainHead(previous, p) ||
			len(p.Events) > 16 || p.Offset < 0 || (p.Offset != 0 && p.Offset != state.Offset) || p.ObservedAt.IsZero() ||
			!protocol.ValidToken(p.BootID) || p.Uptime < 0 || math.IsNaN(p.Uptime) || math.IsInf(p.Uptime, 0) ||
			!safefs.DigestPattern.MatchString(p.ReceivedHash) {
			err = fmt.Errorf("Telegram drain page chain damaged")
			return
		}
		expected := p.Offset
		for _, v := range p.Events {
			e := v.Event
			if e.ID < expected || e.ID >= 1<<63-1 {
				err = fmt.Errorf("Telegram drain update order damaged")
				return
			}
			expected = e.ID + 1
			check := telegramRecoveryRecord{ID: e.ID, Hash: e.Hash, Inbound: e.Inbound, State: "pending"}
			if e.Inbound == nil {
				check.State = "rejected"
			}
			if err = check.validate(want.Account, p.Next); err != nil {
				return
			}
			var stored, raw, inboxState string
			if err = tx.QueryRow("SELECT hash,body,state FROM provider_inbox WHERE account=? AND id=?", want.Account, e.ID).Scan(&stored, &raw, &inboxState); err != nil {
				return
			}
			if stored != e.Hash || inboxState != "recovery_quarantined" {
				err = fmt.Errorf("Telegram drain lost provider evidence")
				return
			}
			if e.Inbound == nil {
				if raw != "" {
					err = fmt.Errorf("Telegram drain rejection changed")
					return
				}
			} else {
				var in interaction.Inbound
				if err = protocol.Decode([]byte(raw), &in, 8192); err != nil {
					return
				}
				if protocol.Hash(in) != protocol.Hash(e.Inbound) {
					err = fmt.Errorf("Telegram drain request changed")
					return
				}
			}
			summary.ObservedEvents++
			switch v.Disposition {
			case "mapped":
				summary.Mapped++
			case "unmapped":
				summary.Unmapped++
			case "rejected":
				summary.Rejected++
			default:
				err = fmt.Errorf("Telegram drain disposition damaged")
				return
			}
		}
		if p.Next != expected {
			err = fmt.Errorf("Telegram drain offset lacks evidence")
			return
		}
		state = advanceTelegramDrain(state, p)
		if len(p.Events) == 0 {
			summary.Pages = state.Pages
			summary.NextOffset = state.Offset
			summary.EvidenceDigest = state.Head
			summary.ObservedAt = p.ObservedAt
			summary.BootID = p.BootID
			summary.Uptime = p.Uptime
			state.Summary = &summary
		}
		if state.Pages == prefixPages {
			prefix = state
		}
	}
	if err = rows.Err(); err != nil {
		return
	}
	if state.Pages != want.Pages {
		err = fmt.Errorf("Telegram drain history missing")
	}
	return
}
