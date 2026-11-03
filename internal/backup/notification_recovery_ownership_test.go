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

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func backupOwnershipFixture(t *testing.T) (*backupEvaluationFixture, notify.RecoveryOwnershipSession, notify.RecoverySummary) {
	t.Helper()
	f, session := backupReconciliationFixture(t, true)
	summary, err := reconcileBackupNotifications(context.Background(), f.state, f.config, session)
	testutil.Must(t, err)
	return f, session.(notify.RecoveryOwnershipSession), summary
}

func TestBackupRecoveryOwnershipAfterAbsentProducerFence(t *testing.T) {
	f, session := backupReconciliationFixture(t, true)
	ctx := context.Background()
	testutil.Must(t, session.Close())
	fence, err := worker.FenceRecoveryOwnership(ctx, f.gatewayPath, f.gatewayConfig, f.binding.Producer)
	testutil.Must(t, err)
	if !fence.ReconciliationPending || fence.DeliveryEnabled {
		t.Fatal(fence)
	}
	resumed, err := worker.OpenRecoverySession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	defer resumed.Close()
	summary, err := reconcileBackupNotifications(ctx, f.state, f.config, resumed)
	testutil.Must(t, err)
	ownership := resumed.(notify.RecoveryOwnershipSession)
	lost := &interruptedOwnershipSession{RecoveryOwnershipSession: ownership, afterPrepare: func(*notify.RecoveryOwnership) error {
		return errors.New("lost completion after pending fence")
	}}
	if _, err = prepareBackupRecoveryOwnership(ctx, f.state, f.config, lost); err == nil {
		t.Fatal("lost acknowledgement ignored")
	}
	result, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, ownership)
	testutil.Must(t, err)
	if result.Ownership.PreviousHistory != fence.History || result.Ownership.PreviousRevision != fence.Revision ||
		result.Ownership.SummaryHash != notify.Hash(summary) || result.Ownership.DeliveryEnabled {
		t.Fatal(result)
	}
	again, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, ownership)
	testutil.Must(t, err)
	if again != result {
		t.Fatal("backup replay changed fence completion")
	}
	testutil.Must(t, resumed.Close())
	assertRecoveryStillFenced(t, f, summary)
	var open int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	if open != 1 {
		t.Fatal("fence recovery discharged backup delivery obligation", open)
	}
}

type interruptedOwnershipSession struct {
	notify.RecoveryOwnershipSession
	summaryError  error
	changeSummary func(*notify.RecoverySummary)
	afterPrepare  func(*notify.RecoveryOwnership) error
	calls         int
}

func (s *interruptedOwnershipSession) Summary(ctx context.Context, p notify.Producer) (notify.RecoverySummary, error) {
	if s.summaryError != nil {
		return notify.RecoverySummary{}, s.summaryError
	}
	v, err := s.RecoveryOwnershipSession.Summary(ctx, p)
	if err == nil && s.changeSummary != nil {
		s.changeSummary(&v)
	}
	return v, err
}

func (s *interruptedOwnershipSession) PrepareRecoveryOwnership(ctx context.Context, p notify.Producer) (notify.RecoveryOwnership, error) {
	s.calls++
	v, err := s.RecoveryOwnershipSession.PrepareRecoveryOwnership(ctx, p)
	if err == nil && s.afterPrepare != nil {
		err = s.afterPrepare(&v)
	}
	return v, err
}

