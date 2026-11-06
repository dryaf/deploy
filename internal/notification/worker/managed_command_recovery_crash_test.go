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

type managedCommandRecoveryCrashInput struct {
	Path, Revision, Point string
	Config                Config
	Recovery              ManagedDeliveryRecovery
	Commands              ManagedCommandRecovery
}

func TestManagedCommandRecoveryCrashChild(t *testing.T) {
	file := os.Getenv("DEPLOY_COMMAND_RECOVERY_CRASH")
	if file == "" {
		return
	}
	raw, err := os.ReadFile(file)
	must(t, err)
	var in managedCommandRecoveryCrashInput
	must(t, json.Unmarshal(raw, &in))
	s, err := OpenManagedRecoveryVerification(in.Path, in.Config, in.Recovery.After, in.Revision, in.Recovery.CommitHash)
	must(t, err)
	defer s.Close()
	err = s.enrollManagedCommandRecovery(in.Commands, in.Recovery, func(point string) error {
		if point == in.Point {
			fmt.Println("ready")
			select {}
		}
		return nil
	})
	t.Fatal("child did not stop", err)
}

func TestManagedCommandRecoverySIGKILL(t *testing.T) {
	for _, point := range []string{"command-recovery-intent", "command-recovery-sql", "command-recovery-head", "command-recovery-complete"} {
		t.Run(point, func(t *testing.T) {
			path, c, revision, r, commands := managedCommandRecoveryFixture(t)
			in := managedCommandRecoveryCrashInput{path, revision, point, c, r, commands}
			raw, err := json.Marshal(in)
			must(t, err)
			file := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(file, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedCommandRecoveryCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_COMMAND_RECOVERY_CRASH="+file)
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
			s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
			must(t, err)
			defer s.Close()
			must(t, s.EnrollManagedCommandRecovery(commands, r))
			must(t, s.RequireManagedCommandHistory(protocol.Hash(commands)))
			if !s.VerificationOnly() {
				t.Fatal("crash resume enabled intake")
			}
		})
	}
}
