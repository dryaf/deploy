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

type signalRecoverySession struct {
	Challenge      interaction.SignalRecoveryChallenge
	Manifest       *interaction.SignalRecoveryManifest
	Summary        *interaction.SignalRecoverySummary
	GatewayHighest uint64
}

// OpenSignalRecoverySession opens only the quarantined working copy. It neither
// polls a provider nor acknowledges a relay or reopens ordinary command intake.
func OpenSignalRecoverySession(ctx context.Context, path string, c Config) (interaction.SignalRecoverySession, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	if err = r.initializeSignalRecovery(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func (r *recoveryReconciler) initializeSignalRecovery(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		var version string
		err := tx.QueryRow("SELECT value FROM metadata WHERE key='signal_reconciliation_schema'").Scan(&version)
		if err == nil {
			if version != "1" {
				return fmt.Errorf("unknown Signal reconciliation schema")
			}
			for _, table := range []string{"reconciliation_signal_sessions", "reconciliation_signal_events"} {
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
		_, err = tx.Exec(`CREATE TABLE reconciliation_signal_sessions(account TEXT PRIMARY KEY,body TEXT NOT NULL);
CREATE TABLE reconciliation_signal_events(account TEXT NOT NULL,position INTEGER NOT NULL,key TEXT NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,request_key TEXT NOT NULL,command_id TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(account,position),UNIQUE(account,key));
INSERT INTO metadata VALUES('signal_reconciliation_schema','1');`)
		return err
	})
}
func (r *recoveryReconciler) signalRecoveryEnrollment(account string) error {
	if provider.ValidSignalAccount(account) && r.config.Interaction != nil {
		for _, d := range r.config.Interaction.Destinations {
			if d.Provider == "signal" && d.Account == account {
				return nil
			}
		}
	}
	return fmt.Errorf("Signal recovery account not enrolled")
}
func readSignalRecovery(tx *sql.Tx, account string) (s signalRecoverySession, err error) {
	var body string
	if err = tx.QueryRow("SELECT body FROM reconciliation_signal_sessions WHERE account=?", account).Scan(&body); err != nil {
		return
	}
	err = protocol.Decode([]byte(body), &s, 16384)
	return
}
func writeSignalRecovery(tx *sql.Tx, s signalRecoverySession) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO reconciliation_signal_sessions VALUES(?,?) ON CONFLICT(account) DO UPDATE SET body=excluded.body", s.Challenge.Relay.Account, string(raw))
	return err
}
func (r *recoveryReconciler) SignalChallenge(ctx context.Context, c interaction.SpoolCursor) (ch interaction.SignalRecoveryChallenge, err error) {
	if err = r.signalRecoveryEnrollment(c.Account); err != nil {
		return
	}
	if !c.Identity.Valid() || c.Position != 0 {
		return ch, fmt.Errorf("invalid Signal relay recovery pin")
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", c.Account).Scan(&raw)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			var old interaction.SpoolCursor
			if err = protocol.Decode([]byte(raw), &old, 4096); err != nil {
				return err
			}
			old.Position = 0
			if old != c {
				return fmt.Errorf("Signal recovery relay differs from restored pin")
			}
		}
		s, err := readSignalRecovery(tx, c.Account)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if s.Summary != nil {
			return fmt.Errorf("Signal recovery completed; inspect summary")
		}
		if s.Challenge.Nonce != "" && s.Challenge.Relay != c {
			return fmt.Errorf("Signal recovery relay changed")
		}
		boot, up, err := r.recoveryClock()
		if err != nil {
			return err
		}
		now := r.now().UTC()
		if s.Challenge.Nonce != "" {
			if now.Before(s.Challenge.IssuedAt) || (boot == s.Challenge.BootID && up < s.Challenge.Uptime) {
				return fmt.Errorf("Signal recovery clock moved backwards")
			}
			if s.Challenge.Validate(now, boot, up) == nil {
				ch = s.Challenge
				return nil
			}
		}
		nonce, err := safefs.NewID("n")
		if err != nil {
			return err
		}
		ch = interaction.SignalRecoveryChallenge{Operation: r.journal.Operation, Gateway: r.journal.After, Relay: c, Nonce: nonce, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute), BootID: boot, Uptime: up}
		s.Challenge = ch
		return writeSignalRecovery(tx, s)
	})
	return
}
func (r *recoveryReconciler) checkSignalRecovery(tx *sql.Tx, ch interaction.SignalRecoveryChallenge) (s signalRecoverySession, err error) {
	if err = r.signalRecoveryEnrollment(ch.Relay.Account); err != nil {
		return
	}
	if ch.Operation != r.journal.Operation || ch.Gateway != r.journal.After {
		return s, fmt.Errorf("Signal recovery authority mismatch")
	}
	if s, err = readSignalRecovery(tx, ch.Relay.Account); err != nil {
		return
	}
	if protocol.Hash(ch) != protocol.Hash(s.Challenge) {
		return s, fmt.Errorf("Signal recovery challenge changed")
	}
	boot, up, err := r.recoveryClock()
	if err != nil {
		return
	}
	err = ch.Validate(r.now(), boot, up)
	return
}
func (r *recoveryReconciler) BeginSignal(ctx context.Context, ch interaction.SignalRecoveryChallenge, m interaction.SignalRecoveryManifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Relay != ch.Relay {
		return fmt.Errorf("Signal recovery manifest relay mismatch")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := r.checkSignalRecovery(tx, ch)
		if err != nil {
			return err
		}
		if s.Summary != nil {
			return fmt.Errorf("Signal recovery completed")
		}
		if s.Manifest != nil && !s.Manifest.Equal(m) {
			return fmt.Errorf("Signal recovery manifest changed")
		}
		var raw string
		var gatewayHighest uint64
		err = tx.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", m.Relay.Account).Scan(&raw)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			var c interaction.SpoolCursor
			if err = protocol.Decode([]byte(raw), &c, 4096); err != nil {
				return err
			}
			if c.Identity != m.Relay.Identity || c.Account != m.Relay.Account || (c.Position > m.Highest && m.Restore.Operation == "") {
				return fmt.Errorf("relay recovery is behind or differs from restored history")
			}
			gatewayHighest = c.Position
		}
		var latest uint64
		if err = tx.QueryRow("SELECT COALESCE(max(id),0) FROM provider_inbox WHERE account=?", m.Relay.Account).Scan(&latest); err != nil {
			return err
		}
		if latest > m.Highest && m.Restore.Operation == "" {
			return fmt.Errorf("relay recovery lost restored provider events")
		}
		if m.Restore.Operation != "" && s.Manifest == nil {
			gatewayHighest = max(gatewayHighest, latest)
			// Preserve the pre-import watermark. Replayed source pages can add
			// provider rows before a lost response causes Begin to run again.
			s.GatewayHighest = gatewayHighest
		}
		s.Manifest = &m
		return writeSignalRecovery(tx, s)
	})
}
func (r *recoveryReconciler) ImportSignal(ctx context.Context, ch interaction.SignalRecoveryChallenge, after uint64, events []interaction.SpoolEvent) error {
	raw, err := json.Marshal(events)
	if err != nil || len(events) == 0 || len(events) > 128 || len(raw)+4096 > protocol.MaxRequest {
		return fmt.Errorf("Signal recovery page exceeds bounds")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := r.checkSignalRecovery(tx, ch)
		if err != nil {
			return err
		}
		if s.Manifest == nil || s.Summary != nil || after > s.Manifest.EventCount() || uint64(len(events)) > s.Manifest.EventCount()-after {
			return fmt.Errorf("Signal recovery page outside manifest")
		}
		if err = r.capacity(tx, len(raw)); err != nil {
			return err
		}
		for i, e := range events {
			if e.Position != s.Manifest.PositionAt(after+uint64(i)) {
				return fmt.Errorf("Signal recovery page position gap")
			}
			if err = r.importRecoverySignalEvent(tx, ch, e); err != nil {
				return err
			}
		}
		return nil
	})
}
func (r *recoveryReconciler) importRecoverySignalEvent(tx *sql.Tx, ch interaction.SignalRecoveryChallenge, e interaction.SpoolEvent) error {
	if err := interaction.ValidateSignalRecoveryEvent(e, ch.Relay); err != nil {
		return err
	}
	account := ch.Relay.Account
	var hash string
	err := tx.QueryRow("SELECT hash FROM reconciliation_signal_events WHERE account=? AND position=?", account, e.Position).Scan(&hash)
	if err == nil {
		if hash != protocol.Hash(e) {
			return fmt.Errorf("Signal recovery evidence replay changed")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	var oldBody string
	err = tx.QueryRow("SELECT hash,body FROM provider_inbox WHERE account=? AND id=?", account, e.Position).Scan(&hash, &oldBody)
	if err == nil {
		if hash != e.Hash {
			return fmt.Errorf("restored Signal event conflicts with relay history")
		}
		if oldBody != "" {
			var in interaction.Inbound
			if err = protocol.Decode([]byte(oldBody), &in, 16384); err != nil {
				return err
			}
			if e.Inbound == nil || protocol.Hash(in) != protocol.Hash(*e.Inbound) {
				return fmt.Errorf("restored Signal request conflicts with relay history")
			}
		}
	} else if err == sql.ErrNoRows {
		body := ""
		if e.Inbound != nil {
			raw, _ := json.Marshal(e.Inbound)
			body = string(raw)
		}
		if _, err = tx.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,?,?,?,'recovery_quarantined')", account, e.Position, e.Hash, body); err != nil {
			return err
		}
	} else {
		return err
	}
	if _, err = tx.Exec("UPDATE provider_inbox SET state='recovery_quarantined' WHERE account=? AND id=?", account, e.Position); err != nil {
		return err
	}
	key, id, state := "", "", "rejected"
	if e.Inbound != nil {
		key = e.Inbound.RequestKey()
		state = "unmapped"
		mapping, err := readRetainedRequest(tx, *e.Inbound)
		if err != nil {
			return err
		}
		if mapping != nil {
			id, state = mapping.ID, "mapped"
			if mapping.Query != nil && mapping.Query.Kind == "rejected" {
				state = "rejected"
			}
		}
	}
	// Never call admission/routing here: an unmapped replay has no fresh identity,
	// deadline, authorization or target, even if its provider timestamp is recent.
	body, _ := json.Marshal(e)
	_, err = tx.Exec("INSERT INTO reconciliation_signal_events VALUES(?,?,?,?,?,?,?,?)", account, e.Position, e.Key, protocol.Hash(e), string(body), key, id, state)
	return err
}
func (r *recoveryReconciler) FinishSignal(ctx context.Context, ch interaction.SignalRecoveryChallenge) (summary interaction.SignalRecoverySummary, err error) {
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := r.checkSignalRecovery(tx, ch)
		if err != nil {
			return err
		}
		if s.Summary != nil {
			if err = r.validateSignalSummary(s); err != nil {
				return err
			}
			summary = *s.Summary
			return nil
		}
		if s.Manifest == nil {
			return fmt.Errorf("Signal recovery manifest missing")
		}
		rows, err := tx.Query("SELECT position,hash,body,state FROM reconciliation_signal_events WHERE account=? ORDER BY position", ch.Relay.Account)
		if err != nil {
			return err
		}
		defer rows.Close()
		summary = interaction.SignalRecoverySummary{Challenge: ch, Manifest: *s.Manifest}
		summary.GatewayHighest = s.GatewayHighest
		if s.Manifest.Restore.Operation != "" && s.GatewayHighest > s.Manifest.Highest {
			summary.MissingRelayFrom = s.Manifest.Highest + 1
			summary.MissingRelayThrough = s.GatewayHighest
		}
		var hashes []string
		for rows.Next() {
			var position uint64
			var hash, body, state string
			var e interaction.SpoolEvent
			if err = rows.Scan(&position, &hash, &body, &state); err != nil {
				return err
			}
			if err = protocol.Decode([]byte(body), &e, 16384); err != nil {
				return err
			}
			if position != s.Manifest.PositionAt(uint64(len(hashes))) || e.Position != position || hash != protocol.Hash(e) {
				return fmt.Errorf("Signal recovery history damaged")
			}
			hashes = append(hashes, hash)
			switch state {
			case "mapped":
				summary.Mapped++
			case "unmapped":
				summary.Unmapped++
			case "rejected":
				summary.Rejected++
			default:
				return fmt.Errorf("invalid Signal recovery disposition")
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if uint64(len(hashes)) != s.Manifest.EventCount() || interaction.RecoveryEvidenceDigest(hashes) != s.Manifest.Digest {
			return fmt.Errorf("Signal recovery history incomplete")
		}
		if err = summary.Validate(); err != nil {
			return err
		}
		s.Summary = &summary
		return writeSignalRecovery(tx, s)
	})
	return
}
func (r *recoveryReconciler) SignalSummary(ctx context.Context, account string) (summary interaction.SignalRecoverySummary, err error) {
	if err = r.signalRecoveryEnrollment(account); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := readSignalRecovery(tx, account)
		if err == sql.ErrNoRows {
			return protocol.ErrRecoveryIncomplete
		}
		if err != nil {
			return err
		}
		if s.Summary == nil {
			return protocol.ErrRecoveryIncomplete
		}
		if err = r.validateSignalSummary(s); err != nil {
			return err
		}
		summary = *s.Summary
		return nil
	})
	return
}

func (r *recoveryReconciler) validateSignalSummary(s signalRecoverySession) error {
	if s.Summary == nil || s.Manifest == nil || !s.Summary.Manifest.Equal(*s.Manifest) ||
		protocol.Hash(s.Summary.Challenge) != protocol.Hash(s.Challenge) ||
		s.Summary.GatewayHighest != s.GatewayHighest ||
		s.Challenge.Operation != r.journal.Operation || s.Challenge.Gateway != r.journal.After {
		return fmt.Errorf("stored Signal recovery completion changed")
	}
	return s.Summary.Validate()
}
