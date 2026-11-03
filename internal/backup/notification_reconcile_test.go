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
	"github.com/dryaf/deploy/internal/testutil"
)

func backupReconciliationFixture(t *testing.T, messages bool, configure ...func(*worker.Config)) (*backupEvaluationFixture, notify.RecoverySession) {
	t.Helper()
	f := newBackupEvaluationFixture(t, configure...)
	f.clock = reportHostClock()
	f.refreshProbe(t)
	testutil.Must(t, f.state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: "shared", Binding: f.binding, Socket: f.client.Socket}))
	if messages {
		f.observe(t, f.subject, "First failure", f.clock.Now)
	}
	_, exports := f.evaluate(t)
	f.transfer(t, f.client)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err := f.gateway.Snapshot(context.Background(), artifact)
	testutil.Must(t, err)
	if messages {
		if len(exports) != 1 {
			t.Fatal(exports)
		}
		f.accept(t, exports[0])
		f.transfer(t, f.client)
		f.clock = reportHostClock()
		f.observe(t, "another", "Unsent failure", f.clock.Now)
		_, exports = f.evaluate(t)
		if len(exports) != 1 {
			t.Fatal(exports)
		}
	}
	f.stopGateway()
	testutil.Must(t, worker.PrepareRecoveryQuarantine(context.Background(), f.gatewayPath, artifact, f.gatewayConfig))
	session, err := worker.OpenRecoverySession(context.Background(), f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	t.Cleanup(func() { session.Close() })
	return f, session
}

func assertRecoveryStillFenced(t *testing.T, f *backupEvaluationFixture, summary notify.RecoverySummary) {
	t.Helper()
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		owner, err := readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if owner.Mode != "fenced" || owner.Binding != f.binding {
			t.Fatal("completion changed active ownership", owner)
		}
		var issued uint64
		var cursor int64
		if err = tx.QueryRow("SELECT submitted,cursor FROM notification_producer").Scan(&issued, &cursor); err != nil {
			return err
		}
		if issued != summary.Manifest.HighestIssued || cursor != summary.Manifest.ImportedReceipts.Position {
			t.Fatal("completion changed delivery counters")
		}
		return nil
	})
	if summary.DeliveryEnabled {
		t.Fatal("delivery was activated")
	}
	if store, err := worker.Open(f.gatewayPath, f.gatewayConfig, false); err == nil {
		store.Close()
		t.Fatal("quarantined worker opened")
	}
	if err := requireLegacyNotificationOwner(f.state); err == nil {
		t.Fatal("legacy notifications activated")
	}
	if err := transferBackupNotifications(context.Background(), f.state, f.client); err == nil {
		t.Fatal("ordinary pump ran during recovery")
	}
}

