package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type telegramHandoffCrashInput struct {
	Path, Revision, Point, Boot string
	Config                      Config
	Recovery                    ManagedDeliveryRecovery
	Renewal                     TelegramHandoffRenewal
	At                          time.Time
	Uptime                      float64
}

func TestManagedTelegramHandoffCrashChild(t *testing.T) {
	file := os.Getenv("DEPLOY_TELEGRAM_HANDOFF_CRASH")
	if file == "" {
		return
	}
	raw, err := os.ReadFile(file)
	must(t, err)
	var in telegramHandoffCrashInput
	must(t, json.Unmarshal(raw, &in))
	s, err := OpenManagedRecoveryVerification(in.Path, in.Config, in.Recovery.After, in.Revision, in.Recovery.CommitHash)
	must(t, err)
	defer s.Close()
	clock := telegramRenewalClock{func() time.Time { return in.At }, func() (string, float64, error) { return in.Boot, in.Uptime, nil }}
	_, err = s.enrollManagedTelegramHandoff(context.Background(), in.Recovery, in.Renewal.Binding.Account, in.Renewal.Binding.ID, in.Renewal.RenewalHash, clock, func(point string) error {
		if point == in.Point {
			fmt.Println("ready")
			select {}
		}
		return nil
	})
	t.Fatal("child did not stop", err)
}

func TestManagedTelegramHandoffSIGKILL(t *testing.T) {
	for _, point := range []string{"telegram_handoff_intent", "telegram_handoff_sql", "telegram_handoff_complete"} {
		t.Run(point, func(t *testing.T) {
			f, renewal, r, revision, s := managedTelegramHandoffFixture(t)
			must(t, s.Close())
			in := telegramHandoffCrashInput{f.Path, revision, point, f.Boot, f.Config, r, renewal, f.At, f.Up}
			raw, err := json.Marshal(in)
			must(t, err)
			file := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(file, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedTelegramHandoffCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_TELEGRAM_HANDOFF_CRASH="+file)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			must(t, cmd.Start())
			scanner := bufio.NewScanner(pipe)
			ready := false
			for scanner.Scan() {
				if scanner.Text() == "ready" {
					ready = true
					break
				}
			}
			if !ready {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("missing durable boundary", scanner.Err())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("child was not killed")
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong child exit")
			}
			s, err = OpenManagedRecoveryVerification(f.Path, f.Config, r.After, revision, r.CommitHash)
			must(t, err)
			defer s.Close()
			h, err := s.enrollManagedTelegramHandoff(context.Background(), r, renewal.Binding.Account, renewal.Binding.ID, renewal.RenewalHash, f.clock(), nil)
			must(t, err)
			must(t, s.RequireManagedTelegramHandoff(context.Background(), h, protocol.Hash(h)))
			if !s.VerificationOnly() {
				t.Fatal("crash resume enabled intake")
			}
		})
	}
}
