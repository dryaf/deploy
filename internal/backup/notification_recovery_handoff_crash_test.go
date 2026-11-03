//go:build linux

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
)

type handoffCrashInput struct {
	Options backupReconciliationOptions
	Config  WorkerInstallation
	Gateway worker.Config
	Point   string
}

func TestBackupRecoveryHandoffCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_HANDOFF_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	testutil.Must(t, err)
	var in handoffCrashInput
	testutil.Must(t, json.Unmarshal(raw, &in))
	err = runBackupRecoveryHandoff(context.Background(), in.Options, in.Config, in.Gateway, io.Discard, func(at string) error {
		if at == in.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash boundary missed", err)
}

func TestBackupRecoveryHandoffSIGKILL(t *testing.T) {
	for _, point := range []string{"handoff_commands_reconciled", "ownership_intent_commit", "handoff_ownership", "producer_intent_commit", "producer_worker_prepared", "producer_commit_transaction", "handoff_delivery_commit", "handoff_command_commit", "handoff_outcomes_installed", "handoff_result"} {
		t.Run(point, func(t *testing.T) {
			f := backupHandoffTestFixture(t, true)
			path := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(handoffCrashInput{f.options, f.config, f.gatewayConfig, point})
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(path, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupRecoveryHandoffCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_HANDOFF_CRASH="+path)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", cmd.ProcessState, string(out))
			}
			var first, replay bytes.Buffer
			testutil.Must(t, runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, &first, nil))
			testutil.Must(t, runBackupRecoveryHandoff(context.Background(), f.options, f.config, f.gatewayConfig, &replay, nil))
			if !bytes.Equal(first.Bytes(), replay.Bytes()) {
				t.Fatal("resumed handoff changed")
			}
			var result backupRecoveryHandoffResult
			testutil.Must(t, json.Unmarshal(first.Bytes(), &result))
			assertBackupHandoff(t, f, result, true)
		})
	}
}
