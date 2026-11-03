//go:build linux

package backup

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
)

func backupCommitFixture(t *testing.T) (*backupEvaluationFixture, notify.RecoveryProducerSession, notify.RecoverySummary) {
	t.Helper()
	f, session, summary := backupOwnershipFixture(t)
	_, err := prepareBackupRecoveryOwnership(context.Background(), f.state, f.config, session)
	testutil.Must(t, err)
	testutil.Must(t, session.Close())
	ledger, err := interaction.OpenLedger(context.Background(), f.state.db, f.binding.Producer, f.binding.Identity, true)
	testutil.Must(t, err)
	commands, err := worker.OpenCommandRecoverySession(context.Background(), f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	_, err = interaction.ReconcileCommands(context.Background(), ledger, commands)
	testutil.Must(t, err)
	testutil.Must(t, commands.Close())
	next, err := worker.OpenRecoveryProducerSession(context.Background(), f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	t.Cleanup(func() { next.Close() })
	return f, next, summary
}

type interruptedProducerSession struct {
	notify.RecoveryProducerSession
	after func(*notify.RecoveryProducerCommit) error
}

func (s interruptedProducerSession) PrepareRecoveryProducerCommit(ctx context.Context, p notify.Producer) (notify.RecoveryProducerCommit, error) {
	proof, err := s.RecoveryProducerSession.PrepareRecoveryProducerCommit(ctx, p)
	if err == nil && s.after != nil {
		err = s.after(&proof)
	}
	return proof, err
}

func TestBackupRecoveryProducerCommitPreservesEvidenceAndFence(t *testing.T) {
	f, session, summary := backupCommitFixture(t)
	ctx := context.Background()
	var beforeExports, beforeReceipts, beforeObligations string
	testutil.Must(t, f.state.db.QueryRow("SELECT json_group_array(json_array(id,body,state,accepted,acceptance)) FROM notification_exports ORDER BY seq").Scan(&beforeExports))
	testutil.Must(t, f.state.db.QueryRow("SELECT json_group_array(body) FROM notification_receipt_history ORDER BY position").Scan(&beforeReceipts))
	testutil.Must(t, f.state.db.QueryRow("SELECT json_group_array(json_array(id,disposition,covered_by,reason)) FROM notification_obligations ORDER BY id").Scan(&beforeObligations))
	_, err := f.state.db.Exec("UPDATE backup_notification_transfers SET cancel_reason='incident resolved',cancel_sent=1 WHERE submitted=0")
	testutil.Must(t, err)
	v, err := commitBackupNotificationRecovery(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	if v.BeforeCursor != summary.Manifest.ImportedReceipts || v.BeforeCursor.Position == 0 || v.Proof.Cursor.Position != 0 || v.After.Identity != summary.Challenge.Identity || v.After.Generation == f.binding.Generation {
		t.Fatal(v)
	}
	again, err := commitBackupNotificationRecovery(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	if again != v {
		t.Fatal("replay changed commit")
	}
	// Older preparation APIs retain their original proof after the source commit.
	old, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	if old != v.Intent.Ownership {
		t.Fatal("old ownership proof changed")
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		owner, err := readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if owner.Mode != "fenced" || owner.Binding != v.After {
			t.Fatal(owner)
		}
		var identityJSON string
		var issued uint64
		var cursor int64
		if err = tx.QueryRow("SELECT identity,submitted,cursor FROM notification_producer").Scan(&identityJSON, &issued, &cursor); err != nil {
			return err
		}
		var identity notify.Identity
		if err = notify.Decode([]byte(identityJSON), &identity, 4096); err != nil {
			return err
		}
		if identity != v.After.Identity || issued != summary.Manifest.HighestIssued || cursor != 0 {
			t.Fatal("partial ledger commit")
		}
		return nil
	})
	var exports, receipts, obligations string
	testutil.Must(t, f.state.db.QueryRow("SELECT json_group_array(json_array(id,body,state,accepted,acceptance)) FROM notification_exports ORDER BY seq").Scan(&exports))
	testutil.Must(t, f.state.db.QueryRow("SELECT json_group_array(body) FROM notification_receipt_history ORDER BY position").Scan(&receipts))
	testutil.Must(t, f.state.db.QueryRow("SELECT json_group_array(json_array(id,disposition,covered_by,reason)) FROM notification_obligations ORDER BY id").Scan(&obligations))
	if exports != beforeExports || receipts != beforeReceipts || obligations != beforeObligations {
		t.Fatal("commit rewrote history or delivery obligations")
	}
	var pending, cancellations int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM backup_notification_transfers WHERE submitted=0 OR cancel_sent<>0").Scan(&pending))
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM backup_notification_transfers WHERE cancel_reason='incident resolved'").Scan(&cancellations))
	if pending != 0 || cancellations != 1 {
		t.Fatal("recovered submissions or cancellations lost")
	}
	if err = transferBackupNotifications(ctx, f.state, f.client); err == nil {
		t.Fatal("source activated")
	}
	if err = requireLegacyNotificationOwner(f.state); err == nil {
		t.Fatal("legacy activated")
	}
	testutil.Must(t, session.Close())
	if normal, err := worker.Open(f.gatewayPath, f.gatewayConfig, false); err == nil {
		normal.Close()
		t.Fatal("worker activated")
	}
	reopened, err := worker.OpenRecoveryProducerSession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	defer reopened.Close()
	again, err = commitBackupNotificationRecovery(ctx, f.state, f.config, reopened)
	testutil.Must(t, err)
	if again != v {
		t.Fatal("restart changed commit")
	}
}

func TestBackupRecoveryProducerCommitLostResponses(t *testing.T) {
	for _, boundary := range []string{"worker", "producer_intent_transaction", "producer_intent_commit", "producer_worker_prepared", "producer_commit_transaction", "producer_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f, session, _ := backupCommitFixture(t)
			failing := interruptedProducerSession{RecoveryProducerSession: session, after: func(*notify.RecoveryProducerCommit) error {
				if boundary == "worker" {
					return errors.New("lost worker proof")
				}
				return nil
			}}
			_, err := commitBackupNotificationRecoveryWith(context.Background(), f.state, f.config, failing, func(at string) error {
				if at == boundary {
					return errors.New("lost response")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault ignored")
			}
			var count int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_commit'").Scan(&count))
			want := 0
			if boundary == "producer_commit" {
				want = 1
			}
			if count != want {
				t.Fatal("incorrect commit durability", count)
			}
			_, err = commitBackupNotificationRecovery(context.Background(), f.state, f.config, session)
			testutil.Must(t, err)
		})
	}
}

func TestBackupRecoveryProducerCommitRejectsChangedEvidence(t *testing.T) {
	for _, mode := range []string{"ownership_intent", "ownership_record", "summary", "payload", "export", "missing_transfer", "producer_cursor", "source_command_ledger", "worker_cursor", "worker_identity", "worker_issued", "worker_command_hash"} {
		t.Run(mode, func(t *testing.T) {
			f, session, _ := backupCommitFixture(t)
			var err error
			switch mode {
			case "ownership_intent":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_ownership_intent'")
			case "ownership_record":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_ownership'")
			case "summary":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_summary'")
			case "payload":
				_, err = f.state.db.Exec("UPDATE backup_notification_recovery_records SET hash='changed'")
			case "export":
				_, err = f.state.db.Exec("UPDATE notification_exports SET body=json_set(body,'$.text','changed')")
			case "missing_transfer":
				_, err = f.state.db.Exec("DELETE FROM backup_notification_transfers WHERE submitted=0")
			case "producer_cursor":
				_, err = f.state.db.Exec("UPDATE notification_producer SET cursor=cursor+1")
			case "source_command_ledger":
				_, err = f.state.db.Exec("DELETE FROM interactive_recovery_completion")
			}
			testutil.Must(t, err)
			altered := interruptedProducerSession{RecoveryProducerSession: session, after: func(v *notify.RecoveryProducerCommit) error {
				switch mode {
				case "worker_cursor":
					v.Cursor.Position = 1
				case "worker_identity":
					v.Cursor.Identity.Recovery = "changed"
				case "worker_issued":
					v.HighestIssued++
				case "worker_command_hash":
					v.CommandSummaryHash = notify.Hash("changed")
				}
				return nil
			}}
			if _, err = commitBackupNotificationRecovery(context.Background(), f.state, f.config, altered); err == nil {
				t.Fatal("damaged evidence accepted", mode)
			}
			var count int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_commit'").Scan(&count))
			if count != 0 {
				t.Fatal("invalid commit saved")
			}
		})
	}
}

func TestBackupRecoveryProducerCommitCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_COMMIT_STATE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	state, err := openBackupState(path, false)
	testutil.Must(t, err)
	defer state.Close()
	var input recoveryExchangeInput
	raw, err := os.ReadFile(os.Getenv("DEPLOY_BACKUP_COMMIT_INPUT"))
	testutil.Must(t, err)
	testutil.Must(t, json.Unmarshal(raw, &input))
	session, err := worker.OpenRecoveryProducerSession(context.Background(), input.WorkerPath, input.WorkerConfig)
	testutil.Must(t, err)
	defer session.Close()
	_, err = commitBackupNotificationRecoveryWith(context.Background(), state, input.Config, session, func(stage string) error {
		if stage == os.Getenv("DEPLOY_BACKUP_COMMIT_BOUNDARY") {
			os.Stdout.WriteString("READY\n")
			select {}
		}
		return nil
	})
	testutil.Must(t, err)
	t.Fatal("crash boundary not reached")
}

func TestBackupRecoveryProducerCommitSIGKILL(t *testing.T) {
	for _, boundary := range []string{"producer_intent_transaction", "producer_intent_commit", "producer_worker_prepared", "producer_commit_transaction", "producer_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f, session, _ := backupCommitFixture(t)
			testutil.Must(t, session.Close())
			path := f.state.directory
			testutil.Must(t, f.state.Close())
			input := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(recoveryExchangeInput{Config: f.config, WorkerPath: f.gatewayPath, WorkerConfig: f.gatewayConfig})
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(input, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackupRecoveryProducerCommitCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_COMMIT_STATE="+path, "DEPLOY_BACKUP_COMMIT_INPUT="+input, "DEPLOY_BACKUP_COMMIT_BOUNDARY="+boundary)
			pipe, err := cmd.StdoutPipe()
			testutil.Must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			testutil.Must(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			scan := bufio.NewScanner(pipe)
			if !scan.Scan() || scan.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("child did not reach boundary", stderr.String())
			}
			testutil.Must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			f.state, err = openBackupState(path, false)
			testutil.Must(t, err)
			defer f.state.Close()
			var intents, records int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_commit_intent'").Scan(&intents))
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_commit'").Scan(&records))
			wantIntent, wantRecord := 1, 0
			if boundary == "producer_intent_transaction" {
				wantIntent = 0
			}
			if boundary == "producer_commit" {
				wantRecord = 1
			}
			if intents != wantIntent || records != wantRecord {
				t.Fatal("partial commit", intents, records)
			}
			resumed, err := worker.OpenRecoveryProducerSession(ctx, f.gatewayPath, f.gatewayConfig)
			testutil.Must(t, err)
			defer resumed.Close()
			got, err := commitBackupNotificationRecovery(ctx, f.state, f.config, resumed)
			testutil.Must(t, err)
			again, err := commitBackupNotificationRecovery(ctx, f.state, f.config, resumed)
			testutil.Must(t, err)
			if got != again {
				t.Fatal("restart changed mapping")
			}
			if err = transferBackupNotifications(ctx, f.state, f.client); err == nil {
				t.Fatal("recovery activated source")
			}
			testutil.Must(t, resumed.Close())
			if normal, err := worker.Open(f.gatewayPath, f.gatewayConfig, false); err == nil {
				normal.Close()
				t.Fatal("recovery activated worker")
			}
		})
	}
}

func TestBackupRecoveryProducerCommitRejectsPartialRollback(t *testing.T) {
	for _, mode := range []string{"intent", "record", "journal", "owner", "binding", "cursor", "identity", "submitted", "transfer", "command_completion"} {
		t.Run(mode, func(t *testing.T) {
			f, session, _ := backupCommitFixture(t)
			v, err := commitBackupNotificationRecovery(context.Background(), f.state, f.config, session)
			testutil.Must(t, err)
			switch mode {
			case "intent":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_commit_intent'")
			case "record":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_commit'")
			case "journal":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery'")
			case "owner":
				err = f.state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: "fenced", Binding: f.binding, Socket: f.client.Socket})
			case "binding":
				raw, e := json.Marshal(f.binding)
				testutil.Must(t, e)
				_, err = f.state.db.Exec("UPDATE backup_notification_binding SET body=?", string(raw))
			case "cursor":
				_, err = f.state.db.Exec("UPDATE notification_producer SET cursor=?", v.BeforeCursor.Position)
			case "identity":
				raw, e := json.Marshal(f.binding.Identity)
				testutil.Must(t, e)
				_, err = f.state.db.Exec("UPDATE notification_producer SET identity=?", string(raw))
			case "submitted":
				_, err = f.state.db.Exec("UPDATE notification_producer SET submitted=submitted-1")
			case "transfer":
				_, err = f.state.db.Exec("UPDATE backup_notification_transfers SET submitted=0")
			case "command_completion":
				_, err = f.state.db.Exec("DELETE FROM interactive_recovery_completion")
			}
			testutil.Must(t, err)
			if _, err = commitBackupNotificationRecovery(context.Background(), f.state, f.config, session); err == nil {
				t.Fatal("partial rollback accepted", mode)
			}
			if err = transferBackupNotifications(context.Background(), f.state, f.client); err == nil {
				t.Fatal("partial rollback lifted source fence")
			}
		})
	}
}

