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

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type ledEvidenceCrashInput struct {
	Config    Config
	Inventory string
	Challenge RecoveryLEDChallenge
	Report    RecoveryLEDReport
	Now       time.Time
}

func TestRecoveryLEDEvidenceCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_EVIDENCE_STATE")
	if path == "" {
		return
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var input ledEvidenceCrashInput
	must(t, d.ReadJSON("test-led-evidence.json", &input))
	r, err := openRecoveryReconciler(context.Background(), path, input.Config, nil)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return input.Now }
	r.clock = func() (string, float64, error) { return "boot-one", 100, nil }
	phase := os.Getenv("DEPLOY_LED_EVIDENCE_PHASE")
	r.fault = func(stage string) error {
		if stage == phase {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	if strings.HasPrefix(phase, "led_challenge_") {
		_, err = r.ChallengeLED(context.Background(), input.Inventory, producer)
	} else {
		_, err = r.SubmitLED(context.Background(), input.Challenge, input.Report)
	}
	must(t, err)
	t.Fatal("kill boundary not reached")
}
func TestRecoveryLEDEvidenceSIGKILL(t *testing.T) {
	for _, phase := range []string{"led_challenge_intent", "led_challenge_transaction", "led_challenge_committed", "led_report_intent", "led_report_transaction", "led_report_committed"} {
		t.Run(phase, func(t *testing.T) {
			f := newLEDEvidenceFixture(t, true, false)
			ctx := context.Background()
			inventory := f.inventory.InventoryID
			if strings.HasPrefix(phase, "led_challenge_") {
				v, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true)
				must(t, err)
				inventory = v.InventoryID
			}
			input := ledEvidenceCrashInput{f.config, inventory, f.challenge, f.report, f.now}
			must(t, f.r.dir.WriteJSON("test-led-evidence.json", input, false))
			must(t, f.r.Close())
			childCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestRecoveryLEDEvidenceCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_LED_EVIDENCE_STATE="+f.path, "DEPLOY_LED_EVIDENCE_PHASE="+phase)
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
			r, err := openRecoveryReconciler(ctx, f.path, f.config, nil)
			must(t, err)
			defer r.Close()
			r.now = func() time.Time { return f.now.Add(time.Hour) }
			r.clock = func() (string, float64, error) { return "rebooted", 1, nil }
			if strings.HasPrefix(phase, "led_challenge_") {
				var intent recoveryLEDChallengeRecord
				must(t, r.stage.ReadJSON(ledEvidenceFile(inventory, producer, "challenge"), &intent))
				ch, err := r.ChallengeLED(ctx, inventory, producer)
				must(t, err)
				if ch != intent.Challenge {
					t.Fatal("restart renewed challenge")
				}
				if _, err = r.SubmitLED(ctx, ch, f.report); err == nil {
					t.Fatal("expired challenge accepted a new report")
				}
			} else {
				var saved RecoveryLEDEvidence
				must(t, r.stage.ReadJSON(ledEvidenceFile(inventory, producer, "report"), &saved))
				proof, err := r.SubmitLED(ctx, f.challenge, f.report)
				must(t, err)
				if protocol.Hash(proof) != protocol.Hash(saved) {
					t.Fatal("restart renewed report")
				}
				v, err := r.reviewLEDInventory(ctx, migrationSocket, nil, false)
				must(t, err)
				if v.Resolutions["selected"].DomainResolved || v.ClearAllowed || v.ActivationAllowed {
					t.Fatal("historical evidence acquired authority")
				}
			}
			must(t, r.checkLEDEvidenceHistory(ctx, r.db))
			if s, err := Open(f.path, f.config, false); err == nil {
				s.Close()
				t.Fatal("recovery fence lost")
			}
		})
	}
}
