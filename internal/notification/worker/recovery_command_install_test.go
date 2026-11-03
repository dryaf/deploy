package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func commandInstallFixture(t *testing.T, running bool) (commandRecoveryFixture, interaction.CommandRecoveryCommit) {
	t.Helper()
	f := newCommandRecoveryFixture(t, running)
	ctx := context.Background()
	_, err := interaction.ReconcileCommands(ctx, f.ledger, f.r)
	must(t, err)
	proof, err := interaction.CommitCommandRecovery(ctx, f.ledger, f.r)
	must(t, err)
	must(t, f.r.initializeCommandInstallations(ctx))
	return f, proof
}

func TestCommandRecoveryInstallationPreservesEvidenceAndFences(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "running"}[running], func(t *testing.T) {
			f, proof := commandInstallFixture(t, running)
			ctx := context.Background()
			var commands, replies, results string
			must(t, f.r.db.QueryRow("SELECT json_group_array(body) FROM commands ORDER BY id").Scan(&commands))
			must(t, f.r.db.QueryRow("SELECT json_group_array(body) FROM command_replies ORDER BY command_id,kind,revision").Scan(&replies))
			must(t, f.db.QueryRow("SELECT json_group_array(json_array(body,receipt,acked)) FROM interactive_results ORDER BY id,revision").Scan(&results))
			installed, err := interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
			must(t, err)
			must(t, installed.Validate())
			if protocol.Hash(installed.Checkpoint) != protocol.Hash(proof) || installed.Commands != 2 {
				t.Fatal(installed)
			}
			must(t, f.r.transaction(ctx, func(tx *sql.Tx) error {
				first, err := readCommand(tx, f.first.ID)
				if err != nil {
					return err
				}
				pending, err := readCommand(tx, f.pending.ID)
				if err != nil {
					return err
				}
				state := "succeeded"
				if running {
					state = "outcome_unknown"
				}
				if first.State != state || first.Revision != 1 || first.Grant != nil || pending.State != "outcome_unknown" || pending.Revision != 1 || pending.Prepared != nil || pending.Grant != nil {
					t.Fatal("wrong historical projection", first, pending)
				}
				return nil
			}))
			var afterCommands, afterReplies, afterResults string
			must(t, f.r.db.QueryRow("SELECT json_group_array(body) FROM commands ORDER BY id").Scan(&afterCommands))
			must(t, f.r.db.QueryRow("SELECT json_group_array(body) FROM command_replies ORDER BY command_id,kind,revision").Scan(&afterReplies))
			must(t, f.db.QueryRow("SELECT json_group_array(json_array(body,receipt,acked)) FROM interactive_results ORDER BY id,revision").Scan(&afterResults))
			if commands != afterCommands || replies != afterReplies || results != afterResults {
				t.Fatal("installation rewrote bodies, receipts, or replies")
			}
			var receipts, grants int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM command_results").Scan(&receipts))
			must(t, f.r.db.QueryRow("SELECT count(*) FROM commands WHERE grant<>'' OR token_hash IS NOT NULL OR confirmed<>0 OR preparation<>''").Scan(&grants))
			if receipts != 0 || grants != 0 {
				t.Fatal("installation minted receipts or authority")
			}
			// Exercise ordinary command handlers using an isolated borrowed candidate.
			// This is not managed activation and starts no service or provider adapter.
			runtime := &Store{db: f.r.db, dir: f.r.dir, identity: f.r.journal.After, config: f.r.config, now: time.Now}
			if _, err = runtime.GrantCommand(ctx, producer, f.first.ID); err == nil {
				t.Fatal("historical execution allowed")
			}
			if err = runtime.ImportCommand(ctx, producer, f.pending.ID, protocol.Hash(f.pending)); err == nil {
				t.Fatal("historical admission allowed")
			}
			last, err := f.ledger.Latest(ctx, f.first.ID)
			must(t, err)
			if _, err = runtime.SubmitCommandResult(ctx, producer, last); err == nil {
				t.Fatal("old result replay entered new receipt stream")
			}
			queued, err := runtime.Commands(ctx, producer)
			must(t, err)
			if len(queued) != 0 {
				t.Fatal("historical commands entered specialist queue")
			}
			again, err := interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(installed) {
				t.Fatal("installation replay changed")
			}
			cfg := f.r.config
			must(t, f.r.Close())
			session, err := OpenCommandRecoveryInstallSession(ctx, f.path, cfg)
			must(t, err)
			defer session.Close()
			reopened, err := interaction.OpenLedger(ctx, f.db, producer, f.r.journal.After, false)
			must(t, err)
			again, err = interaction.InstallCommandRecovery(ctx, reopened, session)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(installed) {
				t.Fatal("restart changed installation")
			}
			must(t, session.Close())
			if normal, err := Open(f.path, cfg, false); err == nil {
				normal.Close()
				t.Fatal("installation activated gateway")
			}
		})
	}
}

