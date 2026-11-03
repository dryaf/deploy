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

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/process"
)

type preservationCrashInput struct {
	Config                       Config
	Inventory, Hash, Point, Boot string
	Now                          time.Time
	Uptime                       float64
	States                       map[string]led.State
}

func TestRecoveryLEDPreservationCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_PRESERVATION_CRASH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	var input preservationCrashInput
	raw, err := os.ReadFile(filepath.Join(path, "test-preservation-input.json"))
	must(t, err)
	must(t, json.Unmarshal(raw, &input))
	r, err := openRecoveryReconciler(context.Background(), path, input.Config, nil)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return input.Now }
	r.clock = func() (string, float64, error) { return input.Boot, input.Uptime, nil }
	r.fault = func(point string) error {
		if point == input.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err = r.preserveLED(context.Background(), migrationSocket, input.Inventory, input.Hash, &fakeLED{states: input.States})
	t.Fatal("kill boundary was not reached", err)
}
func TestRecoveryLEDPreservationSIGKILL(t *testing.T) {
	for _, point := range []string{"led_preservation_intent", "led_preservation_transaction", "led_preservation_commit"} {
		t.Run(point, func(t *testing.T) {
			f, v := preservationFixture(t)
			input := preservationCrashInput{f.config, v.InventoryID, v.InventoryHash, point, f.boot, f.now, f.uptime, f.api.states}
			raw, err := json.Marshal(input)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(f.path, "test-preservation-input.json"), raw, 0600))
			must(t, f.r.Close())
			executable, err := os.Executable()
			must(t, err)
			cmd := exec.Command(executable, "-test.run=^TestRecoveryLEDPreservationCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_LED_PRESERVATION_CRASH="+f.path)
			output, err := process.LimitedOutput(cmd, 20*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(output))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong exit", cmd.ProcessState, string(output))
			}
			r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil)
			must(t, err)
			defer r.Close()
			r.now = func() time.Time { return f.now.Add(24 * time.Hour) }
			// Neither fresh registry access nor current-clock proof is needed to retain
			// the already sealed ownership provenance after the coordinator died.
			r.clock = func() (string, float64, error) { return "new-boot", 0, nil }
			record, _, err := r.readLEDPreservation(context.Background(), r.db)
			must(t, err)
			if record == nil {
				t.Fatal("durable intent missing")
			}
			view, err := r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, nil)
			must(t, err)
			if view.State != "committed" || view.Faults != 3 || view.PostSnapshotFaults != 1 || !view.PreparedAt.Equal(record.PreparedAt) || view.ActivationAllowed || view.ClearAllowed {
				t.Fatal(view)
			}
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_led_preservation").Scan(&count))
			if count != 1 {
				t.Fatal("duplicate preservation", count)
			}
		})
	}
}
