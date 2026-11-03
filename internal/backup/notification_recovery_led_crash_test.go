//go:build linux

package backup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type backupLEDCrashInput struct {
	Config        WorkerInstallation
	GatewayPath   string
	GatewayConfig worker.Config
	Inventory     string
}

func TestBackupLEDRecoveryCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_LED_STATE")
	if path == "" {
		return
	}
	dir, err := safefs.Open(path)
	testutil.Must(t, err)
	defer dir.Close()
	var input backupLEDCrashInput
	testutil.Must(t, dir.ReadJSON("test-led-recovery.json", &input))
	state, err := openBackupState(path, false)
	testutil.Must(t, err)
	defer state.Close()
	session, err := worker.OpenRecoveryLEDEvidenceSession(context.Background(), input.GatewayPath, input.GatewayConfig)
	testutil.Must(t, err)
	defer session.Close()
	_, err = reconcileBackupLEDRecoveryWith(context.Background(), state, input.Config, session, input.Inventory, reportHostClock, func(stage string) error {
		if stage == os.Getenv("DEPLOY_BACKUP_LED_PHASE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	})
	testutil.Must(t, err)
	t.Fatal("kill boundary not reached")
}
func TestBackupLEDRecoverySIGKILL(t *testing.T) {
	for _, phase := range []string{"led_source_challenge", "led_source_report_transaction", "led_source_report_commit", "led_source_worker_accepted", "led_source_receipt_transaction", "led_source_receipt_commit"} {
		t.Run(phase, func(t *testing.T) {
			f := newBackupLEDAdapterFixture(t)
			before := backupLEDSequence(t, f.state)
			path := f.state.directory
			d, err := safefs.Open(path)
			testutil.Must(t, err)
			defer d.Close()
			testutil.Must(t, d.WriteJSON("test-led-recovery.json", backupLEDCrashInput{f.config, f.gatewayPath, f.gatewayConfig, f.inventory.InventoryID}, false))
			testutil.Must(t, f.session.Close())
			testutil.Must(t, f.state.Close())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackupLEDRecoveryCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_LED_STATE="+path, "DEPLOY_BACKUP_LED_PHASE="+phase)
			pipe, err := cmd.StdoutPipe()
			testutil.Must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			testutil.Must(t, cmd.Start())
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
			testutil.Must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			f.state, err = openBackupState(path, false)
			testutil.Must(t, err)
			defer f.state.Close()
			f.session, err = worker.OpenRecoveryLEDEvidenceSession(context.Background(), f.gatewayPath, f.gatewayConfig)
			testutil.Must(t, err)
			// Only the SQL commit can allocate the new status sequence. Process death
			// inside that transaction rolls the evaluator and report back together.
			want := before + 1
			if phase == "led_source_challenge" || phase == "led_source_report_transaction" {
				want = before
			}
			if got := backupLEDSequence(t, f.state); got != want {
				t.Fatal("evaluation/report were not atomic", got, want)
			}
			proof, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
			testutil.Must(t, err)
			if backupLEDSequence(t, f.state) != before+1 {
				t.Fatal("restart evaluated twice")
			}
			replay, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
			testutil.Must(t, err)
			if notify.Hash(proof) != notify.Hash(replay) {
				t.Fatal("restart changed frozen proof")
			}
			assertBackupLEDDeliveryFenced(t, f)
		})
	}
}
