package install

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type freshSignalCrashInput struct {
	Spec     FreshSignalSpec
	Boundary string
	Removal  bool
	Restart  bool
}

func TestFreshSignalCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_FRESH_SIGNAL_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess only")
	}
	d, err := safefs.Open(path)
	testutil.Must(t, err)
	defer d.Close()
	var in freshSignalCrashInput
	testutil.Must(t, d.ReadJSON("input.json", &in))
	f := &fakeFreshSignalManager{units: in.Spec.UnitDirectory}
	if in.Removal || in.Restart {
		testutil.Must(t, d.ReadJSON("initial-manager.json", &f.state))
	}
	i := freshSignalInstaller{manager: f, probe: func(context.Context, *safefs.Directory, FreshSignalManifest) error { return nil }, fault: func(at string) error {
		if at != in.Boundary {
			return nil
		}
		testutil.Must(t, d.WriteJSON("manager.json", f.state, false))
		return syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}}
	i.restartProbe = func(context.Context, *safefs.Directory, FreshSignalManifest) error { return nil }
	if in.Restart {
		var m FreshSignalManifest
		testutil.Must(t, d.ReadJSON("manifest.json", &m))
		_, err = i.restart(context.Background(), m.reference())
	} else if in.Removal {
		var m FreshSignalManifest
		testutil.Must(t, d.ReadJSON("manifest.json", &m))
		_, err = i.uninstall(context.Background(), m.reference())
	} else {
		_, err = i.setup(context.Background(), in.Spec)
	}
	t.Fatal("crash helper survived", err)
}
func TestFreshSignalServiceSIGKILL(t *testing.T) {
	binary := freshSignalTestBinary(t)
	for _, at := range []string{"intent", "prepared", "staged", "verified", "binding-returned", "bound", "unit-returned", "unit", "published", "start-returned", "started", "complete"} {
		t.Run(at, func(t *testing.T) {
			s, i, f := freshSignalFixture(t, binary)
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			testutil.Must(t, err)
			defer d.Close()
			testutil.Must(t, d.WriteJSON("input.json", freshSignalCrashInput{Spec: s, Boundary: at}, false))
			cmd := exec.Command(os.Args[0], "-test.run=^TestFreshSignalCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_FRESH_SIGNAL_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong helper exit", string(out))
			}
			testutil.Must(t, d.ReadJSON("manager.json", &f.state))
			alreadyStarted := f.state.Active
			m, err := i.setup(context.Background(), s)
			testutil.Must(t, err)
			if alreadyStarted && f.starts != 0 {
				t.Fatal("lost reply restarted service")
			}
			_, err = InspectFreshSignal(context.Background(), m.reference(), false)
			testutil.Must(t, err)
			starts, reloads := f.starts, f.reloads
			_, err = i.setup(context.Background(), s)
			testutil.Must(t, err)
			if starts != f.starts || reloads != f.reloads {
				t.Fatal("completed retry repeated effects")
			}
		})
	}
}

func TestFreshSignalRemovalSIGKILL(t *testing.T) {
	binary := freshSignalTestBinary(t)
	for _, at := range []string{"remove-intent", "removing", "stop-returned", "disable-returned", "unit-removed", "removed"} {
		t.Run(at, func(t *testing.T) {
			s, i, f := freshSignalFixture(t, binary)
			m, err := i.setup(context.Background(), s)
			testutil.Must(t, err)
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			testutil.Must(t, err)
			defer d.Close()
			testutil.Must(t, d.WriteJSON("input.json", freshSignalCrashInput{Spec: s, Boundary: at, Removal: true}, false))
			testutil.Must(t, d.WriteJSON("initial-manager.json", f.state, false))
			testutil.Must(t, d.WriteJSON("manifest.json", m, false))
			cmd := exec.Command(os.Args[0], "-test.run=^TestFreshSignalCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_FRESH_SIGNAL_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong helper exit", string(out))
			}
			testutil.Must(t, d.ReadJSON("manager.json", &f.state))
			_, err = i.uninstall(context.Background(), m.reference())
			testutil.Must(t, err)
			_, j, err := inspectRetainedFreshSignal(m.reference())
			testutil.Must(t, err)
			if j.Phase != "removed" || f.state.Active || f.state.Enabled || f.state.Loaded {
				t.Fatal("removal not completed")
			}
			stops, reloads, disables := f.stops, f.reloads, f.disables
			_, err = i.uninstall(context.Background(), m.reference())
			testutil.Must(t, err)
			if stops != f.stops || reloads != f.reloads || disables != f.disables {
				t.Fatal("completed removal repeated effects")
			}

		})
	}
}

func TestFreshSignalRestartSIGKILL(t *testing.T) {
	binary := freshSignalTestBinary(t)
	for _, at := range []string{"restart-intent", "restart-anchored", "restart-start-returned", "restart-verified", "restart-complete"} {
		t.Run(at, func(t *testing.T) {
			spec, i, f := freshSignalFixture(t, binary)
			i.restartProbe = func(context.Context, *safefs.Directory, FreshSignalManifest) error { return nil }
			m, err := i.setup(context.Background(), spec)
			testutil.Must(t, err)
			f.state.Active, f.state.PID = false, 0
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			testutil.Must(t, err)
			defer d.Close()
			testutil.Must(t, d.WriteJSON("input.json", freshSignalCrashInput{Spec: spec, Boundary: at, Restart: true}, false))
			testutil.Must(t, d.WriteJSON("initial-manager.json", f.state, false))
			testutil.Must(t, d.WriteJSON("manifest.json", m, false))
			cmd := exec.Command(os.Args[0], "-test.run=^TestFreshSignalCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_FRESH_SIGNAL_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing restart crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong child exit", string(out))
			}
			testutil.Must(t, d.ReadJSON("manager.json", &f.state))
			active := f.state.Active
			starts := f.starts
			_, err = i.restart(context.Background(), m.reference())
			testutil.Must(t, err)
			if active && f.starts != starts || !active && f.starts != starts+1 {
				t.Fatal("restart count differs from observed process")
			}
			starts = f.starts
			_, err = i.restart(context.Background(), m.reference())
			testutil.Must(t, err)
			if f.starts != starts {
				t.Fatal("completed restart replayed")
			}
		})
	}
}
