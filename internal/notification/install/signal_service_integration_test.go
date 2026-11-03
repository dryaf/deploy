//go:build notificationintegration

package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

// Only the manager is simulated. Capability checks, executables, runtime leases,
// /proc identity and Unix peer health are exercised against real processes.
type executableSignalService struct {
	*fakeSignalServiceManager
	t    *testing.T
	cmd  *exec.Cmd
	done chan error
	log  *os.File
}

func (f *executableSignalService) manager(m SignalServiceManifest) userSignalServiceManager {
	return userSignalServiceManager{run: func(_ context.Context, program string, args ...string) ([]byte, error) {
		return signalServiceManagerOutput(f.t, m, signalServiceManagerProperties(m, f.state), program, args...)
	}}
}
func (f *executableSignalService) Inspect(ctx context.Context, m SignalServiceManifest) (signalServiceState, error) {
	f.manifest = m
	return f.manager(m).Inspect(ctx, m)
}
func (f *executableSignalService) Start(ctx context.Context) error {
	m := f.manifest
	var err error
	f.log, err = os.OpenFile(filepath.Join(m.Spec.Root, "test-runtime.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	args := m.commandArgs()
	f.cmd = exec.Command(args[0], args[1:]...)
	f.cmd.Stdout = f.log
	f.cmd.Stderr = f.log
	if err = f.cmd.Start(); err != nil {
		f.log.Close()
		return err
	}
	f.done = make(chan error, 1)
	cmd, done := f.cmd, f.done
	go func() { done <- cmd.Wait() }()
	f.starts++
	f.state = signalServiceState{Loaded: true, Enabled: true, Active: true, PID: uint32(f.cmd.Process.Pid)}
	return nil
}
func (f *executableSignalService) Stop(ctx context.Context) error {
	if f.cmd != nil {
		if err := f.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		select {
		case <-f.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return fmt.Errorf("endpoint did not stop")
		}
		f.cmd = nil
		f.log.Close()
		f.log = nil
	}
	return f.fakeSignalServiceManager.Stop(ctx)
}
func (f *executableSignalService) Verify(ctx context.Context, m SignalServiceManifest) error {
	return f.manager(m).Verify(ctx, m)
}
func TestSignalServiceNativeExecutable(t *testing.T) {
	spec, i, f, p, _ := managedSignalFixture(t)
	build := exec.Command("go", "build", "-o", spec.BinaryFile, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	if err := os.Chmod(spec.BinaryFile, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(raw)
	s := signalServiceSpecFor(t, p, spec.UnitDirectory, spec.BinaryFile)
	service := &executableSignalService{fakeSignalServiceManager: &fakeSignalServiceManager{units: s.UnitDirectory}, t: t}
	t.Cleanup(func() {
		if service.cmd != nil {
			service.cmd.Process.Kill()
			<-service.done
			service.log.Close()
		}
	})
	si := signalServiceInstaller{manager: service}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m, err := si.setup(ctx, s)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(s.Root, "test-runtime.log"))
		t.Fatal(err, string(log))
	}
	if _, err = si.resume(ctx, s.Root); err != nil {
		t.Fatal("live resume", err)
	}
	if err = service.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = si.resume(ctx, s.Root); err != nil {
		t.Fatal("endpoint restart", err)
	}
	ref := s.Endpoint
	ref.ServiceRoot = s.Root
	ref.ServiceHash = m.ManifestHash
	spec.SignalVerification = &ref
	native := &executableManagedServices{fakeManagedServices: f, root: spec.Root}
	t.Cleanup(func() {
		if native.cmd != nil {
			native.cmd.Process.Kill()
			<-native.done
			native.log.Close()
		}
	})
	i.services = native
	i.probe = probeManagedBinary
	i.signalProbe = nil
	i.signalCheck = nil
	i.recoveryProbe = nil
	gateway, err := i.prepare(ctx, "adopt-signal", spec)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(err, string(log))
	}
	if out, e := process.LimitedOutput(exec.CommandContext(ctx, "systemd-analyze", "--user", "verify", "--man=no", filepath.Join(s.UnitDirectory, signalServiceUnit), filepath.Join(s.UnitDirectory, managedUnit)), 15*time.Second, 16384); e != nil {
		t.Fatal("unit syntax", e, string(out))
	}
	if err = checkManagedSignalHealthPID(ctx, gateway, service.state.PID+1); err == nil {
		t.Fatal("wrong socket peer PID accepted")
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal("gateway resume", err)
	}
	if err = native.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = native.EnableStart(ctx); err != nil {
		t.Fatal(err)
	}
	if err = native.Verify(ctx, gateway); err != nil {
		t.Fatal("gateway restart", err)
	}
	// A lost endpoint invalidates the live gateway; no account intake is enabled.
	if err = service.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-native.done:
		if err == nil {
			t.Fatal("gateway ignored endpoint loss")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("gateway stayed ready without endpoint")
	}
	native.cmd = nil
	native.log.Close()
	native.log = nil
	if _, err = si.resume(ctx, s.Root); err != nil {
		t.Fatal(err)
	}
	if err = native.EnableStart(ctx); err != nil {
		t.Fatal(err)
	}
	if err = native.Verify(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	if _, err = i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal("gateway uninstall", err)
	}
	// The process also audits its independent service publication, not only the endpoint.
	evidence := filepath.Join(s.Root, "endpoint.json")
	retained, e := os.ReadFile(evidence)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(evidence, []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-service.done:
		if e == nil {
			t.Fatal("verifier ignored lost service evidence")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("verifier retained readiness after evidence loss")
	}
	service.cmd = nil
	service.log.Close()
	service.log = nil
	service.state.Active = false
	service.state.PID = 0
	if e = os.WriteFile(evidence, retained, 0600); e != nil {
		t.Fatal(e)
	}
	if _, err = si.uninstall(ctx, s.Root); err != nil {
		t.Fatal("endpoint uninstall", err)
	}
	if service.cmd != nil || native.cmd != nil {
		t.Fatal("runtime leaked after uninstall")
	}
}
