package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
)

type epochCrashInput struct {
	Config      Config
	Point, Hash string
}

func TestRecoveryLEDEpochCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_EPOCH_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(filepath.Join(path, "test-epoch-input.json"))
	must(t, err)
	var input epochCrashInput
	must(t, json.Unmarshal(raw, &input))
	r, err := openRecoveryReconciler(context.Background(), path, input.Config, nil)
	must(t, err)
	defer r.Close()
	r.fault = func(at string) error {
		if at == input.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err = r.handoffLED(context.Background(), migrationSocket, input.Hash)
	t.Fatal("crash boundary missed", err)
}

func TestRecoveryLEDEpochSIGKILL(t *testing.T) {
	for _, point := range []string{"led_epoch_intent", "led_epoch_archive", "led_epoch_reset", "led_epoch_transaction", "led_epoch_commit"} {
		t.Run(point, func(t *testing.T) {
			r, path, c, _, hash := epochFixture(t, true)
			raw, err := json.Marshal(epochCrashInput{c, point, hash})
			must(t, err)
			must(t, os.WriteFile(filepath.Join(path, "test-epoch-input.json"), raw, 0600))
			must(t, r.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryLEDEpochCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_LED_EPOCH_CRASH="+path)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected child exit", cmd.ProcessState, string(out))
			}
			r, err = openRecoveryReconciler(context.Background(), path, c, nil)
			must(t, err)
			defer r.Close()
			var intent recoveryLEDEpoch
			must(t, r.stage.ReadJSON(ledEpochFile, &intent))
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_led_epoch").Scan(&count))
			if (count == 1) != (point == "led_epoch_commit") {
				t.Fatal("non-atomic archive", count, point)
			}
			v, err := r.handoffLED(context.Background(), migrationSocket, hash)
			must(t, err)
			if v.RecordHash != protocol.Hash(intent) || !v.PreparedAt.Equal(intent.PreparedAt) {
				t.Fatal("crash replaced intent", v)
			}
			if active, err := Open(path, c, false); err == nil {
				active.Close()
				t.Fatal("crash released quarantine")
			}
		})
	}
}
