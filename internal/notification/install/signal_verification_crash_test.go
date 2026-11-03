package install

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

type signalVerificationCrash struct {
	Spec     SignalVerificationSpec
	Boundary string
}

func TestSignalVerificationCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_SIGNAL_VERIFICATION_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var in signalVerificationCrash
	if err = json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	_, err = prepareSignalVerification(context.Background(), in.Spec, func(at string) error {
		if at == in.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash boundary missed", err)
}
func TestSignalVerificationSIGKILL(t *testing.T) {
	for _, point := range []string{"endpoint_intent", "endpoint_destination_bound", "endpoint_gateway_staged", "endpoint_complete"} {
		t.Run(point, func(t *testing.T) {
			s := signalVerificationFixture(t)
			raw, err := json.Marshal(signalVerificationCrash{s, point})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "crash.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSignalVerificationCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DEPLOY_SIGNAL_VERIFICATION_CRASH="+path)
			out, err := process.LimitedOutput(cmd, 50*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong exit", string(out))
			}
			m, err := PrepareSignalVerification(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			v, err := openSignalEndpointVerifier(context.Background(), s.Root, m.ManifestHash)
			if err != nil {
				t.Fatal(err)
			}
			v.close()
			if _, err = PrepareSignalVerification(context.Background(), s); err != nil {
				t.Fatal("repeat diverged", err)
			}
		})
	}
}
