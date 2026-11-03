package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// ReconcileCommands exchanges retained specialist history with an offline
// quarantined gateway. The caller owns the session. There is no admission,
// confirmation renewal, identity rotation or provider intake activation here.
func ReconcileCommands(ctx context.Context, l *Ledger, s CommandRecoverySession) (summary CommandRecoverySummary, err error) {
	if l == nil || s == nil {
		return summary, fmt.Errorf("command recovery dependencies missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	summary, err = s.CommandSummary(ctx, l.producer)
	if err == nil {
		return summary, l.recordRecoverySummary(ctx, summary)
	}
	if !errors.Is(err, protocol.ErrRecoveryIncomplete) {
		return
	}
	var complete int
	err = l.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery_completion'").Scan(&complete)
	if err != nil {
		return
	}
	if complete != 0 {
		return summary, fmt.Errorf("gateway lost retained command recovery completion")
	}
	ch, err := s.CommandChallenge(ctx, l.producer)
	if err != nil {
		return
	}
	m, err := l.CaptureRecovery(ctx, ch)
	if err != nil {
		return
	}
	if err = s.BeginCommands(ctx, ch, m); err != nil {
		return
	}
	var after uint64
	for {
		page, e := l.RecoveryPage(ctx, ch, after)
		if e != nil {
			return summary, e
		}
		if len(page.Records) > 0 {
			if err = s.ImportCommands(ctx, ch, after, page.Records); err != nil {
				return
			}
		}
		if page.Done {
			break
		}
		after = page.Next
	}
	summary, err = s.FinishCommands(ctx, ch)
	if err != nil {
		return
	}
	err = l.recordRecoverySummary(ctx, summary)
	return
}
func (l *Ledger) recordRecoverySummary(ctx context.Context, summary CommandRecoverySummary) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	saved, err := l.readRecovery(tx)
	if err != nil {
		return err
	}
	if protocol.Hash(summary.Challenge) != protocol.Hash(saved.Challenge) || summary.Manifest != saved.Manifest || summary.ExecutionEnabled ||
		summary.Terminal > saved.Manifest.Commands || summary.Uncertain != saved.Manifest.Commands-summary.Terminal || summary.MissingRestored > 65536 {
		return fmt.Errorf("command recovery summary mismatch")
	}
	var exists int
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery_completion'").Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		var raw string
		var old CommandRecoverySummary
		if err = tx.QueryRow("SELECT body FROM interactive_recovery_completion WHERE singleton=1").Scan(&raw); err != nil {
			return err
		}
		if err = protocol.Decode([]byte(raw), &old, 16384); err != nil {
			return err
		}
		if protocol.Hash(old) != protocol.Hash(summary) {
			return fmt.Errorf("command recovery completion changed")
		}
		return nil
	}
	if intent, _, err := readLedgerCommandCommit(tx); err != nil {
		return err
	} else if intent != nil {
		return fmt.Errorf("command commit lost its prior reconciliation completion")
	}
	if _, err = tx.Exec("CREATE TABLE interactive_recovery_completion(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL)"); err != nil {
		return err
	}
	raw, _ := json.Marshal(summary)
	if _, err = tx.Exec("INSERT INTO interactive_recovery_completion VALUES(1,?)", string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
