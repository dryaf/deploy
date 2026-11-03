package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// RecoveryInbox exclusively owns an offline relay inbox. It exposes no receiving,
// sending, listing/ACK or identity rotation operations. The caller drains the
// relay first; opening fails while its normal instance lease is held.
type RecoveryInbox struct {
	inbox *Inbox
	fault func(string) error
}
type recoveryExport struct {
	Challenge interaction.SignalRecoveryChallenge
	Manifest  interaction.SignalRecoveryManifest
	Summary   *interaction.SignalRecoverySummary
	Handoff   *interaction.SignalRecoveryHandoff
	Cursor    *interaction.SignalRecoveryCursor
}

func OpenRecoveryInbox(path, account string) (*RecoveryInbox, error) {
	inbox, err := OpenInbox(path, account, false)
	if err != nil {
		return nil, err
	}
	r := &RecoveryInbox{inbox: inbox}
	err = inbox.transaction(context.Background(), func(tx *sql.Tx) error {
		var tables int
		if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('recovery_exports','recovery_records')").Scan(&tables); err != nil {
			return err
		}
		if tables == 2 {
			var version int
			if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				return err
			}
			if version != 2 && version != runtimeInboxSchema {
				return fmt.Errorf("unowned relay recovery schema")
			}
			return nil
		}
		if tables != 0 {
			return fmt.Errorf("relay recovery export schema incomplete")
		}
		_, err := tx.Exec(`CREATE TABLE recovery_exports(operation TEXT PRIMARY KEY,body TEXT NOT NULL);
CREATE TABLE recovery_records(operation TEXT NOT NULL,position INTEGER NOT NULL,hash TEXT NOT NULL,PRIMARY KEY(operation,position));`)
		if err == nil && inbox.baseline == nil {
			_, err = tx.Exec("PRAGMA user_version=2")
		}
		return err
	})
	if err != nil {
		inbox.Close()
		return nil, err
	}
	return r, nil
}
func (r *RecoveryInbox) Close() error { return r.inbox.Close() }
func (r *RecoveryInbox) Cursor() interaction.SpoolCursor {
	c := r.inbox.Cursor()
	c.Position = 0
	return c
}
func (r *RecoveryInbox) validate(ch interaction.SignalRecoveryChallenge) error {
	if ch.Relay != r.Cursor() {
		return fmt.Errorf("Signal recovery relay pin changed")
	}
	// The gateway owns the monotonic lease clock. A relay on another host cannot
	// compare its own boot/uptime to that clock; it checks structure and wall time.
	// Begin/Import/Finish on the gateway enforce the actual boot and uptime.
	return ch.Validate(time.Now().UTC(), ch.BootID, ch.Uptime)
}
func (r *RecoveryInbox) boundary(stage string) error {
	if r.fault != nil {
		return r.fault(stage)
	}
	return nil
}
func readRecoveryExport(tx *sql.Tx, op string) (saved recoveryExport, err error) {
	var body string
	if err = tx.QueryRow("SELECT body FROM recovery_exports WHERE operation=?", op).Scan(&body); err != nil {
		return
	}
	err = protocol.Decode([]byte(body), &saved, 16384)
	return
}
func saveRecoveryExport(tx *sql.Tx, saved recoveryExport) error {
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO recovery_exports VALUES(?,?) ON CONFLICT(operation) DO UPDATE SET body=excluded.body", saved.Challenge.Operation, string(raw))
	return err
}
func verifyRecoveryExport(tx *sql.Tx, saved recoveryExport) error {
	if err := saved.Manifest.Validate(); err != nil {
		return err
	}
	var count, highest uint64
	if err := tx.QueryRow("SELECT count(*),COALESCE(max(position),0) FROM recovery_records WHERE operation=?", saved.Challenge.Operation).Scan(&count, &highest); err != nil {
		return err
	}
	if count != saved.Manifest.EventCount() || (count > 0 && highest != saved.Manifest.PositionAt(count-1)) || (count == 0 && highest != 0) {
		return fmt.Errorf("relay recovery evidence incomplete")
	}
	return nil
}
func (r *RecoveryInbox) Capture(ctx context.Context, ch interaction.SignalRecoveryChallenge) (m interaction.SignalRecoveryManifest, err error) {
	if err = r.validate(ch); err != nil {
		return
	}
	err = r.inbox.transaction(ctx, func(tx *sql.Tx) error {
		saved, err := readRecoveryExport(tx, ch.Operation)
		if err == nil {
			if saved.Challenge.Gateway != ch.Gateway || saved.Challenge.Relay != ch.Relay || ch.IssuedAt.Before(saved.Challenge.IssuedAt) ||
				(ch.Nonce == saved.Challenge.Nonce && protocol.Hash(ch) != protocol.Hash(saved.Challenge)) {
				return fmt.Errorf("relay recovery challenge changed")
			}
			if saved.Summary != nil && protocol.Hash(ch) != protocol.Hash(saved.Challenge) {
				return fmt.Errorf("completed relay export cannot renew")
			}
			if err = verifyRecoveryExport(tx, saved); err != nil {
				return err
			}
			saved.Challenge = ch
			m = saved.Manifest
			return saveRecoveryExport(tx, saved)
		}
		if err != sql.ErrNoRows {
			return err
		}
		var unowned int
		if err = tx.QueryRow("SELECT count(*) FROM recovery_records WHERE operation=?", ch.Operation).Scan(&unowned); err != nil {
			return err
		}
		if unowned != 0 {
			return fmt.Errorf("unowned relay recovery records")
		}
		m.Relay = r.Cursor()
		if err = tx.QueryRow("SELECT acked,listed FROM identity WHERE singleton=1").Scan(&m.Acknowledged, &m.Listed); err != nil {
			return err
		}
		if err = tx.QueryRow("SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='events'),0)").Scan(&m.Highest); err != nil {
			return err
		}
		if r.inbox.baseline != nil {
			if err = addBaselineHistory(&m, *r.inbox.baseline); err != nil {
				return err
			}
		}
		if m.EventCount() > 65536 || m.Acknowledged > m.Listed || m.Listed > m.Highest {
			return fmt.Errorf("invalid relay recovery watermarks")
		}
		var pages, size int64
		if err = tx.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
			return err
		}
		if err = tx.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
			return err
		}
		reserve := int64(m.EventCount())*256 + (64 << 10)
		if pages*size+reserve > 32<<20 {
			return fmt.Errorf("relay recovery database budget exhausted")
		}
		if err = r.inbox.dir.Capacity(8<<20 + reserve*3); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT position,key,hash,body FROM events ORDER BY position")
		if err != nil {
			return err
		}
		defer rows.Close()
		var hashes []string
		for rows.Next() {
			var position uint64
			var key, hash, body string
			var e interaction.SpoolEvent
			if err = rows.Scan(&position, &key, &hash, &body); err != nil {
				return err
			}
			if err = protocol.Decode([]byte(body), &e, 16384); err != nil {
				return err
			}
			if position != m.PositionAt(uint64(len(hashes))) || e.Position != 0 || e.Key != key || e.Hash != hash {
				return fmt.Errorf("relay event identity or sequence damaged")
			}
			e.Position = position
			if err = interaction.ValidateSignalRecoveryEvent(e, m.Relay); err != nil {
				return err
			}
			hash = protocol.Hash(e)
			hashes = append(hashes, hash)
			if _, err = tx.Exec("INSERT INTO recovery_records VALUES(?,?,?)", ch.Operation, position, hash); err != nil {
				return err
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if uint64(len(hashes)) != m.EventCount() {
			return fmt.Errorf("relay history contains gaps")
		}
		m.Digest = interaction.RecoveryEvidenceDigest(hashes)
		if err = m.Validate(); err != nil {
			return err
		}
		if err = saveRecoveryExport(tx, recoveryExport{Challenge: ch, Manifest: m}); err != nil {
			return err
		}
		return r.boundary("relay_capture_transaction")
	})
	if err == nil {
		err = r.boundary("relay_capture_commit")
	}
	return
}
func (r *RecoveryInbox) Page(ctx context.Context, ch interaction.SignalRecoveryChallenge, after uint64) (page interaction.SignalRecoveryPage, err error) {
	if err = r.validate(ch); err != nil {
		return
	}
	err = r.inbox.transaction(ctx, func(tx *sql.Tx) error {
		saved, err := readRecoveryExport(tx, ch.Operation)
		if err != nil {
			return err
		}
		if protocol.Hash(saved.Challenge) != protocol.Hash(ch) || after > saved.Manifest.EventCount() {
			return fmt.Errorf("relay recovery page session mismatch")
		}
		if err = verifyRecoveryExport(tx, saved); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT r.position,r.hash,e.body FROM recovery_records r LEFT JOIN events e ON e.position=r.position WHERE r.operation=? ORDER BY r.position LIMIT 128 OFFSET ?", ch.Operation, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		page.Next = after
		size := 2
		for rows.Next() {
			var position uint64
			var hash, body string
			var e interaction.SpoolEvent
			if err = rows.Scan(&position, &hash, &body); err != nil {
				return err
			}
			if err = protocol.Decode([]byte(body), &e, 16384); err != nil {
				return err
			}
			if e.Position != 0 {
				return fmt.Errorf("stored relay position changed")
			}
			e.Position = position
			if position != saved.Manifest.PositionAt(page.Next) || protocol.Hash(e) != hash {
				return fmt.Errorf("relay recovery record damaged")
			}
			if err = interaction.ValidateSignalRecoveryEvent(e, saved.Manifest.Relay); err != nil {
				return err
			}
			raw, _ := json.Marshal(e)
			extra := len(raw)
			if len(page.Events) > 0 {
				extra++
			}
			if size+extra+4096 > protocol.MaxRequest {
				break
			}
			size += extra
			page.Events = append(page.Events, e)
			page.Next++
		}
		if err = rows.Err(); err != nil {
			return err
		}
		page.Done = page.Next == saved.Manifest.EventCount()
		if !page.Done && len(page.Events) == 0 {
			return fmt.Errorf("relay recovery page missing")
		}
		return nil
	})
	return
}
func (r *RecoveryInbox) Completed(ctx context.Context, ch interaction.SignalRecoveryChallenge) (done bool, err error) {
	err = r.inbox.transaction(ctx, func(tx *sql.Tx) error {
		s, err := readRecoveryExport(tx, ch.Operation)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if s.Challenge.Gateway != ch.Gateway || s.Challenge.Relay != ch.Relay {
			return fmt.Errorf("relay recovery authority changed")
		}
		done = s.Summary != nil
		return nil
	})
	return
}
func (r *RecoveryInbox) RecordSummary(ctx context.Context, s interaction.SignalRecoverySummary) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return r.inbox.transaction(ctx, func(tx *sql.Tx) error {
		if s.Manifest.Relay != r.Cursor() {
			return fmt.Errorf("relay recovery summary account mismatch")
		}
		saved, err := readRecoveryExport(tx, s.Challenge.Operation)
		if err != nil {
			return err
		}
		if err = verifyRecoveryExport(tx, saved); err != nil {
			return err
		}
		if protocol.Hash(saved.Challenge) != protocol.Hash(s.Challenge) || !saved.Manifest.Equal(s.Manifest) || s.IntakeEnabled ||
			s.Mapped > s.Manifest.EventCount() || s.Unmapped > s.Manifest.EventCount()-s.Mapped || s.Rejected != s.Manifest.EventCount()-s.Mapped-s.Unmapped {
			return fmt.Errorf("relay recovery summary mismatch")
		}
		if saved.Summary != nil && protocol.Hash(*saved.Summary) != protocol.Hash(s) {
			return fmt.Errorf("relay recovery completion changed")
		}
		saved.Summary = &s
		return saveRecoveryExport(tx, saved)
	})
}
