package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"golang.org/x/sys/unix"
)

func TestRecoveryHandlesObserveAuthorityChanges(t *testing.T) {
	for _, mode := range []string{"lock_missing", "lock_replaced", "lock_fifo", "identity", "journal_missing", "journal_changed", "manifest", "closed"} {
		t.Run(mode, func(t *testing.T) {
			f, d := dispositionFixture(t, "retry")
			ctx := context.Background()
			_, err := f.r.DecideRecoveryMessage(ctx, d)
			must(t, err)
			switch mode {
			case "lock_missing", "lock_replaced", "lock_fifo":
				name := filepath.Join(f.path, "worker.lock")
				must(t, os.Remove(name))
				if mode == "lock_replaced" {
					must(t, os.WriteFile(name, nil, 0600))
				}
				if mode == "lock_fifo" {
					must(t, unix.Mkfifo(name, 0600))
				}
			case "identity":
				must(t, f.r.dir.WriteJSON("identity.json", f.r.journal.Before, true))
			case "journal_missing":
				must(t, os.Remove(filepath.Join(f.path, "recovery.json")))
			case "journal_changed":
				j := f.r.journal
				j.CandidateHash = protocol.Hash("another candidate")
				must(t, f.r.dir.WriteJSON("recovery.json", j, true))
			case "manifest":
				must(t, os.WriteFile(filepath.Join(f.r.stage.File().Name(), "reconciliation.json"), []byte("{}"), 0600))
			case "closed":
				must(t, f.r.Close())
			}
			if _, err = f.r.Summary(ctx, producer); err == nil {
				t.Fatal("stale recovery handle read evidence")
			}
			if _, err = f.r.RecoveryMessageDisposition(ctx, producer, d.MessageID); err == nil {
				t.Fatal("stale recovery handle returned decision")
			}
			if _, err = f.r.DecideRecoveryMessage(ctx, d); err == nil {
				t.Fatal("stale recovery handle wrote")
			}
		})
	}
}

func TestRecoveryAuthorityChangeBeforeCommitRollsBack(t *testing.T) {
	f, d := dispositionFixture(t, "retry")
	ctx := context.Background()
	f.r.fault = func(stage string) error {
		if stage == "reconciliation_transaction" {
			return f.r.dir.WriteJSON("identity.json", f.r.journal.Before, true)
		}
		return nil
	}
	if _, err := f.r.DecideRecoveryMessage(ctx, d); err == nil {
		t.Fatal("changed epoch committed a decision")
	}
	var count int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_dispositions").Scan(&count))
	if count != 0 {
		t.Fatal("transaction not rolled back")
	}
	// Restoring the exact original authority permits only the already-journaled
	// choice, retaining its audit time. A new choice still cannot replace it.
	f.r.fault = nil
	must(t, f.r.dir.WriteJSON("identity.json", f.r.journal.After, true))
	changed := d
	changed.Reason = "replace old decision"
	if _, err := f.r.DecideRecoveryMessage(ctx, changed); err == nil {
		t.Fatal("intent replaced")
	}
	record, err := f.r.DecideRecoveryMessage(ctx, d)
	must(t, err)
	if record.Decision != d || !record.RecordedAt.Equal(f.now) {
		t.Fatal(record)
	}
}
