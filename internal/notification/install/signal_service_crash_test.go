package install

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type signalServiceCrashInput struct {
	Spec     SignalServiceSpec
	Boundary string
	Removing bool
	State    signalServiceState
}

func TestSignalServiceCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_SIGNAL_SERVICE_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess only")
	}
	d, err := safefs.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var in signalServiceCrashInput
	if err = d.ReadJSON("input.json", &in); err != nil {
		t.Fatal(err)
	}
	f := &fakeSignalServiceManager{units: in.Spec.UnitDirectory, state: in.State}
	i := signalServiceInstaller{manager: f, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
	i.fault = func(at string) error {
		if at != in.Boundary {
			return nil
		}
		if err := d.WriteJSON("manager.json", f.state, false); err != nil {
			t.Fatal(err)
		}
		return syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	if in.Removing {
		_, err = i.uninstall(context.Background(), in.Spec.Root)
	} else {
		_, err = i.setup(context.Background(), in.Spec)
	}
	t.Fatal("helper survived", err)
}
func TestSignalServiceSIGKILL(t *testing.T) {
	testSignalServiceSIGKILL(t, false)
}
func TestSignalProductionSIGKILL(t *testing.T) {
	testSignalServiceSIGKILL(t, true)
}
func testSignalServiceSIGKILL(t *testing.T, production bool) {
	for _, at := range []string{"replacement-intent", "replacement-bound", "replacement-complete", "bound", "unit", "published", "start-returned", "complete", "remove-intent", "stop-returned", "unit-removed"} {
		t.Run(at, func(t *testing.T) {
			fixture := signalServiceFixture
			if production {
				fixture = productionSignalFixture
			}
			s, i, f := fixture(t)
			if strings.HasPrefix(at, "replacement-") {
				prior, err := i.setup(context.Background(), s)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = i.uninstall(context.Background(), s.Root); err != nil {
					t.Fatal(err)
				}
				s = replacementServiceSpec(t, prior)
				s.Runtime = prior.Spec.Runtime
			}
			removing := at == "remove-intent" || at == "stop-returned" || at == "unit-removed"
			if removing {
				if _, err := i.setup(context.Background(), s); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = d.WriteJSON("input.json", signalServiceCrashInput{Spec: s, Boundary: at, Removing: removing, State: f.state}, false); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSignalServiceCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_SIGNAL_SERVICE_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong exit", string(out))
			}
			if err = d.ReadJSON("manager.json", &f.state); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(s.BinaryFile); err != nil {
				t.Fatal(err)
			}
			if _, err = i.resume(context.Background(), s.Root); err != nil {
				t.Fatal("resume without original executable", err)
			}
			if removing && f.state.Loaded {
				t.Fatal("removal not completed")
			}
			starts, stops, reloads := f.starts, f.stops, f.reloads
			if _, err = i.resume(context.Background(), s.Root); err != nil || starts != f.starts || stops != f.stops || reloads != f.reloads {
				t.Fatal("completed retry repeated effects", err)
			}
		})
	}
}
