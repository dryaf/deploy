//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
)

type backupLEDCLICrashInput struct {
	Operation, Point string
	Options          backupLEDRecoveryOptions
	Config           WorkerInstallation
	Gateway          worker.Config
	States           map[string]led.State
}

type countedBackupLEDRegistry struct {
	*backupRecoveryLEDRegistry
	path string
}

func (a countedBackupLEDRegistry) Clear(ctx context.Context, id string) error {
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(id + "\n")
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return a.backupRecoveryLEDRegistry.Clear(ctx, id)
}

func TestBackupLEDRecoveryCLIHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_LED_CLI_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(filepath.Join(path, "input.json"))
	testutil.Must(t, err)
	var input backupLEDCLICrashInput
	testutil.Must(t, json.Unmarshal(raw, &input))
	api := countedBackupLEDRegistry{&backupRecoveryLEDRegistry{states: input.States}, filepath.Join(path, "clears")}
	err = runBackupLEDRecoveryWith(context.Background(), input.Operation, input.Options, input.Config, input.Gateway, "/private/staged-led.sock", api, io.Discard, func(at string) error {
		if at == input.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash boundary missed", err)
}

func TestBackupLEDRecoveryCLISIGKILL(t *testing.T) {
	for _, point := range []string{"backup_led_evidence_result", "backup_led_cleanup_result", "backup_led_cleanup_import"} {
		t.Run(point, func(t *testing.T) {
			f, o := prepareBackupLEDCLI(t)
			op := "recovery-led-clear"
			if point == "backup_led_evidence_result" {
				op = "recovery-led-evidence"
			}
			path := t.TempDir()
			raw, err := json.Marshal(backupLEDCLICrashInput{op, point, o, f.config, f.gatewayConfig, f.registry.states})
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(filepath.Join(path, "input.json"), raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupLEDRecoveryCLIHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_LED_CLI_CRASH="+path)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", cmd.ProcessState, string(out))
			}
			api := countedBackupLEDRegistry{f.registry, filepath.Join(path, "clears")}
			testutil.Must(t, runBackupLEDRecovery(context.Background(), op, o, f.config, f.gatewayConfig, "/private/staged-led.sock", api, io.Discard))
			calls, err := os.ReadFile(filepath.Join(path, "clears"))
			if op == "recovery-led-evidence" {
				if !os.IsNotExist(err) {
					t.Fatal("evidence called Clear", err)
				}
			} else {
				testutil.Must(t, err)
				if string(calls) != o.Fault+"\n" {
					t.Fatal("lost response replayed clear", string(calls))
				}
			}
			view, err := worker.ReviewRecoveryLED(context.Background(), f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", nil, false)
			testutil.Must(t, err)
			if op == "recovery-led-clear" && len(view.CleanupImports) != 1 {
				t.Fatal("cleanup import not resumed", view.CleanupImports)
			}
		})
	}
}
