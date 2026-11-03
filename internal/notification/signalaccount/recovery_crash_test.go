package signalaccount

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

type recoveryCrashInput struct {
	Request  RecoveryRequest
	Boundary string
}

func TestSignalRecoveryCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_SIGNAL_RECOVERY_CRASH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(filepath.Join(path, "crash-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var input recoveryCrashInput
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	svc := &recoveryServiceFixture{fixtureService: &fixtureService{path: path, binding: input.Request.Binding}}
	apply, verify, _ := recoveryCallbacks(t, path)
	c := recoveryCoordinator{fault: func(at string) error {
		if at == input.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	_, err = c.run(context.Background(), path, input.Request, svc, apply, verify)
	t.Fatal("missing SIGKILL", err)
}
func TestSignalRecoverySIGKILL(t *testing.T) {
	for _, point := range []string{"recovery_intent", "recovery_fence", "recovery_stop", "recovery_applied", "recovery_result", "recovery_complete"} {
		t.Run(point, func(t *testing.T) {
			path, req, svc := recoveryFixture(t)
			if err := svc.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(recoveryCrashInput{req, point})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(path, "crash-input.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSignalRecoveryCrashChild$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_SIGNAL_RECOVERY_CRASH="+path)
			output, err := process.LimitedOutput(cmd, 20*time.Second, 8192)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(output))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", string(output))
			}
			requireRuntime(t, path, false)
			apply, verify, calls := recoveryCallbacks(t, path)
			done, err := FenceRecovery(context.Background(), path, req, svc, apply, verify)
			if err != nil || done.RuntimeAllowed || done.Phase != "complete" {
				t.Fatal(done, err)
			}
			if (point == "recovery_result" || point == "recovery_complete") && *calls != 0 {
				t.Fatal("completed effect repeated")
			}
		})
	}
}

func TestSignalRecoveryInheritedChildLease(t *testing.T) {
	path, req, svc := recoveryFixture(t)
	child := exec.Command("sleep", "30")
	child.ExtraFiles = []*os.File{svc.lease.File()}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	})
	apply, verify, calls := recoveryCallbacks(t, path)
	if _, err := FenceRecovery(context.Background(), path, req, svc, apply, verify); err == nil || *calls != 0 {
		t.Fatal("inherited CLI lease ignored", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	done, err := FenceRecovery(context.Background(), path, req, svc, apply, verify)
	if err != nil || done.Phase != "complete" {
		t.Fatal(done, err)
	}
}
