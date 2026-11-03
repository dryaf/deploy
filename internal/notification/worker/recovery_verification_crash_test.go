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

	"github.com/dryaf/deploy/internal/process"
)

type verificationCrash struct {
	Path, Point, Commit string
	Config              Config
}

func TestRecoveryVerificationCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_RECOVERY_VERIFICATION_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var in verificationCrash
	must(t, json.Unmarshal(raw, &in))
	_, err = recoveryVerification(context.Background(), in.Path, in.Config, in.Commit, true, func(at string) error {
		if at == in.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash boundary missed", err)
}
func TestRecoveryVerificationSIGKILL(t *testing.T) {
	for _, point := range []string{"recovery_verification_intent", "recovery_verification_complete"} {
		t.Run(point, func(t *testing.T) {
			path, c, result, review := verificationFixture(t)
			raw, err := json.Marshal(verificationCrash{path, point, result.Commit.CommitHash, c})
			must(t, err)
			input := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(input, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryVerificationCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DEPLOY_RECOVERY_VERIFICATION_CRASH="+input)
			out, err := process.LimitedOutput(cmd, 50*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", string(out))
			}
			adopted, err := AdoptPublishedRecoveryVerification(context.Background(), path, c, result.Commit.CommitHash)
			must(t, err)
			if adopted != review {
				t.Fatal(adopted)
			}
		})
	}
}
