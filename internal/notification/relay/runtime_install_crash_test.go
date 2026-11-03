package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/process"
)

type runtimeInstallCrash struct {
	Source, Destination, Point string
	Cursor                     interaction.SignalRecoveryCursor
}

func TestSignalRuntimeInstallCrashHelper(t *testing.T) {
	file := os.Getenv("DEPLOY_SIGNAL_RUNTIME_CRASH")
	if file == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var in runtimeInstallCrash
	if err = json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	_, err = installSignalRuntime(context.Background(), in.Source, in.Destination, in.Cursor, func(at string) error {
		if at == in.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash boundary missed", err)
}
func TestSignalRuntimeInstallSIGKILL(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, point := range runtimeInstallBoundaries {
			t.Run(fmt.Sprintf("%t/%s", restored, point), func(t *testing.T) {
				src, dst, p := runtimeInstallFixture(t, restored)
				raw, err := json.Marshal(runtimeInstallCrash{src, dst, point, p})
				if err != nil {
					t.Fatal(err)
				}
				input := filepath.Join(t.TempDir(), "input.json")
				if err = os.WriteFile(input, raw, 0600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestSignalRuntimeInstallCrashHelper$", "-test.timeout=45s")
				cmd.Env = append(os.Environ(), "DEPLOY_SIGNAL_RUNTIME_CRASH="+input)
				out, err := process.LimitedOutput(cmd, 50*time.Second, 16384)
				if err == nil || cmd.ProcessState == nil {
					t.Fatal("missing crash", err, string(out))
				}
				status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatal("wrong exit", string(out))
				}
				one, err := InstallSignalRuntime(context.Background(), src, dst, p)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = VerifyInstalledSignalRuntime(context.Background(), dst, one.InstallationHash); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