func TestBackupRecoveryOwnershipRealHandoffAndLostResponse(t *testing.T) {
	f, session, summary := backupOwnershipFixture(t)
	ctx := context.Background()
	lost := &interruptedOwnershipSession{RecoveryOwnershipSession: session, afterPrepare: func(*notify.RecoveryOwnership) error {
		return errors.New("lost migration response")
	}}
	if _, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, lost); err == nil {
		t.Fatal("lost response ignored")
	}
	var records int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_ownership'").Scan(&records))
	if records != 0 {
		t.Fatal("failed response recorded as completion")
	}
	testutil.Must(t, session.Close())
	resumed, err := worker.OpenRecoveryOwnershipSession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	defer resumed.Close()
	got, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, resumed)
	testutil.Must(t, err)
	if got.Intent.Binding != f.binding || got.Ownership.Before != f.binding.Identity ||
		got.Ownership.After != summary.Challenge.Identity || got.Ownership.SummaryHash != notify.Hash(summary) {
		t.Fatal(got)
	}
	again, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, resumed)
	testutil.Must(t, err)
	if again != got {
		t.Fatal("replay changed completion")
	}
	// Release the offline lease so the ordinary-open assertion exercises the
	// durable quarantine, rather than merely failing on lock contention.
	testutil.Must(t, resumed.Close())
	generation, err := worker.OpenRecoveryGenerationSession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	defer generation.Close()
	_, err = generation.PrepareRecoveryGeneration(ctx, f.binding.Producer)
	testutil.Must(t, err)
	again, err = prepareBackupRecoveryOwnership(ctx, f.state, f.config, generation.(notify.RecoveryOwnershipSession))
	testutil.Must(t, err)
	if again != got {
		t.Fatal("generation staging invalidated backup's recorded handoff")
	}
	testutil.Must(t, generation.Close())
	assertRecoveryStillFenced(t, f, summary)
	var open int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	if open != 1 {
		t.Fatal("handoff changed delivery obligations", open)
	}
	// Source receipt provenance and the candidate's uncertain delivery survive.
	var receipts int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_receipt_history").Scan(&receipts))
	if receipts == 0 {
		t.Fatal("old receipt evidence lost")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.gatewayPath, "recovery", summary.Challenge.Operation, "reconciled.db")+"?mode=ro")
	testutil.Must(t, err)
	defer db.Close()
	var ambiguous, newReceipts int
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM messages WHERE state='ambiguous'").Scan(&ambiguous))
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM receipts").Scan(&newReceipts))
	if ambiguous != 1 || newReceipts != 0 {
		t.Fatal("handoff changed delivery evidence", ambiguous, newReceipts)
	}
}

func TestBackupRecoveryOwnershipRejectsUnavailableOrChangedSummary(t *testing.T) {
	for _, mode := range []string{"unavailable", "incomplete", "changed", "missing_local"} {
		t.Run(mode, func(t *testing.T) {
			f, session, _ := backupOwnershipFixture(t)
			broken := &interruptedOwnershipSession{RecoveryOwnershipSession: session}
			switch mode {
			case "unavailable":
				broken.summaryError = errors.New("worker unavailable")
			case "incomplete":
				broken.summaryError = notify.ErrRecoveryIncomplete
			case "changed":
				broken.changeSummary = func(v *notify.RecoverySummary) { v.Ambiguous++ }
			case "missing_local":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_summary'")
				testutil.Must(t, err)
			}
			if _, err := prepareBackupRecoveryOwnership(context.Background(), f.state, f.config, broken); err == nil {
				t.Fatal("invalid summary accepted")
			}
			var n int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key IN ('notification_recovery_ownership','notification_recovery_ownership_intent')").Scan(&n))
			if n != 0 || broken.calls != 0 {
				t.Fatal("ownership changed without matching summaries", n, broken.calls)
			}
		})
	}
}

