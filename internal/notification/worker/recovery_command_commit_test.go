package worker

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestCommandRecoveryCommitPreservesHistoryAndFences(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "running"}[running], func(t *testing.T) {
			f := newCommandRecoveryFixture(t, running)
			ctx := context.Background()
			summary, err := interaction.ReconcileCommands(ctx, f.ledger, f.r)
			must(t, err)
			var before string
			must(t, f.db.QueryRow("SELECT json_group_array(json_array(id,revision,body,receipt,acked)) FROM interactive_results ORDER BY id,revision").Scan(&before))
			proof, err := interaction.CommitCommandRecovery(ctx, f.ledger, f.r)
			must(t, err)
			must(t, proof.Validate())
			if protocol.Hash(proof.Summary) != protocol.Hash(summary) {
				t.Fatal("commit changed summary")
			}
			again, err := interaction.CommitCommandRecovery(ctx, f.ledger, f.r)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(proof) {
				t.Fatal("lost response replay changed proof")
			}
			if _, err = interaction.OpenLedger(ctx, f.db, producer, summary.Manifest.Identity, false); err == nil {
				t.Fatal("old epoch reopened")
			}
			current, err := interaction.OpenLedger(ctx, f.db, producer, summary.Challenge.Identity, false)
			must(t, err)
			again, err = interaction.CommitCommandRecovery(ctx, current, f.r)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(proof) {
				t.Fatal("new handle changed checkpoint")
			}
			if _, err = interaction.ReconcileCommands(ctx, current, f.r); err != nil {
				t.Fatal("historical summary no longer replayable", err)
			}
			for _, ledger := range []*interaction.Ledger{f.ledger, current} {
				if _, err = ledger.PendingResults(ctx); err == nil {
					t.Fatal("recovery exposed old result outbox")
				}
				if err = ledger.Import(ctx, f.pending); err == nil {
					t.Fatal("recovery enabled admission")
				}
				if _, err = ledger.Execute(ctx, f.first, f.grant, "", time.Now(), func(*sql.Tx, interaction.Command) (interaction.Result, error) {
					t.Fatal("executed twice")
					return interaction.Result{}, nil
				}); err == nil {
					t.Fatal("recovery enabled execution")
				}
				if _, err = ledger.CompleteJob(ctx, f.first.ID, "durable-job", func(*sql.Tx) (interaction.Result, error) {
					t.Fatal("completed old job")
					return interaction.Result{}, nil
				}); err == nil {
					t.Fatal("recovery enabled job completion")
				}
			}
			var after string
			must(t, f.db.QueryRow("SELECT json_group_array(json_array(id,revision,body,receipt,acked)) FROM interactive_results ORDER BY id,revision").Scan(&after))
			if after != before {
				t.Fatal("old receipts or results changed")
			}
			var floors, unfenced, executions, newReceipts int
			must(t, f.db.QueryRow("SELECT count(*) FROM interactive_recovery_result_floors").Scan(&floors))
			must(t, f.db.QueryRow("SELECT count(*) FROM interactive_inbox WHERE fenced=0").Scan(&unfenced))
			must(t, f.db.QueryRow("SELECT value FROM domain_calls").Scan(&executions))
			must(t, f.r.db.QueryRow("SELECT count(*) FROM command_results").Scan(&newReceipts))
			if floors != 2 || unfenced != 0 || executions != 1 || newReceipts != 0 {
				t.Fatal(floors, unfenced, executions, newReceipts)
			}
			must(t, f.r.Close())
			if normal, err := Open(f.path, f.r.config, false); err == nil {
				normal.Close()
				t.Fatal("command commit activated gateway")
			}
		})
	}
}

func TestCommandRecoveryCommitRejectsGatewayDamage(t *testing.T) {
	for _, mode := range []string{"records", "mapping", "command", "schema", "summary", "grant", "confirmation", "state"} {
		t.Run(mode, func(t *testing.T) {
			f := newCommandRecoveryFixture(t, false)
			ctx := context.Background()
			_, err := interaction.ReconcileCommands(ctx, f.ledger, f.r)
			must(t, err)
			switch mode {
			case "records":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_command_records")
			case "mapping":
				_, err = f.r.db.Exec("UPDATE command_inbox SET command_id='changed'")
			case "command":
				_, err = f.r.db.Exec("UPDATE commands SET body=json_set(body,'$.id','changed')")
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='command_reconciliation_schema'")
			case "summary":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_command_sessions")
			case "grant":
				_, err = f.r.db.Exec("UPDATE commands SET grant='old-grant'")
			case "confirmation":
				_, err = f.r.db.Exec("UPDATE commands SET confirmed=1")
			case "state":
				_, err = f.r.db.Exec("UPDATE commands SET state='queued'")
			}
			must(t, err)
			if _, err = interaction.CommitCommandRecovery(ctx, f.ledger, f.r); err == nil {
				t.Fatal("damaged gateway authorized commit")
			}
			old, err := interaction.OpenLedger(ctx, f.db, producer, f.r.journal.Before, false)
			must(t, err)
			if _, err = old.PendingResults(ctx); err == nil {
				t.Fatal("failed commit released source fence")
			}
		})
	}
}