func TestBackupRecoveryProducerCommitRechecksAuthority(t *testing.T) {
	for _, boundary := range []string{"producer_intent_transaction", "producer_worker_prepared", "producer_commit_transaction"} {
		for _, loss := range []string{"cancel", "notifier.lock", "notification-recovery.lock", "ownership"} {
			if loss == "ownership" && boundary != "producer_worker_prepared" {
				continue
			}
			t.Run(boundary+"/"+loss, func(t *testing.T) {
				f, session, _ := backupCommitFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reached := false
				_, err := commitBackupNotificationRecoveryWith(ctx, f.state, f.config, session, func(at string) error {
					if at != boundary {
						return nil
					}
					reached = true
					if loss == "cancel" {
						cancel()
					} else if loss == "ownership" {
						// Only the between-call boundary permits an independent writer.
						if at == "producer_worker_prepared" {
							_, e := f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_ownership'")
							testutil.Must(t, e)
						} else {
							return errors.New("transaction interrupted")
						}
					} else {
						path := filepath.Join(f.state.directory, loss)
						testutil.Must(t, os.Rename(path, path+".held"))
						testutil.Must(t, os.WriteFile(path, nil, 0600))
					}
					return nil
				})
				if !reached || err == nil {
					t.Fatal("authority loss ignored", reached, err)
				}
				var n int
				testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_commit'").Scan(&n))
				if n != 0 {
					t.Fatal("committed without authority")
				}
			})
		}
	}
}