func TestBackupRecoveryOwnershipRejectsChangedProof(t *testing.T) {
	changes := map[string]func(*notify.RecoveryOwnership){
		"epoch":      func(p *notify.RecoveryOwnership) { p.After.Recovery = "foreign" },
		"source":     func(p *notify.RecoveryOwnership) { p.Before.Recovery = "foreign" },
		"producer":   func(p *notify.RecoveryOwnership) { p.Producer.Epoch = "foreign" },
		"operation":  func(p *notify.RecoveryOwnership) { p.Operation, _ = safefs.NewID("n") },
		"summary":    func(p *notify.RecoveryOwnership) { p.SummaryHash = notify.Hash("changed") },
		"activation": func(p *notify.RecoveryOwnership) { p.DeliveryEnabled = true },
		"revision":   func(p *notify.RecoveryOwnership) { p.Revision++ },
		"overflow":   func(p *notify.RecoveryOwnership) { p.PreviousRevision, p.Revision = ^uint64(0), 0 },
		"baseline":   func(p *notify.RecoveryOwnership) { p.PreviousRevision, p.Revision = 0, 1 },
		"history":    func(p *notify.RecoveryOwnership) { p.History = "missing" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f, session, _ := backupOwnershipFixture(t)
			broken := &interruptedOwnershipSession{RecoveryOwnershipSession: session, afterPrepare: func(p *notify.RecoveryOwnership) error { change(p); return nil }}
			if _, err := prepareBackupRecoveryOwnership(context.Background(), f.state, f.config, broken); err == nil {
				t.Fatal("changed proof accepted")
			}
			var n int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_ownership'").Scan(&n))
			if n != 0 {
				t.Fatal("invalid proof persisted")
			}
			_, err := prepareBackupRecoveryOwnership(context.Background(), f.state, f.config, session)
			testutil.Must(t, err)
		})
	}
}

func TestBackupRecoveryOwnershipRejectsDamagedLocalEvidence(t *testing.T) {
	for _, mode := range []string{"missing_intent", "changed_intent", "changed_record", "missing_finish", "cursor", "config"} {
		t.Run(mode, func(t *testing.T) {
			f, session, _ := backupOwnershipFixture(t)
			ctx := context.Background()
			record, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, session)
			testutil.Must(t, err)
			switch mode {
			case "missing_intent":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_ownership_intent'")
			case "changed_intent":
				record.Intent.After.Recovery = "changed"
				err = f.state.put("notification_recovery_ownership_intent", record.Intent)
			case "changed_record":
				record.Ownership.History = notify.Hash("conflicting history")
				err = f.state.put("notification_recovery_ownership", record)
			case "missing_finish":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_finish'")
			case "cursor":
				_, err = f.state.db.Exec("UPDATE notification_producer SET cursor=cursor+1")
			case "config":
				f.config.ConfigHash = "changed"
			}
			testutil.Must(t, err)
			if _, err = prepareBackupRecoveryOwnership(ctx, f.state, f.config, session); err == nil {
				t.Fatal("damaged evidence accepted", mode)
			}
		})
	}
}

func TestBackupRecoveryOwnershipSourceChangesDuringWorkerCall(t *testing.T) {
	for _, mode := range []string{"summary", "cursor", "intent", "notifier.lock", "notification-recovery.lock"} {
		t.Run(mode, func(t *testing.T) {
			f, session, _ := backupOwnershipFixture(t)
			broken := &interruptedOwnershipSession{RecoveryOwnershipSession: session, afterPrepare: func(*notify.RecoveryOwnership) error {
				switch mode {
				case "summary":
					_, err := f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_summary'")
					return err
				case "cursor":
					_, err := f.state.db.Exec("UPDATE notification_producer SET cursor=cursor+1")
					return err
				case "intent":
					_, err := f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_ownership_intent'")
					return err
				default:
					path := filepath.Join(f.state.directory, mode)
					if err := os.Rename(path, path+".held"); err != nil {
						return err
					}
					return os.WriteFile(path, nil, 0600)
				}
			}}
			if _, err := prepareBackupRecoveryOwnership(context.Background(), f.state, f.config, broken); err == nil {
				t.Fatal("changed authority accepted")
			}
			var n int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_ownership'").Scan(&n))
			if n != 0 {
				t.Fatal("completion recorded after losing authority")
			}
		})
	}
}