func TestCommandRecoveryInstallationRejectsCorruption(t *testing.T) {
	for _, mode := range []string{"outcome", "revision", "mapping", "grant", "summary", "schema", "source_uncommitted", "source_fence", "producer_column", "request_column", "input_column"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := commandInstallFixture(t, false)
			var err error
			switch mode {
			case "outcome":
				_, err = f.r.db.Exec("UPDATE reconciliation_command_records SET outcome='failed' WHERE outcome='succeeded'")
			case "revision":
				_, err = f.r.db.Exec("UPDATE commands SET revision=100")
			case "producer_column":
				_, err = f.r.db.Exec("UPDATE commands SET producer='another' WHERE id=?", f.first.ID)
			case "request_column":
				_, err = f.r.db.Exec("UPDATE commands SET request_key='another' WHERE id=?", f.first.ID)
			case "input_column":
				_, err = f.r.db.Exec("UPDATE commands SET input_hash='another' WHERE id=?", f.first.ID)
			case "mapping":
				_, err = f.r.db.Exec("DELETE FROM command_inbox")
			case "grant":
				_, err = f.r.db.Exec("UPDATE commands SET grant='revived'")
			case "summary":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_command_sessions")
			case "schema":
				_, err = f.r.db.Exec("UPDATE metadata SET value='2' WHERE key='command_installation_schema'")
			case "source_uncommitted":
				_, err = f.db.Exec("UPDATE interactive_recovery_commit SET proof=''")
			case "source_fence":
				_, err = f.db.Exec("UPDATE interactive_inbox SET fenced=0")
			}
			must(t, err)
			if _, err = interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r); err == nil {
				t.Fatal("damaged evidence installed")
			}
			var count int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_command_installations").Scan(&count))
			if count != 0 {
				t.Fatal("bad installation recorded")
			}
		})
	}
}

func TestCommandRecoveryInstallationRejectsPartialRollback(t *testing.T) {
	for _, mode := range []string{"intent", "record", "state", "revision", "imported", "conflict", "non_admission", "schema"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := commandInstallFixture(t, false)
			_, err := interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r)
			must(t, err)
			switch mode {
			case "intent":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), commandInstallationFile(producer)))
			case "record":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_command_installations")
			case "state":
				_, err = f.r.db.Exec("UPDATE commands SET state='outcome_unknown' WHERE id=?", f.first.ID)
			case "revision":
				_, err = f.r.db.Exec("UPDATE commands SET revision=0 WHERE id=?", f.first.ID)
			case "imported":
				_, err = f.r.db.Exec("UPDATE commands SET imported=0 WHERE id=?", f.first.ID)
			case "conflict":
				_, err = f.r.db.Exec("UPDATE commands SET conflict=1 WHERE id=?", f.first.ID)
			case "non_admission":
				_, err = f.r.db.Exec("UPDATE commands SET non_admission='old-proof' WHERE id=?", f.first.ID)
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='command_installation_schema'")
			}
			must(t, err)
			if _, err = interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r); err == nil {
				t.Fatal("partial rollback accepted", mode)
			}
		})
	}
}

