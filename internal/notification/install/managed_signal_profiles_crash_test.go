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
)

func TestManagedSignalProfilesSIGKILL(t *testing.T) {
	for _, boundary := range []string{"prepared", "unit-published", "manifest-published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			spec, i, f, _, _ := managedSignalProfilesFixture(t, false)
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = d.WriteJSON("input.json", managedCrashInput{Spec: spec, Operation: "migrate-signal-profiles", Boundary: boundary, Service: f.state}, false); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestManagedCrashHelper$", "-test.timeout=60s")
			cmd.Env = append(os.Environ(), "DEPLOY_MANAGED_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(cmd, 65*time.Second, 16384)
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
			if err = os.Remove(spec.BinaryFile); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(spec.RuntimeFile); err != nil {
				t.Fatal(err)
			}
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			_, c := effectiveRuntimeForTest(t, m)
			if m.SignalProfiles == nil || c.Providers["signal"].Socket != c.Signal.Socket {
				t.Fatal("resume lost Signal profiles")
			}
			stops := f.stops
			if _, err = i.resume(context.Background(), spec.Root); err != nil || stops != f.stops {
				t.Fatal("completed resume repeated lifecycle", err)
			}
		})
	}
}