func TestBackupRecoveryExchangeRealQuarantinedWorker(t *testing.T) {
	f, session := backupReconciliationFixture(t, true)
	ctx := context.Background()
	summary, err := reconcileBackupNotifications(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	if summary.Manifest.HighestIssued != 2 || summary.Manifest.HighestAcknowledged != 1 || summary.Ambiguous != 1 || summary.Snapshot.Sequence != 3 {
		t.Fatal(summary)
	}
	assertRecoveryStillFenced(t, f, summary)
	// Inspect the actual candidate: acceptance imported after the snapshot must
	// suppress the queued copy, while reconstructed work remains ambiguous.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.gatewayPath, "recovery", summary.Challenge.Operation, "reconciled.db")+"?mode=ro")
	testutil.Must(t, err)
	defer db.Close()
	var accepted, ambiguous, receipts int
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM messages WHERE state='provider_accepted'").Scan(&accepted))
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM messages WHERE state='ambiguous'").Scan(&ambiguous))
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
	var published uint64
	testutil.Must(t, db.QueryRow("SELECT seq FROM status WHERE producer=?", f.binding.Producer.ID).Scan(&published))
	if accepted != 1 || ambiguous != 1 || receipts != 0 || published != 1 {
		t.Fatal("reconciliation published or lost evidence", accepted, ambiguous, receipts, published)
	}
	var open int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	if open != 1 {
		t.Fatal("outstanding obligation lost", open)
	}
	got, err := reconcileBackupNotifications(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	if notify.Hash(got) != notify.Hash(summary) {
		t.Fatal("completion replay changed")
	}
}

type interruptedRecoverySession struct {
	notify.RecoverySession
	operation    string
	lost         bool
	summaryError error
	change       func(*notify.RecoverySummary)
}

func (s *interruptedRecoverySession) fail(operation string) error {
	if s.operation == operation && !s.lost {
		s.lost = true
		return errors.New("fixture lost committed response")
	}
	return nil
}
func (s *interruptedRecoverySession) Summary(ctx context.Context, p notify.Producer) (notify.RecoverySummary, error) {
	if s.summaryError != nil {
		return notify.RecoverySummary{}, s.summaryError
	}
	v, err := s.RecoverySession.Summary(ctx, p)
	if err == nil && s.change != nil {
		s.change(&v)
	}
	return v, err
}
func (s *interruptedRecoverySession) Begin(ctx context.Context, ch notify.RecoveryChallenge, m notify.RecoveryManifest) error {
	if err := s.RecoverySession.Begin(ctx, ch, m); err != nil {
		return err
	}
	return s.fail("begin")
}
func (s *interruptedRecoverySession) Import(ctx context.Context, ch notify.RecoveryChallenge, records []notify.RecoveryRecord) error {
	if err := s.RecoverySession.Import(ctx, ch, records); err != nil {
		return err
	}
	return s.fail("import")
}
func (s *interruptedRecoverySession) Finish(ctx context.Context, ch notify.RecoveryChallenge, snap notify.Snapshot) (notify.RecoverySummary, error) {
	v, err := s.RecoverySession.Finish(ctx, ch, snap)
	if err == nil {
		err = s.fail("finish")
	}
	if err == nil && s.change != nil {
		s.change(&v)
	}
	return v, err
}

func TestBackupRecoveryExchangeLostResponses(t *testing.T) {
	for _, op := range []string{"begin", "import", "finish"} {
		t.Run(op, func(t *testing.T) {
			f, session := backupReconciliationFixture(t, true)
			broken := &interruptedRecoverySession{RecoverySession: session, operation: op}
			if _, err := reconcileBackupNotifications(context.Background(), f.state, f.config, broken); err == nil {
				t.Fatal("lost response not surfaced")
			}
			var before uint64
			testutil.Must(t, f.state.db.QueryRow("SELECT max(sequence) FROM backup_notification_snapshots").Scan(&before))
			// Reopening the offline session proves replay does not rely on memory.
			testutil.Must(t, session.Close())
			resumed, err := worker.OpenRecoverySession(context.Background(), f.gatewayPath, f.gatewayConfig)
			testutil.Must(t, err)
			defer resumed.Close()
			summary, err := reconcileBackupNotifications(context.Background(), f.state, f.config, resumed)
			testutil.Must(t, err)
			if op == "finish" && summary.Snapshot.Sequence != before {
				t.Fatal("lost completion was re-evaluated")
			}
			assertRecoveryStillFenced(t, f, summary)
		})
	}
}

func TestBackupRecoveryExchangeNoEvidenceOnUnavailableSummary(t *testing.T) {
	f, session := backupReconciliationFixture(t, false)
	broken := &interruptedRecoverySession{RecoverySession: session, summaryError: errors.New("fixture storage failure")}
	if _, err := reconcileBackupNotifications(context.Background(), f.state, f.config, broken); err == nil {
		t.Fatal("unavailable status treated as absent")
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error { return requireNotificationRecoveryInactive(tx) })
	bad := f.config
	bad.ConfigHash = "changed"
	if _, err := reconcileBackupNotifications(context.Background(), f.state, bad, session); err == nil {
		t.Fatal("wrong configuration accepted")
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error { return requireNotificationRecoveryInactive(tx) })
}

func TestBackupRecoveryExchangeRejectsChangedCompletions(t *testing.T) {
	cases := map[string]func(*notify.RecoverySummary){
		"activation": func(s *notify.RecoverySummary) { s.DeliveryEnabled = true },
		"nonce":      func(s *notify.RecoverySummary) { s.Challenge.Nonce = "changed" },
		"epoch":      func(s *notify.RecoverySummary) { s.Challenge.Identity.Recovery = "changed" },
		"manifest":   func(s *notify.RecoverySummary) { s.Manifest.HighestIssued++ },
		"snapshot":   func(s *notify.RecoverySummary) { s.Snapshot.Sequence++ },
		"unknown":    func(s *notify.RecoverySummary) { s.UnknownLast = 1 },
		"ambiguity":  func(s *notify.RecoverySummary) { s.Ambiguous = -1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, session := backupReconciliationFixture(t, false)
			broken := &interruptedRecoverySession{RecoverySession: session, change: change}
			if _, err := reconcileBackupNotifications(context.Background(), f.state, f.config, broken); err == nil {
				t.Fatal("changed completion accepted")
			}
			var count int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_summary'").Scan(&count))
			if count != 0 {
				t.Fatal("bad summary committed")
			}
			summary, err := reconcileBackupNotifications(context.Background(), f.state, f.config, session)
			testutil.Must(t, err)
			assertRecoveryStillFenced(t, f, summary)
		})
	}
}

func TestBackupRecoveryExchangeCompletionDoesNotRenewHealth(t *testing.T) {
	f, session := backupReconciliationFixture(t, false)
	broken := &interruptedRecoverySession{RecoverySession: session, operation: "finish"}
	if _, err := reconcileBackupNotifications(context.Background(), f.state, f.config, broken); err == nil {
		t.Fatal("expected lost response")
	}
	summary, err := reconcileBackupNotificationsWith(context.Background(), f.state, f.config, session, func() backupEvaluationClock {
		// Completed evidence must not cause fresh health evaluation or be renewed.
		t.Fatal("completion replay requested a fresh clock")
		return backupEvaluationClock{}
	}, nil)
	testutil.Must(t, err)
	if summary.Manifest.HighestIssued != 0 || summary.Snapshot.Sequence != 2 {
		t.Fatal(summary)
	}
	assertRecoveryStillFenced(t, f, summary)
	broken = &interruptedRecoverySession{RecoverySession: session, summaryError: notify.ErrRecoveryIncomplete}
	if _, err = reconcileBackupNotifications(context.Background(), f.state, f.config, broken); err == nil {
		t.Fatal("lost gateway completion silently recreated")
	}
}

func TestBackupRecoveryFreshEvaluationDoesNotExport(t *testing.T) {
	f, session := backupReconciliationFixture(t, false)
	ctx := context.Background()
	ch, err := session.Challenge(ctx, f.binding.Producer)
	testutil.Must(t, err)
	_, err = beginBackupNotificationRecovery(ctx, f.state, ch)
	testutil.Must(t, err)
	f.observe(t, f.subject, "Failure after evidence capture", time.Now().UTC())
	intent, err := prepareBackupRecoveryFinish(ctx, f.state, f.config, ch, reportHostClock(), nil)
	testutil.Must(t, err)
	if intent.Snapshot.Subjects[0].Condition != "critical" {
		t.Fatal("stale healthy snapshot reused", intent)
	}
	var issued uint64
	var pending int
	testutil.Must(t, f.state.db.QueryRow("SELECT submitted FROM notification_producer").Scan(&issued))
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM outbox WHERE accepted IS NULL").Scan(&pending))
	if issued != 0 || pending == 0 {
		t.Fatal("fresh domain change was exported or lost", issued, pending)
	}
	err = backupTransferTx(ctx, f.state, func(tx *sql.Tx) error {
		_, _, err := evaluateBackupNotificationPass(tx, f.config, reportHostClock())
		return err
	})
	if err == nil {
		t.Fatal("ordinary evaluator ran while fenced")
	}
	var seq uint64
	testutil.Must(t, f.state.db.QueryRow("SELECT max(sequence) FROM backup_notification_snapshots").Scan(&seq))
	if seq != intent.Snapshot.Sequence {
		t.Fatal("failed normal evaluation committed")
	}
}

type recoveryExchangeInput struct {
	Config       WorkerInstallation
	WorkerPath   string
	WorkerConfig worker.Config
}

func TestBackupRecoveryExchangeCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_EXCHANGE_STATE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := openBackupState(path, false)
	testutil.Must(t, err)
	defer s.Close()
	var input recoveryExchangeInput
	raw, err := os.ReadFile(os.Getenv("DEPLOY_BACKUP_EXCHANGE_INPUT"))
	testutil.Must(t, err)
	testutil.Must(t, json.Unmarshal(raw, &input))
	session, err := worker.OpenRecoverySession(context.Background(), input.WorkerPath, input.WorkerConfig)
	testutil.Must(t, err)
	defer session.Close()
	_, err = reconcileBackupNotificationsWith(context.Background(), s, input.Config, session, reportHostClock, func(stage string) error {
		if stage == os.Getenv("DEPLOY_BACKUP_EXCHANGE_BOUNDARY") {
			os.Stdout.WriteString("READY\n")
			select {}
		}
		return nil
	})
	testutil.Must(t, err)
	t.Fatal("crash boundary not reached")
}

