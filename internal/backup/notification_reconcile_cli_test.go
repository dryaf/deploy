//go:build linux

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/ui"
)

func backupReconciliationCLIFixture(t *testing.T, configure ...func(*worker.Config)) (*backupEvaluationFixture, backupReconciliationOptions) {
	t.Helper()
	f, session := backupReconciliationFixture(t, true, configure...)
	ch, err := session.Challenge(context.Background(), f.binding.Producer)
	testutil.Must(t, err)
	testutil.Must(t, session.Close())
	return f, backupReconciliationOptions{State: f.state.directory, NotificationState: f.gatewayPath, Operation: ch.Operation}
}

func backupReconciliationCLIArguments(o backupReconciliationOptions) []string {
	return []string{"--local", "--config", o.Config, "--state-dir", o.State, "--notification-config", o.NotificationConfig, "--notification-state", o.NotificationState, "--operation", o.Operation}
}

func TestBackupReconciliationCLIExchangeAndRetry(t *testing.T) {
	f, o := backupReconciliationCLIFixture(t)
	var out bytes.Buffer
	testutil.Must(t, runBackupReconciliation(context.Background(), o, f.config, f.gatewayConfig, &out, nil))
	var summary notify.RecoverySummary
	testutil.Must(t, json.Unmarshal(out.Bytes(), &summary))
	if summary.Challenge.Operation != o.Operation || summary.Manifest.HighestIssued != 2 || summary.Ambiguous != 1 {
		t.Fatal(summary)
	}
	assertRecoveryStillFenced(t, f, summary)
	// Historical retry cannot renew the evaluation or replay delivery, even if
	// domain state changed since completion.
	testutil.Must(t, f.state.put("paused", true))
	var retry bytes.Buffer
	testutil.Must(t, runBackupReconciliation(context.Background(), o, f.config, f.gatewayConfig, &retry, nil))
	if !bytes.Equal(out.Bytes(), retry.Bytes()) {
		t.Fatal("retry changed the completed summary")
	}
	o.Operation, _ = safefs.NewID("n")
	if err := runBackupReconciliation(context.Background(), o, f.config, f.gatewayConfig, io.Discard, nil); err == nil {
		t.Fatal("completed exchange accepted another operation")
	}
}

func TestBackupReconciliationCLIFailsBeforeProducerFence(t *testing.T) {
	for _, kind := range []string{"operation", "configuration", "enrollment", "worker-lease", "source-lease", "missing-source", "missing-gateway", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f, o := backupReconciliationCLIFixture(t)
			c, cfg := f.config, f.gatewayConfig
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "operation":
				o.Operation, _ = safefs.NewID("n")
			case "configuration":
				c.ConfigHash = notify.Hash("wrong config")
			case "enrollment":
				cfg.Producers = nil
			case "worker-lease":
				s, err := worker.OpenRecoverySession(ctx, f.gatewayPath, cfg)
				testutil.Must(t, err)
				defer s.Close()
			case "source-lease":
				d, err := safefs.Open(f.state.directory)
				testutil.Must(t, err)
				defer d.Close()
				lease, err := d.Lock("notification-recovery.lock", true, true)
				testutil.Must(t, err)
				defer lease.Close()
			case "missing-source":
				o.State = filepath.Join(t.TempDir(), "absent")
			case "missing-gateway":
				o.NotificationState = filepath.Join(t.TempDir(), "absent")
			case "cancelled":
				cancel()
			}
			if err := runBackupReconciliation(ctx, o, c, cfg, io.Discard, nil); err == nil {
				t.Fatal("invalid scope accepted")
			}
			var owner backupNotificationOwner
			testutil.Must(t, f.state.get("notification_owner", &owner))
			if owner.Mode != "shared" {
				t.Fatal("failure changed producer ownership", owner)
			}
			var count int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM meta WHERE key IN ('notification_recovery','notification_recovery_summary')").Scan(&count))
			if count != 0 {
				t.Fatal("failure persisted producer evidence")
			}
			if kind == "missing-source" || kind == "missing-gateway" {
				path := o.State
				if kind == "missing-gateway" {
					path = o.NotificationState
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("created missing state", err)
				}
			}
		})
	}
}

func TestBackupReconciliationCLIFlagsAndPrivateConfig(t *testing.T) {
	f, o := backupReconciliationCLIFixture(t)
	o.Config = writeBackupLEDCLIInstallation(t, f.config)
	o.NotificationConfig, _ = writeBackupLEDCLIRuntime(t, f.gatewayConfig, "")
	o.State, o.NotificationState = filepath.Join(t.TempDir(), "absent-source"), filepath.Join(t.TempDir(), "absent-gateway")
	prior := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = prior }()
	args := backupReconciliationCLIArguments(o)
	var out bytes.Buffer
	testutil.Must(t, runBackupReconciliationCLI(args, &out))
	testutil.Must(t, RunCLI(append([]string{"recovery-notifications"}, args...)))
	if !bytes.Contains(out.Bytes(), []byte("no state opened")) {
		t.Fatal(out.String())
	}
	for _, bad := range [][]string{{"--local=false"}, {"--operation", ""}, {"--operation", "unreviewed"}, {"--config", "relative"}, {"--notification-state", o.State}, {"--notification-state", o.State + "/nested"}, {"--refresh"}, {"--activate"}, {"positional"}} {
		if err := runBackupReconciliationCLI(append(append([]string{}, args...), bad...), io.Discard); err == nil {
			t.Fatal("invalid option accepted", bad)
		}
	}
	testutil.Must(t, os.Chmod(o.Config, 0644))
	if err := runBackupReconciliationCLI(args, io.Discard); err == nil {
		t.Fatal("public installation accepted")
	}
	for _, path := range []string{o.State, o.NotificationState} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("dry-run initialized state", err)
		}
	}
}

type backupReconciliationCLICrashInput struct {
	Options backupReconciliationOptions
	Config  WorkerInstallation
	Gateway worker.Config
	Point   string
}

func TestBackupReconciliationCLIHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_RECONCILIATION_CLI_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	testutil.Must(t, err)
	var input backupReconciliationCLICrashInput
	testutil.Must(t, json.Unmarshal(raw, &input))
	err = runBackupReconciliation(context.Background(), input.Options, input.Config, input.Gateway, io.Discard, func(at string) error {
		if at == input.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash boundary missed", err)
}

func TestBackupReconciliationCLISIGKILL(t *testing.T) {
	for _, point := range []string{"recovery_summary_transaction", "recovery_summary_commit", "recovery_cli_result"} {
		t.Run(point, func(t *testing.T) {
			f, o := backupReconciliationCLIFixture(t)
			raw, err := json.Marshal(backupReconciliationCLICrashInput{o, f.config, f.gatewayConfig, point})
			testutil.Must(t, err)
			path := filepath.Join(t.TempDir(), "input.json")
			testutil.Must(t, os.WriteFile(path, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupReconciliationCLIHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_RECONCILIATION_CLI_CRASH="+path)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", cmd.ProcessState, string(out))
			}
			var result bytes.Buffer
			testutil.Must(t, runBackupReconciliation(context.Background(), o, f.config, f.gatewayConfig, &result, nil))
			var summary notify.RecoverySummary
			testutil.Must(t, json.Unmarshal(result.Bytes(), &summary))
			assertRecoveryStillFenced(t, f, summary)
			var replay bytes.Buffer
			testutil.Must(t, runBackupReconciliation(context.Background(), o, f.config, f.gatewayConfig, &replay, nil))
			if !bytes.Equal(result.Bytes(), replay.Bytes()) {
				t.Fatal("resumed completion changed")
			}
		})
	}
}