func TestBackupRecoveryOwnershipRequiresExclusiveLiveSessions(t *testing.T) {
	for _, mode := range []string{"missing_notifier", "missing_coordinator", "busy_notifier", "busy_coordinator", "cancelled", "closed_worker"} {
		t.Run(mode, func(t *testing.T) {
			f, session, _ := backupOwnershipFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir, err := safefs.Open(f.state.directory)
			testutil.Must(t, err)
			defer dir.Close()
			name := "notifier.lock"
			if mode == "missing_coordinator" || mode == "busy_coordinator" {
				name = "notification-recovery.lock"
			}
			switch mode {
			case "missing_notifier", "missing_coordinator":
				testutil.Must(t, os.Remove(filepath.Join(f.state.directory, name)))
			case "busy_notifier", "busy_coordinator":
				lease, err := dir.Lock(name, true, false)
				testutil.Must(t, err)
				defer lease.Close()
			case "cancelled":
				cancel()
			case "closed_worker":
				testutil.Must(t, session.Close())
			}
			wrapped := &interruptedOwnershipSession{RecoveryOwnershipSession: session}
			if _, err = prepareBackupRecoveryOwnership(ctx, f.state, f.config, wrapped); err == nil {
				t.Fatal("missing authority accepted")
			}
			var n int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key IN ('notification_recovery_ownership','notification_recovery_ownership_intent')").Scan(&n))
			if n != 0 || wrapped.calls != 0 {
				t.Fatal("handoff proceeded without authority", n, wrapped.calls)
			}
		})
	}
}

func TestBackupRecoveryOwnershipCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_OWNERSHIP_STATE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := openBackupState(path, false)
	testutil.Must(t, err)
	defer s.Close()
	var input recoveryExchangeInput
	raw, err := os.ReadFile(os.Getenv("DEPLOY_BACKUP_OWNERSHIP_INPUT"))
	testutil.Must(t, err)
	testutil.Must(t, json.Unmarshal(raw, &input))
	session, err := worker.OpenRecoveryOwnershipSession(context.Background(), input.WorkerPath, input.WorkerConfig)
	testutil.Must(t, err)
	defer session.Close()
	_, err = prepareBackupRecoveryOwnershipWith(context.Background(), s, input.Config, session, func(stage string) error {
		if stage == os.Getenv("DEPLOY_BACKUP_OWNERSHIP_BOUNDARY") {
			os.Stdout.WriteString("READY\n")
			select {}
		}
		return nil
	})
	testutil.Must(t, err)
	t.Fatal("crash boundary not reached")
}

func TestBackupRecoveryOwnershipSIGKILL(t *testing.T) {
	for _, boundary := range []string{"ownership_intent_transaction", "ownership_intent_commit", "ownership_worker_prepared", "ownership_record_transaction", "ownership_record_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f, session, summary := backupOwnershipFixture(t)
			testutil.Must(t, session.Close())
			path := f.state.directory
			testutil.Must(t, f.state.Close())
			input := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(recoveryExchangeInput{Config: f.config, WorkerPath: f.gatewayPath, WorkerConfig: f.gatewayConfig})
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(input, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackupRecoveryOwnershipCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_OWNERSHIP_STATE="+path, "DEPLOY_BACKUP_OWNERSHIP_INPUT="+input, "DEPLOY_BACKUP_OWNERSHIP_BOUNDARY="+boundary)
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
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_ownership_intent'").Scan(&intents))
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_ownership'").Scan(&records))
			wantIntent, wantRecord := 1, 0
			if boundary == "ownership_intent_transaction" {
				wantIntent = 0
			}
			if boundary == "ownership_record_commit" {
				wantRecord = 1
			}
			if intents != wantIntent || records != wantRecord {
				t.Fatal("incorrect commit durability", intents, records)
			}
			resumed, err := worker.OpenRecoveryOwnershipSession(ctx, f.gatewayPath, f.gatewayConfig)
			testutil.Must(t, err)
			defer resumed.Close()
			got, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, resumed)
			testutil.Must(t, err)
			again, err := prepareBackupRecoveryOwnership(ctx, f.state, f.config, resumed)
			testutil.Must(t, err)
			if got != again {
				t.Fatal("restart changed completion")
			}
			testutil.Must(t, resumed.Close())
			assertRecoveryStillFenced(t, f, summary)
		})
	}
}