func TestCommandRecoveryInstallationLostResponses(t *testing.T) {
	for _, boundary := range []string{"command_install_intent", "command_install_transaction", "command_install_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f, _ := commandInstallFixture(t, false)
			f.r.fault = func(at string) error {
				if at == boundary {
					return errors.New("lost response")
				}
				return nil
			}
			if _, err := interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r); err == nil {
				t.Fatal("fault ignored")
			}
			f.r.fault = nil
			got, err := interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r)
			must(t, err)
			again, err := interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r)
			must(t, err)
			if protocol.Hash(got) != protocol.Hash(again) {
				t.Fatal("lost response changed installation")
			}
		})
	}
}

func TestCommandRecoveryInstallationPreservesHistoricalReceipts(t *testing.T) {
	for _, damage := range []string{"none", "receipt", "ack"} {
		t.Run(damage, func(t *testing.T) {
			f := newCommandRecoverySnapshotFixture(t, false, true)
			ctx := context.Background()
			_, err := interaction.ReconcileCommands(ctx, f.ledger, f.r)
			must(t, err)
			_, err = interaction.CommitCommandRecovery(ctx, f.ledger, f.r)
			must(t, err)
			must(t, f.r.initializeCommandInstallations(ctx))
			var before string
			must(t, f.r.db.QueryRow("SELECT json_array(hash,body,receipt,acked) FROM command_results WHERE command_id=?", f.first.ID).Scan(&before))
			if damage != "none" {
				f.r.fault = func(at string) error {
					if at == "command_install_intent" {
						return errors.New("lost response")
					}
					return nil
				}
				if _, err = interaction.InstallCommandRecovery(ctx, f.ledger, f.r); err == nil {
					t.Fatal("missing fault")
				}
				f.r.fault = nil
				if damage == "ack" {
					_, err = f.r.db.Exec("UPDATE command_results SET acked=0")
				} else {
					_, err = f.r.db.Exec("UPDATE command_results SET receipt=json_set(receipt,'$.identity.epoch',999)")
				}
				must(t, err)
				if _, err = interaction.InstallCommandRecovery(ctx, f.ledger, f.r); err == nil {
					t.Fatal("changed receipt history installed")
				}
				return
			}
			_, err = interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
			must(t, err)
			var after string
			must(t, f.r.db.QueryRow("SELECT json_array(hash,body,receipt,acked) FROM command_results WHERE command_id=?", f.first.ID).Scan(&after))
			if after != before {
				t.Fatal("historical receipt or ACK changed")
			}
			runtime := &Store{db: f.r.db, dir: f.r.dir, identity: f.r.journal.After, config: f.r.config, now: time.Now}
			last, err := f.ledger.Latest(ctx, f.first.ID)
			must(t, err)
			if _, err = runtime.SubmitCommandResult(ctx, producer, last); err == nil {
				t.Fatal("historical receipt replayed")
			}
		})
	}
}

func TestCommandRecoveryInstallationPreservesHigherSnapshotRevision(t *testing.T) {
	f := newCommandRecoverySnapshotFixture(t, false, true)
	// A restored gateway head ahead of the surviving specialist must not be lowered
	// or treated as evidence that the source's earlier terminal result is current.
	_, err := f.r.db.Exec("UPDATE recovery_commands SET revision=2 WHERE id=?", f.first.ID)
	must(t, err)
	_, err = f.r.db.Exec("UPDATE commands SET revision=2 WHERE id=?", f.first.ID)
	must(t, err)
	ctx := context.Background()
	summary, err := interaction.ReconcileCommands(ctx, f.ledger, f.r)
	must(t, err)
	if summary.Terminal != 0 {
		t.Fatal("earlier result treated as current")
	}
	_, err = interaction.CommitCommandRecovery(ctx, f.ledger, f.r)
	must(t, err)
	must(t, f.r.initializeCommandInstallations(ctx))
	_, err = interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
	must(t, err)
	var state string
	var revision, conflict int
	must(t, f.r.db.QueryRow("SELECT state,revision,conflict FROM commands WHERE id=?", f.first.ID).Scan(&state, &revision, &conflict))
	if state != "outcome_unknown" || revision != 2 || conflict != 1 {
		t.Fatal(state, revision, conflict)
	}
}

