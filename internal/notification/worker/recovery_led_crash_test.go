package worker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestRecoveryLEDInventoryCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_INVENTORY_STATE")
	if path == "" {
		return
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var c Config
	must(t, d.ReadJSON("test-config.json", &c))
	api := &fakeLED{}
	must(t, d.ReadJSON("test-daemon.json", &api.states))
	r, err := openRecoveryReconciler(context.Background(), path, c, nil)
	must(t, err)
	defer r.Close()
	r.fault = func(stage string) error {
		if stage == os.Getenv("DEPLOY_LED_INVENTORY_PHASE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	_, err = r.reviewLEDInventory(context.Background(), migrationSocket, api, false)
	must(t, err)
	t.Fatal("kill boundary not reached")
}

func TestRecoveryLEDInventorySIGKILL(t *testing.T) {
	for _, phase := range []string{"led_inventory_schema", "led_inventory_intent", "led_inventory_archive", "led_inventory_before_commit", "led_inventory_committed"} {
		t.Run(phase, func(t *testing.T) {
			f := newLEDRecoveryFixture(t)
			before := f.r.journal.Before
			must(t, f.r.dir.WriteJSON("test-config.json", f.config, false))
			must(t, f.r.dir.WriteJSON("test-daemon.json", f.api.states, false))
			must(t, f.r.Close())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryLEDInventoryCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_LED_INVENTORY_STATE="+f.path, "DEPLOY_LED_INVENTORY_PHASE="+phase)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				cmd.Wait()
				t.Fatalf("boundary not reached: %s %s", scanner.Text(), stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			if s, err := Open(f.path, f.config, false); err == nil {
				s.Close()
				t.Fatal("interrupted review admitted worker")
			}
			r, err := openRecoveryReconciler(ctx, f.path, f.config, nil)
			must(t, err)
			defer r.Close()
			var head recoveryLEDInventory
			err = r.stage.ReadJSON(recoveryLEDHead, &head)
			if phase != "led_inventory_schema" {
				must(t, err)
				// Both reboot and expiry must remain visible after sealing a pending
				// record. Refresh cannot silently replace the interrupted observation.
				r.now = func() time.Time { return head.ValidUntil.Add(time.Minute) }
				r.clock = func() (string, float64, error) { return "rebooted", 1, nil }
			}
			var observer LEDRecoveryObserver
			if phase == "led_inventory_schema" {
				observer = f.api
			}
			pendingRefresh := phase == "led_inventory_intent" || phase == "led_inventory_archive" || phase == "led_inventory_before_commit"
			v, err := r.reviewLEDInventory(ctx, migrationSocket, observer, pendingRefresh)
			must(t, err)
			if phase != "led_inventory_schema" && (v.InventoryHash != protocol.Hash(head) || v.ObservationCurrent || v.Sequence != 1) {
				t.Fatal("restart renewed observation", v)
			}
			if v.ClearAllowed || v.ActivationAllowed || !v.Quarantined {
				t.Fatal("review granted mutation authority")
			}
			set, err := r.readLEDInventorySet(ctx, r.db)
			must(t, err)
			if set.Pending || len(set.Records) != 1 || set.Head.Recovery.Before != before {
				t.Fatal("capture was not sealed exactly once")
			}
			for _, source := range []recoveryLEDSource{set.Head.Sources.Snapshot, set.Head.Sources.Latest} {
				for _, fault := range source.Faults {
					must(t, validateLEDFaultIdentity(fault.ID, fault.Binding, fault.Identity, before, source.Layout.Legacy))
				}
			}
		})
	}
}