func TestBackupRecoveryExchangeSIGKILL(t *testing.T) {
	for _, boundary := range []string{"recovery_finish_intent_transaction", "recovery_finish_intent_commit", "recovery_summary_transaction", "recovery_summary_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f, session := backupReconciliationFixture(t, true)
			testutil.Must(t, session.Close())
			path := f.state.directory
			testutil.Must(t, f.state.Close())
			input := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(recoveryExchangeInput{Config: f.config, WorkerPath: f.gatewayPath, WorkerConfig: f.gatewayConfig})
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(input, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackupRecoveryExchangeCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_EXCHANGE_STATE="+path, "DEPLOY_BACKUP_EXCHANGE_INPUT="+input, "DEPLOY_BACKUP_EXCHANGE_BOUNDARY="+boundary)
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
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait() // Join stderr copying before inspecting its buffer.
				t.Fatal("child did not reach boundary", scanner.Text(), stderr.String())
			}
			testutil.Must(t, cmd.Process.Kill())
			if err = cmd.Wait(); err == nil {
				t.Fatal("expected SIGKILL")
			}
			f.state, err = openBackupState(path, false)
			testutil.Must(t, err)
			defer f.state.Close()
			var intents, completions int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_finish'").Scan(&intents))
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_summary'").Scan(&completions))
			wantIntent, wantCompletion := 1, 0
			if boundary == "recovery_finish_intent_transaction" {
				wantIntent = 0
			}
			if boundary == "recovery_summary_commit" {
				wantCompletion = 1
			}
			if intents != wantIntent || completions != wantCompletion {
				t.Fatal("incorrect crash durability", intents, completions)
			}
			resumed, err := worker.OpenRecoverySession(context.Background(), f.gatewayPath, f.gatewayConfig)
			testutil.Must(t, err)
			defer resumed.Close()
			summary, err := reconcileBackupNotifications(context.Background(), f.state, f.config, resumed)
			testutil.Must(t, err)
			assertRecoveryStillFenced(t, f, summary)
		})
	}
}