func TestCommandRecoveryInstallationRechecksAuthority(t *testing.T) {
	for _, stage := range []string{"command_install_intent", "command_install_transaction"} {
		for _, mode := range []string{"lease", "identity", "context"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				f, _ := commandInstallFixture(t, false)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.r.fault = func(at string) error {
					if at != stage {
						return nil
					}
					switch mode {
					case "lease":
						lock := filepath.Join(f.path, "worker.lock")
						must(t, os.Rename(lock, lock+".held"))
						must(t, os.WriteFile(lock, nil, 0600))
					case "identity":
						changed := f.r.journal.After
						changed.Recovery = "changed"
						must(t, f.r.dir.WriteJSON("identity.json", changed, true))
					case "context":
						cancel()
					}
					return nil
				}
				if _, err := interaction.InstallCommandRecovery(ctx, f.ledger, f.r); err == nil {
					t.Fatal("lost authority installed outcomes")
				}
				var count int
				must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_command_installations").Scan(&count))
				if count != 0 {
					t.Fatal("authority failure committed installation")
				}
			})
		}
	}
}

func TestCommandRecoveryInstallationRetainsNonAdmissionOutcomes(t *testing.T) {
	for _, outcome := range []string{"expired", "rejected", "non_admission"} {
		t.Run(outcome, func(t *testing.T) {
			f := newCommandRecoveryFixture(t, false)
			ctx := context.Background()
			if outcome == "non_admission" {
				grant := f.grant
				grant.CommandID = f.pending.ID
				grant.CommandHash = protocol.Hash(f.pending)
				_, err := f.ledger.ProveNonAdmission(ctx, f.pending, grant)
				must(t, err)
			} else {
				// Model the source's retained receipt for a late preparation superseded by
				// an old gateway terminal decision, without relabelling that receipt.
				last, err := f.ledger.Latest(ctx, f.pending.ID)
				must(t, err)
				receipt := interaction.ResultReceipt{Identity: f.grant.Identity, Producer: producer, CommandID: f.pending.ID, Revision: last.Revision, Hash: protocol.Hash(last), Disposition: "superseded_no_effect", NonAdmission: true, TerminalState: outcome, TerminalDecisionID: protocol.Hash(outcome)}
				must(t, f.ledger.ImportReceipt(ctx, receipt))
			}
			summary, err := interaction.ReconcileCommands(ctx, f.ledger, f.r)
			must(t, err)
			if summary.Terminal != 2 {
				t.Fatal("non-admission evidence discarded", summary)
			}
			_, err = interaction.CommitCommandRecovery(ctx, f.ledger, f.r)
			must(t, err)
			must(t, f.r.initializeCommandInstallations(ctx))
			_, err = interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
			must(t, err)
			var state, operationalProof string
			must(t, f.r.db.QueryRow("SELECT state,non_admission FROM commands WHERE id=?", f.pending.ID).Scan(&state, &operationalProof))
			want := outcome
			if want == "non_admission" {
				want = "expired"
			}
			if state != want || operationalProof != "" {
				t.Fatal("old proof became current authority", state, operationalProof)
			}
		})
	}
}

func TestCommandRecoveryInstallationChecksCapacityBeforeIntent(t *testing.T) {
	f, _ := commandInstallFixture(t, false)
	limit := f.r.config.MaxDatabaseBytes
	f.r.config.MaxDatabaseBytes = 1
	if _, err := interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r); err == nil {
		t.Fatal("over-capacity installation accepted")
	}
	_, err := os.Stat(filepath.Join(f.r.stage.File().Name(), commandInstallationFile(producer)))
	if !os.IsNotExist(err) {
		t.Fatal("capacity failure published intent", err)
	}
	f.r.config.MaxDatabaseBytes = limit
	_, err = interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r)
	must(t, err)
}
