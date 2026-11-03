package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type fakeSignalServiceManager struct {
	units                  string
	state                  signalServiceState
	manifest               SignalServiceManifest
	starts, stops, reloads int
}

func (f *fakeSignalServiceManager) Inspect(_ context.Context, m SignalServiceManifest) (signalServiceState, error) {
	f.manifest = m
	return f.state, nil
}
func (f *fakeSignalServiceManager) Reload(context.Context) error {
	f.reloads++
	_, err := os.Stat(filepath.Join(f.units, signalServiceUnit))
	if os.IsNotExist(err) {
		f.state = signalServiceState{}
		return nil
	}
	if err != nil {
		return err
	}
	f.state.Loaded = true
	return nil
}
func (f *fakeSignalServiceManager) Start(context.Context) error {
	f.starts++
	f.state = signalServiceState{Loaded: true, Active: true, Enabled: true, PID: 123}
	return nil
}
func (f *fakeSignalServiceManager) Stop(context.Context) error {
	f.stops++
	f.state.Active = false
	f.state.PID = 0
	return nil
}
func (f *fakeSignalServiceManager) Disable(context.Context) error {
	f.state.Enabled = false
	return nil
}
func (f *fakeSignalServiceManager) Verify(ctx context.Context, m SignalServiceManifest) error {
	if !f.state.Active || !f.state.Enabled {
		return errors.New("fixture service inactive")
	}
	v, err := openSignalEndpointVerifierService(ctx, m.Spec.Endpoint.Root, m.Spec.Endpoint.ManifestHash, m.binding())
	if err != nil {
		return err
	}
	defer v.close()
	return v.check(ctx)
}
func signalServiceSpecFor(t *testing.T, p SignalVerificationManifest, units, binary string) SignalServiceSpec {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	op, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	return SignalServiceSpec{Version: 1, Operation: op, Root: root, UnitDirectory: units, BinaryFile: binary, BinarySHA256: managedDigest(b), Endpoint: SignalVerificationReference{Root: p.Spec.Root, ManifestHash: p.ManifestHash}}
}
func signalServiceFixture(t *testing.T) (SignalServiceSpec, signalServiceInstaller, *fakeSignalServiceManager) {
	t.Helper()
	s := signalVerificationFixture(t)
	p, err := PrepareSignalVerification(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	units := t.TempDir()
	if err := os.Chmod(units, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "input-deploy")
	raw, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(binary, raw, 0700); err != nil {
		t.Fatal(err)
	}
	spec := signalServiceSpecFor(t, p, units, binary)
	f := &fakeSignalServiceManager{units: units}
	i := signalServiceInstaller{manager: f, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
	return spec, i, f
}
func TestSignalServiceLifecycle(t *testing.T) {
	s, i, f := signalServiceFixture(t)
	ctx := context.Background()
	m, err := i.setup(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Endpoint
	ref.ServiceRoot = s.Root
	ref.ServiceHash = m.ManifestHash
	if _, err = inspectSignalService(ref); err != nil {
		t.Fatal(err)
	}
	if v, e := openSignalEndpointVerifier(ctx, s.Endpoint.Root, s.Endpoint.ManifestHash); e == nil {
		v.close()
		t.Fatal("foreground endpoint bypassed managed binding")
	}
	starts, reloads := f.starts, f.reloads
	if got, e := i.resume(ctx, s.Root); e != nil || protocol.Hash(got) != protocol.Hash(m) || f.starts != starts || f.reloads != reloads {
		t.Fatal("completed retry changed service", e)
	}
	if err = os.Remove(filepath.Join(s.Root, "complete.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = i.resume(ctx, s.Root); err == nil {
		t.Fatal("lost completion reconstructed")
	}
	d, err := safefs.Open(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.WriteJSON("complete.json", m, false); err != nil {
		t.Fatal(err)
	}
	if err = f.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = i.resume(ctx, s.Root); err != nil || f.starts != starts+1 {
		t.Fatal("restart failed", err)
	}
	if _, err = i.uninstall(ctx, s.Root); err != nil {
		t.Fatal(err)
	}
	if _, err = inspectSignalService(ref); err == nil {
		t.Fatal("removed service adoptable")
	}
	if f.state.Loaded || f.state.Active || f.state.Enabled {
		t.Fatal(f.state)
	}
	stops := f.stops
	if _, err = i.resume(ctx, s.Root); err != nil || f.stops != stops {
		t.Fatal("removed retry repeated stop", err)
	}
	for _, name := range []string{"intent.json", "endpoint.json", "ready.json", "complete.json", "remove.json", "deploy"} {
		if _, err = os.Stat(filepath.Join(s.Root, name)); err != nil {
			t.Fatal("retained evidence removed", err)
		}
	}
}
func TestSignalServiceInterrupted(t *testing.T) {
	for _, at := range []string{"prepared", "bound", "unit", "published", "reloaded", "start-returned", "started", "complete"} {
		t.Run(at, func(t *testing.T) {
			s, i, f := signalServiceFixture(t)
			stop := errors.New("interrupt")
			i.fault = func(p string) error {
				if p == at {
					return stop
				}
				return nil
			}
			if _, err := i.setup(context.Background(), s); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			if _, err := i.resume(context.Background(), s.Root); err != nil {
				t.Fatal(err)
			}
			if f.starts != 1 {
				t.Fatal("replayed start", f.starts)
			}
		})
	}
}
func TestSignalServiceRemovalInterrupted(t *testing.T) {
	for _, at := range []string{"remove-intent", "removing", "stop-returned", "disable-returned", "unit-removed", "removed"} {
		t.Run(at, func(t *testing.T) {
			s, i, f := signalServiceFixture(t)
			if _, err := i.setup(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("interrupt")
			i.fault = func(p string) error {
				if p == at {
					return stop
				}
				return nil
			}
			if _, err := i.uninstall(context.Background(), s.Root); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			if _, err := i.resume(context.Background(), s.Root); err != nil {
				t.Fatal(err)
			}
			if f.state.Loaded || f.state.Active || f.state.Enabled {
				t.Fatal(f.state)
			}
		})
	}
}
func TestSignalServiceRejectsAuthorityLoss(t *testing.T) {
	for _, file := range []string{"deploy", "endpoint.json", "intent.json", "ready.json", "complete.json", "lifecycle.json", "installation.lock", "operation.lock"} {
		t.Run(file, func(t *testing.T) {
			s, i, f := signalServiceFixture(t)
			if _, err := i.setup(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			starts := f.starts
			if err := os.Remove(filepath.Join(s.Root, file)); err != nil {
				t.Fatal(err)
			}
			if _, err := i.resume(context.Background(), s.Root); err == nil {
				t.Fatal("lost authority recreated")
			}
			if f.starts != starts {
				t.Fatal("service started after proof loss")
			}
		})
	}
}
func TestSignalServiceRejectsForeignUnitAndCapability(t *testing.T) {
	s, i, f := signalServiceFixture(t)
	path := filepath.Join(s.UnitDirectory, signalServiceUnit)
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := i.setup(context.Background(), s); err == nil {
		t.Fatal("foreign unit replaced")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	i.probe = func(context.Context, *safefs.Directory, SignalServiceManifest) error {
		return errors.New("old executable")
	}
	if _, err := i.setup(context.Background(), s); err == nil {
		t.Fatal("missing capability accepted")
	}
	if f.starts != 0 || f.stops != 0 {
		t.Fatal("mutated service before probe")
	}
	if _, err := os.Stat(filepath.Join(s.Endpoint.Root, signalServiceEndpointBinding)); !os.IsNotExist(err) {
		t.Fatal("endpoint bound before capability", err)
	}
}
func TestManagedSignalSupervision(t *testing.T) {
	for _, prior := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "upgrade"}[prior], func(t *testing.T) {
			spec, i, f, p, _ := managedSignalFixtureProviders(t, true)
			ctx := context.Background()
			if prior {
				if _, err := i.prepare(ctx, "adopt-signal", spec); err != nil {
					t.Fatal(err)
				}
			}
			s := signalServiceSpecFor(t, p, spec.UnitDirectory, spec.BinaryFile)
			sm := &fakeSignalServiceManager{units: s.UnitDirectory}
			installer := signalServiceInstaller{manager: sm, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
			service, err := installer.setup(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			ref := *spec.SignalVerification
			ref.ServiceRoot = s.Root
			ref.ServiceHash = service.ManifestHash
			spec.SignalVerification = &ref
			i.signalCheck = func(ctx context.Context, m ManagedManifest) error { return sm.Verify(ctx, service) }
			operation := "adopt-signal"
			if prior {
				operation = "supervise-signal"
			}
			m, err := i.prepare(ctx, operation, spec)
			if err != nil {
				t.Fatal(err)
			}
			unit, err := os.ReadFile(filepath.Join(spec.UnitDirectory, managedUnit))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(unit), "Requires="+signalServiceUnit+"\nAfter="+signalServiceUnit) {
				t.Fatal("missing dependency")
			}
			stops := f.stops
			if _, err = i.resume(ctx, spec.Root); err != nil || stops != f.stops {
				t.Fatal("completed resume failed", err)
			}
			if _, err = i.prepare(ctx, operation, spec); err == nil {
				t.Fatal("duplicate transition accepted")
			}
			spec.SignalVerification = nil
			if _, err = i.prepare(ctx, "update", spec); err != nil {
				t.Fatal("update lost supervision/Telegram proof", err)
			}
			if m.SignalVerification.ServiceHash != service.ManifestHash {
				t.Fatal("lost service pin")
			}
			// Gateway removal leaves endpoint available; service removal is explicit and last.
			if _, err = i.uninstall(ctx, spec.Root); err != nil {
				t.Fatal(err)
			}
			if _, err = installer.uninstall(ctx, s.Root); err != nil {
				t.Fatal(err)
			}
			if _, err = i.uninstall(ctx, spec.Root); err != nil {
				t.Fatal("retired gateway lost service evidence after endpoint removal", err)
			}
			if _, err = i.resume(ctx, spec.Root); err != nil {
				t.Fatal("retired gateway resume", err)
			}
			retained, e := safefs.Open(spec.Root)
			if e != nil {
				t.Fatal(e)
			}
			_, e = readManagedSignalBinding(retained, m)
			retained.Close()
			if e == nil {
				t.Fatal("live gateway accepted removed verifier")
			}

		})
	}
}

func TestSignalServiceBoundStagingCannotBeRecreated(t *testing.T) {
	for _, name := range []string{"deploy", "endpoint.json"} {
		t.Run(name, func(t *testing.T) {
			s, i, _ := signalServiceFixture(t)
			i.fault = func(at string) error {
				if at == "bound" {
					return errors.New("interrupt")
				}
				return nil
			}
			if _, err := i.setup(context.Background(), s); err == nil {
				t.Fatal("missing interruption")
			}
			d, err := safefs.Open(s.Root)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			m, err := readSignalService(d, "intent.json")
			if err != nil {
				t.Fatal(err)
			}
			// Simulate crash after publishing the endpoint binding but before journal advancement.
			if err = d.WriteJSON("lifecycle.json", signalServiceJournal{m.ManifestHash, "prepared"}, true); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filepath.Join(s.Root, name)); err != nil {
				t.Fatal(err)
			}
			i.fault = nil
			if _, err = i.resume(context.Background(), s.Root); err == nil {
				t.Fatal("lost staging reconstructed after binding")
			}
		})
	}
}
func TestSignalServiceForeignUnitOnRemoval(t *testing.T) {
	s, i, f := signalServiceFixture(t)
	if _, err := i.setup(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.UnitDirectory, signalServiceUnit)
	if err := os.WriteFile(path, []byte("foreign replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := i.uninstall(context.Background(), s.Root); err == nil {
		t.Fatal("foreign unit removed")
	}
	if f.stops != 0 {
		t.Fatal("foreign unit stopped")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "foreign replacement" {
		t.Fatal("foreign unit changed", err)
	}
}

func TestSignalServiceRejectsPermissionDrift(t *testing.T) {
	s, i, _ := signalServiceFixture(t)
	m, err := i.setup(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Endpoint
	ref.ServiceRoot = s.Root
	ref.ServiceHash = m.ManifestHash
	for _, path := range []string{filepath.Join(s.Root, "deploy"), filepath.Join(s.UnitDirectory, signalServiceUnit)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		changed := os.FileMode(0600)
		if info.Mode().Perm() == 0600 {
			changed = 0644
		}
		if err = os.Chmod(path, changed); err != nil {
			t.Fatal(err)
		}
		if _, err = inspectSignalService(ref); err == nil {
			t.Fatal("changed service permissions accepted")
		}
		if err = os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestManagedSignalSupervisionInterrupted(t *testing.T) {
	for _, point := range []string{"prepared", "manifest-published", "start-returned", "complete"} {
		t.Run(point, func(t *testing.T) {
			spec, i, _, p, _ := managedSignalFixture(t)
			ctx := context.Background()
			if _, err := i.prepare(ctx, "adopt-signal", spec); err != nil {
				t.Fatal(err)
			}
			s := signalServiceSpecFor(t, p, spec.UnitDirectory, spec.BinaryFile)
			sm := &fakeSignalServiceManager{units: s.UnitDirectory}
			si := signalServiceInstaller{manager: sm, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
			service, err := si.setup(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			ref := *spec.SignalVerification
			ref.ServiceRoot = s.Root
			ref.ServiceHash = service.ManifestHash
			spec.SignalVerification = &ref
			i.signalCheck = func(ctx context.Context, m ManagedManifest) error { return sm.Verify(ctx, service) }
			stop := errors.New("interruption")
			i.fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if _, err = i.prepare(ctx, "supervise-signal", spec); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			m, err := i.resume(ctx, spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			if m.SignalVerification.ServiceHash != service.ManifestHash {
				t.Fatal("supervision lost")
			}
		})
	}
}
