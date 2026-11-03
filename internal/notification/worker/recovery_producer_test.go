package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestRecoveryProducerCommitLeavesOutcomesUnread(t *testing.T) {
	f, _, g := generationFixture(t)
	proof, err := f.r.PrepareRecoveryProducerCommit(context.Background(), producer)
	must(t, err)
	must(t, proof.Validate())
	if proof.Generation != g || proof.Cursor.Position != 0 || proof.HighestIssued != f.manifest.HighestIssued || proof.CommandSummaryHash != "" {
		t.Fatal(proof)
	}
	var receipts, listed, acked int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
	must(t, f.r.db.QueryRow("SELECT listed,acked FROM producers WHERE id=?", producer.ID).Scan(&listed, &acked))
	if receipts != 1 || listed != 0 || acked != 0 {
		t.Fatal("commit preparation consumed outcomes")
	}
}

func TestRecoveryProducerCommitRejectsChangedState(t *testing.T) {
	for _, mode := range []string{"listed", "acked", "submitted", "epoch", "plan", "summary"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _ := generationFixture(t)
			var err error
			switch mode {
			case "listed":
				_, err = f.r.db.Exec("UPDATE producers SET listed=1")
			case "acked":
				_, err = f.r.db.Exec("UPDATE producers SET acked=1")
			case "submitted":
				_, err = f.r.db.Exec("UPDATE producers SET submitted=submitted+1")
			case "epoch":
				_, err = f.r.db.Exec("UPDATE producers SET epoch='changed'")
			case "plan":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), generationFile(producer)))
			case "summary":
				_, err = f.r.db.Exec("UPDATE reconciliation_producers SET completion=''")
			}
			must(t, err)
			if _, err = f.r.PrepareRecoveryProducerCommit(context.Background(), producer); err == nil {
				t.Fatal("damaged producer commit accepted")
			}
		})
	}
}

func TestRecoveryProducerCommitChecksBothCommandLedgers(t *testing.T) {
	for _, mode := range []string{"valid", "worker_history", "worker_schema", "source_history", "source_pin", "source_completion"} {
		t.Run(mode, func(t *testing.T) {
			f := newCommandRecoveryFixture(t, false)
			ctx := context.Background()
			ch, err := f.r.Challenge(ctx, producer)
			must(t, err)
			manifest := protocol.RecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: f.r.journal.Before, Producer: producer}}
			must(t, f.r.Begin(ctx, ch, manifest))
			_, err = f.r.Finish(ctx, ch, snapshot(time.Now().UTC(), 1, 1))
			must(t, err)
			_, err = f.r.PrepareRecoveryOwnership(ctx, producer)
			must(t, err)
			_, err = f.r.PrepareRecoveryGeneration(ctx, producer)
			must(t, err)
			if _, err = f.r.PrepareRecoveryProducerCommit(ctx, producer); err == nil {
				t.Fatal("unreconciled commands accepted")
			}
			summary, err := interaction.ReconcileCommands(ctx, f.ledger, f.r)
			must(t, err)
			if mode == "worker_history" {
				_, err = f.r.db.Exec("DELETE FROM reconciliation_command_records")
			}
			if mode == "worker_schema" {
				_, err = f.r.db.Exec("UPDATE metadata SET value='2' WHERE key='command_reconciliation_schema'")
			}
			must(t, err)
			proof, err := f.r.PrepareRecoveryProducerCommit(ctx, producer)
			if mode == "worker_history" || mode == "worker_schema" {
				if err == nil {
					t.Fatal("lost worker evidence accepted")
				}
				return
			}
			must(t, err)
			if proof.CommandSummaryHash != protocol.Hash(summary) {
				t.Fatal("command completion not bound")
			}
			switch mode {
			case "source_history":
				_, err = f.db.Exec("UPDATE interactive_recovery_records SET hash='changed'")
			case "source_pin":
				_, err = f.db.Exec("UPDATE interactive_identity SET body=json_set(body,'$.recovery_operation','changed')")
			case "source_completion":
				_, err = f.db.Exec("DELETE FROM interactive_recovery_completion")
			}
			must(t, err)
			tx, err := f.db.BeginTx(ctx, nil)
			must(t, err)
			defer tx.Rollback()
			err = interaction.ValidateDeliveryRecoveryCommit(tx, proof)
			if mode == "valid" {
				must(t, err)
			} else if err == nil {
				t.Fatal("lost source evidence accepted")
			}
		})
	}
}
