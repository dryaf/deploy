package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type commandRecoverySession struct {
	Challenge protocol.RecoveryChallenge
	Manifest  *interaction.CommandRecoveryManifest
	Summary   *interaction.CommandRecoverySummary
}

// This separate session leaves all live command execution and provider intake
// quarantined. Evidence classification is not authorization or an epoch commit.
func OpenCommandRecoverySession(ctx context.Context, path string, c Config) (interaction.CommandRecoverySession, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	if err = r.initializeCommandRecovery(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func (r *recoveryReconciler) initializeCommandRecovery(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		var version string
		err := tx.QueryRow("SELECT value FROM metadata WHERE key='command_reconciliation_schema'").Scan(&version)
		if err == nil {
			if version != "1" {
				return fmt.Errorf("unknown command reconciliation schema")
			}
			for _, table := range []string{"reconciliation_command_sessions", "reconciliation_command_records"} {
				var count int
				if err = tx.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
					return err
				}
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_command_sessions(producer TEXT PRIMARY KEY,body TEXT NOT NULL);
CREATE TABLE reconciliation_command_records(producer TEXT NOT NULL,ordinal INTEGER NOT NULL,id TEXT NOT NULL UNIQUE,request_key TEXT NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL,outcome TEXT NOT NULL,PRIMARY KEY(producer,ordinal));
INSERT INTO metadata VALUES('command_reconciliation_schema','1');`)
		return err
	})
}
func (r *recoveryReconciler) commandRecoveryEnrollment(p protocol.Producer) error {
	if _, err := r.config.enrollment(p); err != nil {
		return err
	}
	if r.config.Interaction != nil {
		for _, w := range r.config.Interaction.Workers {
			if w.Producer == p {
				return nil
			}
		}
	}
	return fmt.Errorf("command recovery producer not enrolled")
}
func readCommandRecoverySession(tx *sql.Tx, p protocol.Producer) (s commandRecoverySession, err error) {
	var body string
	if err = tx.QueryRow("SELECT body FROM reconciliation_command_sessions WHERE producer=?", p.ID).Scan(&body); err != nil {
		return
	}
	err = protocol.Decode([]byte(body), &s, 16384)
	return
}
func writeCommandRecoverySession(tx *sql.Tx, p protocol.Producer, s commandRecoverySession) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO reconciliation_command_sessions VALUES(?,?) ON CONFLICT(producer) DO UPDATE SET body=excluded.body", p.ID, string(body))
	return err
}
func (r *recoveryReconciler) CommandChallenge(ctx context.Context, p protocol.Producer) (ch protocol.RecoveryChallenge, err error) {
	if err = r.commandRecoveryEnrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := readCommandRecoverySession(tx, p)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if s.Summary != nil {
			return fmt.Errorf("command recovery already complete; inspect summary")
		}
		boot, up, err := r.recoveryClock()
		if err != nil {
			return err
		}
		now := r.now().UTC()
		if s.Challenge.Nonce != "" {
			if now.Before(s.Challenge.IssuedAt) || (boot == s.Challenge.BootID && up < s.Challenge.Uptime) {
				return fmt.Errorf("command recovery clock moved backwards")
			}
			if interaction.ValidateCommandRecoveryChallenge(s.Challenge, r.journal.Before, p, now, boot, up) == nil {
				ch = s.Challenge
				return nil
			}
		}
		nonce, err := safefs.NewID("n")
		if err != nil {
			return err
		}
		ch = protocol.RecoveryChallenge{Operation: r.journal.Operation, Identity: r.journal.After, Producer: p, Nonce: nonce, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute), BootID: boot, Uptime: up}
		s.Challenge = ch
		return writeCommandRecoverySession(tx, p, s)
	})
	return
}
func (r *recoveryReconciler) checkCommandRecovery(tx *sql.Tx, ch protocol.RecoveryChallenge) (s commandRecoverySession, err error) {
	if err = r.commandRecoveryEnrollment(ch.Producer); err != nil {
		return
	}
	if ch.Operation != r.journal.Operation || ch.Identity != r.journal.After {
		return s, fmt.Errorf("command recovery operation mismatch")
	}
	if s, err = readCommandRecoverySession(tx, ch.Producer); err != nil {
		return
	}
	if protocol.Hash(s.Challenge) != protocol.Hash(ch) {
		return s, fmt.Errorf("command recovery nonce changed")
	}
	boot, up, err := r.recoveryClock()
	if err != nil {
		return
	}
	err = interaction.ValidateCommandRecoveryChallenge(ch, r.journal.Before, ch.Producer, r.now(), boot, up)
	return
}
func (r *recoveryReconciler) BeginCommands(ctx context.Context, ch protocol.RecoveryChallenge, m interaction.CommandRecoveryManifest) error {
	if m.Identity != r.journal.Before || m.Producer != ch.Producer || m.Commands > 65536 || m.Results > 8*m.Commands || !safefs.DigestPattern.MatchString(m.Digest) {
		return fmt.Errorf("invalid command recovery manifest")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := r.checkCommandRecovery(tx, ch)
		if err != nil {
			return err
		}
		if s.Summary != nil {
			return fmt.Errorf("command recovery completed")
		}
		if s.Manifest != nil && *s.Manifest != m {
			return fmt.Errorf("command recovery manifest changed")
		}
		s.Manifest = &m
		return writeCommandRecoverySession(tx, ch.Producer, s)
	})
}
func (r *recoveryReconciler) ImportCommands(ctx context.Context, ch protocol.RecoveryChallenge, after uint64, records []interaction.CommandRecoveryRecord) error {
	raw, err := json.Marshal(records)
	if err != nil || len(records) == 0 || len(records) > 128 || len(raw)+4096 > protocol.MaxRequest {
		return fmt.Errorf("command recovery page exceeds bounds")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := r.checkCommandRecovery(tx, ch)
		if err != nil {
			return err
		}
		if s.Manifest == nil || s.Summary != nil || after > s.Manifest.Commands || uint64(len(records)) > s.Manifest.Commands-after {
			return fmt.Errorf("command recovery page outside manifest")
		}
		if err = r.capacity(tx, len(raw)); err != nil {
			return err
		}
		for i, v := range records {
			if err = r.importRecoveryCommand(tx, ch, *s.Manifest, after+uint64(i)+1, v); err != nil {
				return err
			}
		}
		return nil
	})
}
func (r *recoveryReconciler) importRecoveryCommand(tx *sql.Tx, ch protocol.RecoveryChallenge, m interaction.CommandRecoveryManifest, n uint64, v interaction.CommandRecoveryRecord) error {
	if err := v.Validate(m); err != nil {
		return err
	}
	c := v.Command
	var hash string
	err := tx.QueryRow("SELECT hash FROM reconciliation_command_records WHERE producer=? AND ordinal=?", ch.Producer.ID, n).Scan(&hash)
	if err == nil {
		if hash != protocol.Hash(v) {
			return fmt.Errorf("command recovery evidence replay changed")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	var input, id string
	err = tx.QueryRow("SELECT input_hash,command_id FROM command_inbox WHERE request_key=?", c.RequestKey).Scan(&input, &id)
	if err == nil && (id != c.ID || input != c.InputHash) {
		return fmt.Errorf("restored request mapping conflicts with command evidence")
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	var body string
	err = tx.QueryRow("SELECT body FROM commands WHERE id=?", c.ID).Scan(&body)
	if err == nil {
		var old interaction.Command
		if err = protocol.Decode([]byte(body), &old, protocol.MaxRequest); err != nil {
			return err
		}
		if protocol.Hash(old) != protocol.Hash(c) {
			return fmt.Errorf("restored command conflicts with retained identity")
		}
	} else if err == sql.ErrNoRows {
		raw, _ := json.Marshal(c)
		if _, err = tx.Exec("INSERT INTO commands(id,request_key,input_hash,producer,body,state,imported,created) VALUES(?,?,?,?,?,'outcome_unknown',1,?)", c.ID, c.RequestKey, c.InputHash, c.Producer.ID, string(raw), c.IssuedAt.UnixNano()); err != nil {
			return err
		}
	} else {
		return err
	}
	if _, err = tx.Exec("INSERT INTO command_inbox VALUES(?,?,?) ON CONFLICT(request_key) DO NOTHING", c.RequestKey, c.InputHash, c.ID); err != nil {
		return err
	}
	for _, e := range v.Results {
		var existing string
		err = tx.QueryRow("SELECT hash FROM command_results WHERE command_id=? AND revision=?", c.ID, e.Result.Revision).Scan(&existing)
		if err == nil && existing != protocol.Hash(e.Result) {
			return fmt.Errorf("restored result contradicts retained result")
		}
		if err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	outcome, conflict, err := recoveryCommandOutcome(tx, v)
	if err != nil {
		return err
	}
	if conflict {
		if _, err = tx.Exec("UPDATE commands SET state='outcome_unknown',conflict=1 WHERE id=?", c.ID); err != nil {
			return err
		}
	}
	// Results remain old-epoch audit evidence. No new result receipts, replies,
	// confirmations, grants, or automatic executions are emitted by this import.
	raw, _ := json.Marshal(v)
	_, err = tx.Exec("INSERT INTO reconciliation_command_records VALUES(?,?,?,?,?,?,?)", ch.Producer.ID, n, c.ID, c.RequestKey, protocol.Hash(v), string(raw), outcome)
	return err
}
func (r *recoveryReconciler) FinishCommands(ctx context.Context, ch protocol.RecoveryChallenge) (summary interaction.CommandRecoverySummary, err error) {
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := r.checkCommandRecovery(tx, ch)
		if err != nil {
			return err
		}
		if s.Summary != nil {
			summary = *s.Summary
			return nil
		}
		if s.Manifest == nil {
			return fmt.Errorf("missing command recovery manifest")
		}
		rows, err := tx.Query("SELECT ordinal,hash,body,outcome FROM reconciliation_command_records WHERE producer=? ORDER BY ordinal", ch.Producer.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		var hashes []string
		var results uint64
		summary = interaction.CommandRecoverySummary{Challenge: ch, Manifest: *s.Manifest}
		for rows.Next() {
			var n uint64
			var hash, body, state string
			if err = rows.Scan(&n, &hash, &body, &state); err != nil {
				return err
			}
			var v interaction.CommandRecoveryRecord
			if err = protocol.Decode([]byte(body), &v, protocol.MaxRequest); err != nil {
				return err
			}
			if n != uint64(len(hashes))+1 || hash != protocol.Hash(v) {
				return fmt.Errorf("command recovery history damaged")
			}
			hashes = append(hashes, hash)
			results += uint64(len(v.Results))
			if interaction.Terminal(state) {
				summary.Terminal++
			} else {
				summary.Uncertain++
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if uint64(len(hashes)) != s.Manifest.Commands || results != s.Manifest.Results || interaction.CommandRecoveryDigest(hashes) != s.Manifest.Digest {
			return fmt.Errorf("command recovery history incomplete")
		}
		if err = tx.QueryRow("SELECT count(*) FROM recovery_commands c WHERE producer=? AND NOT EXISTS(SELECT 1 FROM reconciliation_command_records r WHERE r.id=c.id)", ch.Producer.ID).Scan(&summary.MissingRestored); err != nil {
			return err
		}
		s.Summary = &summary
		return writeCommandRecoverySession(tx, ch.Producer, s)
	})
	return
}
func (r *recoveryReconciler) CommandSummary(ctx context.Context, p protocol.Producer) (summary interaction.CommandRecoverySummary, err error) {
	if err = r.commandRecoveryEnrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := readCommandRecoverySession(tx, p)
		if err == sql.ErrNoRows {
			return protocol.ErrRecoveryIncomplete
		}
		if err != nil {
			return err
		}
		if s.Summary == nil {
			return protocol.ErrRecoveryIncomplete
		}
		summary = *s.Summary
		return nil
	})
	return
}