func TestBackupRecoveryProducerCommitDoesNotSkipNewOutcomes(t *testing.T) {
	f, session, summary := backupCommitFixture(t)
	ctx := context.Background()
	stages := session.(worker.RecoveryGenerationSession)
	var id string
	testutil.Must(t, f.state.db.QueryRow("SELECT id FROM notification_exports ORDER BY seq DESC LIMIT 1").Scan(&id))
	review, err := stages.ReviewRecoveryMessage(ctx, f.binding.Producer, id)
	testutil.Must(t, err)
	_, err = stages.DecideRecoveryMessage(ctx, worker.RecoveryDisposition{ID: review.RequestID, Operation: review.Operation, Identity: review.Identity, Producer: f.binding.Producer, MessageID: id, MessageHash: review.MessageHash, SummaryHash: review.SummaryHash, Action: "retry", Actor: "fixture-operator", Reason: "Reviewed duplicate delivery risk", AcceptDuplicateRisk: true})
	testutil.Must(t, err)
	_, err = stages.ApplyRecoveryMessage(ctx, f.binding.Producer, id)
	testutil.Must(t, err)
	_, err = stages.PrepareRecoveryGeneration(ctx, f.binding.Producer)
	testutil.Must(t, err)
	_, err = stages.BindRecoveryRetry(ctx, f.binding.Producer, id)
	testutil.Must(t, err)
	var pin string
	testutil.Must(t, f.state.db.QueryRow("SELECT body FROM interactive_identity").Scan(&pin))
	v, err := commitBackupNotificationRecovery(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	if v.Proof.Cursor.Position != 0 || v.BeforeCursor.Position == 0 {
		t.Fatal("old cursor reused")
	}
	var after string
	testutil.Must(t, f.state.db.QueryRow("SELECT body FROM interactive_identity").Scan(&after))
	if pin != after {
		t.Fatal("delivery commit changed command identity")
	}
	if _, err = interaction.OpenLedger(ctx, f.state.db, f.binding.Producer, v.After.Identity, false); err == nil {
		t.Fatal("new command epoch enabled")
	}
	testutil.Must(t, session.Close())
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.gatewayPath, "recovery", summary.Challenge.Operation, "reconciled.db")+"?mode=ro")
	testutil.Must(t, err)
	defer db.Close()
	var pending, acked, listed int
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM receipts WHERE producer=?", f.binding.Producer.ID).Scan(&pending))
	testutil.Must(t, db.QueryRow("SELECT acked,listed FROM producers WHERE id=?", f.binding.Producer.ID).Scan(&acked, &listed))
	if pending != 1 || acked != 0 || listed != 0 {
		t.Fatal("producer commit consumed unimported outcome")
	}
	var newHistory int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_receipt_history WHERE identity=?", notify.Hash(v.After.Identity)).Scan(&newHistory))
	if newHistory != 0 {
		t.Fatal("commit invented imported receipts")
	}
}

