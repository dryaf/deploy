package worker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type ledCleanupCrashInput struct {
	Config                  Config
	Inventory, Evidence, ID string
	States                  map[string]led.State
	Now                     time.Time
}

func TestRecoveryLEDCleanupCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_CLEANUP_STATE")
	if path == "" {
		return
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var input ledCleanupCrashInput
	must(t, d.ReadJSON("test-led-cleanup.json", &input))
	r, err := openRecoveryReconciler(context.Background(), path, input.Config, nil)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return input.Now }
	r.clock = func() (string, float64, error) { return "boot-one", 100, nil }
	r.fault = func(stage string) error {
		if stage == os.Getenv("DEPLOY_LED_CLEANUP_PHASE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	api := &inventoryLEDObserver{fakeLED: &fakeLED{states: input.States}}
	_, err = r.clearLEDFault(context.Background(), migrationSocket, input.Inventory, "selected", input.ID, input.Evidence, api, acceptLEDDomain)
	must(t, err)
	t.Fatal("kill boundary not reached")
}

func TestRecoveryLEDCleanupSIGKILL(t *testing.T) {
	for _, phase := range []string{"led_clear_intent", "led_clear_transaction", "led_clear_committed", "led_clear_returned", "led_clear_result", "led_clear_result_transaction", "led_clear_result_committed"} {
		t.Run(phase, func(t *testing.T) {
			f := newLEDEvidenceFixture(t, true, false)
			evidence := cleanupProposal(t, f)
			input := ledCleanupCrashInput{f.config, f.inventory.InventoryID, evidence, f.old, f.api.states, f.now}
			must(t, f.r.dir.WriteJSON("test-led-cleanup.json", input, false))
			must(t, f.r.Close())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryLEDCleanupCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_LED_CLEANUP_STATE="+f.path, "DEPLOY_LED_CLEANUP_PHASE="+phase)
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
			r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil)
			must(t, err)
			defer r.Close()
			r.now = func() time.Time { return f.now.Add(time.Hour) }
			r.clock = func() (string, float64, error) { return "rebooted", 1, nil }
			v, err := r.clearLEDFault(context.Background(), migrationSocket, input.Inventory, "selected", input.ID, input.Evidence, f.api, acceptLEDDomain)
			must(t, err)
			want := "uncertain"
			if strings.HasPrefix(phase, "led_clear_result") {
				want = "acknowledged"
			}
			if v.Outcome != want || !v.IssuedAt.Equal(input.Now) || len(f.api.cleared) != 0 {
				t.Fatal("restart replayed or renewed attempt", phase, v)
			}
			w, err := r.clearLEDFault(context.Background(), migrationSocket, input.Inventory, "selected", input.ID, input.Evidence, nil, nil)
			must(t, err)
			if protocol.Hash(v) != protocol.Hash(w) {
				t.Fatal("retry changed history")
			}
			_, err = r.readLEDClears(context.Background(), r.db)
			must(t, err)
			if s, err := Open(f.path, f.config, false); err == nil {
				s.Close()
				t.Fatal("recovery fence lost")
			}
		})
	}
}