func TestBackupRecoveryDeliveryAndCommandCommitsReplayInEitherOrder(t *testing.T) {
	for _, commandsFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivery_first", true: "commands_first"}[commandsFirst], func(t *testing.T) {
			f, session, _ := backupCommitFixture(t)
			ctx := context.Background()
			ledger, err := interaction.OpenLedger(ctx, f.state.db, f.binding.Producer, f.binding.Identity, false)
			testutil.Must(t, err)
			commands := session.(interaction.CommandRecoveryCommitSession)
			if commandsFirst {
				_, err = interaction.CommitCommandRecovery(ctx, ledger, commands)
				testutil.Must(t, err)
			}
			delivery, err := commitBackupNotificationRecovery(ctx, f.state, f.config, session)
			testutil.Must(t, err)
			command, err := interaction.CommitCommandRecovery(ctx, ledger, commands)
			testutil.Must(t, err)
			again, err := commitBackupNotificationRecovery(ctx, f.state, f.config, session)
			testutil.Must(t, err)
			if again != delivery || command.Summary.Challenge.Identity != delivery.After.Identity {
				t.Fatal("recovery commits disagree")
			}
			current, err := interaction.OpenLedger(ctx, f.state.db, f.binding.Producer, delivery.After.Identity, false)
			testutil.Must(t, err)
			repeated, err := interaction.CommitCommandRecovery(ctx, current, commands)
			testutil.Must(t, err)
			if notify.Hash(repeated) != notify.Hash(command) {
				t.Fatal("command proof changed after delivery commit")
			}
			testutil.Must(t, session.Close())
			installer, err := worker.OpenCommandRecoveryInstallSession(ctx, f.gatewayPath, f.gatewayConfig)
			testutil.Must(t, err)
			defer installer.Close()
			installed, err := interaction.InstallCommandRecovery(ctx, current, installer)
			testutil.Must(t, err)
			if installed.Commands != 0 || installed.ExecutionEnabled {
				t.Fatal(installed)
			}
			replay, err := interaction.InstallCommandRecovery(ctx, current, installer)
			testutil.Must(t, err)
			if notify.Hash(installed) != notify.Hash(replay) {
				t.Fatal("empty installation changed")
			}
			again, err = commitBackupNotificationRecovery(ctx, f.state, f.config, installer.(notify.RecoveryProducerSession))
			testutil.Must(t, err)
			if again != delivery {
				t.Fatal("outcome installation changed delivery commit")
			}

			if _, err = current.PendingResults(ctx); err == nil {
				t.Fatal("command results enabled")
			}
			if err = transferBackupNotifications(ctx, f.state, f.client); err == nil {
				t.Fatal("delivery enabled")
			}
		})
	}
}
