package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func managedSignalFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeManagedServices, SignalVerificationManifest, ManagedManifest) {
	return managedSignalFixtureProviders(t, false)
}
func managedSignalFixtureProviders(t *testing.T, telegram bool) (ManagedSpec, managedInstaller, *fakeManagedServices, SignalVerificationManifest, ManagedManifest) {
	return managedSignalFixtureConfigured(t, telegram, nil)
}
func managedSignalFixtureConfigured(t *testing.T, telegram bool, configure func(*Runtime), unitDirectories ...string) (ManagedSpec, managedInstaller, *fakeManagedServices, SignalVerificationManifest, ManagedManifest) {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	spec := ManagedSpec{Version: 1, Root: filepath.Join(base, "installation"), UnitDirectory: filepath.Join(base, "units"), BinaryFile: filepath.Join(base, "input-deploy")}
	if len(unitDirectories) == 1 {
		spec.UnitDirectory = unitDirectories[0]
	}
	if err := os.Mkdir(spec.UnitDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(spec.BinaryFile, raw, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(raw)
	f := &fakeManagedServices{t: t, units: spec.UnitDirectory, state: managedServiceState{Load: "not-found", Active: "inactive"}}
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i := managedInstaller{services: f, probe: func(context.Context, *safefs.Directory, string, string, bool) error { return nil }, recoveryProbe: nop, signalProbe: nop}
	i.signalCheck = func(ctx context.Context, m ManagedManifest) error {
		v, e := openSignalEndpointVerifier(ctx, m.SignalVerification.Root, m.SignalVerification.ManifestHash)
		if e != nil {
			return e
		}
		v.close()
		return nil
	}
	s := signalVerificationFixtureConfigured(t, func(config, state string) {
		spec.State = state
		spec.RuntimeFile = config
		if _, e := i.prepare(context.Background(), "setup", spec); e != nil {
			t.Fatal(e)
		}
		if e := f.Stop(context.Background()); e != nil {
			t.Fatal(e)
		}
	}, telegram, configure)
	i.telegramProbe = nop
	i.telegramTransport = &managedTelegramTransport{}
	spec.RecoveryCommitHash = s.CommitHash
	before, err := i.prepare(context.Background(), "adopt-recovery", spec)
	if err != nil {
		t.Fatal(err)
	}
	if telegram {
		before, err = i.prepareTelegramRenewal(context.Background(), spec.Root)
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := PrepareSignalVerification(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	spec.RecoveryCommitHash = ""
	spec.SignalVerification = &SignalVerificationReference{Root: s.Root, ManifestHash: p.ManifestHash}
	spec.RuntimeFile = filepath.Join(spec.SignalVerification.Root, signalVerificationGateway)
	return spec, i, f, p, before
}
func TestManagedSignalAdoptionLifecycle(t *testing.T) {
	spec, i, f, p, before := managedSignalFixture(t)
	ctx := context.Background()
	original, err := os.ReadFile(p.Spec.GatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	m, err := i.prepare(ctx, "adopt-signal", spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.SignalVerification == nil || m.SignalVerification.ManifestHash != p.ManifestHash || m.Identity != before.Identity || m.RecoveryCommitHash != before.RecoveryCommitHash || m.ConfigHash == before.ConfigHash || m.HelperRevision != m.Revision {
		t.Fatal(m)
	}
	stops := f.stops
	got, err := i.resume(ctx, spec.Root)
	if err != nil || protocol.Hash(got) != protocol.Hash(m) || stops != f.stops {
		t.Fatal("completed retry changed/stopped installation", err)
	}
	after, err := os.ReadFile(p.Spec.GatewayConfig)
	if err != nil || string(after) != string(original) {
		t.Fatal("old recovery input changed", err)
	}
	if _, _, _, err = reviewManagedRollback(spec.Root, before.Revision); err == nil {
		t.Fatal("rollback crossed Signal configuration transition")
	}
	spec.SignalVerification = nil
	binary, e := os.ReadFile(spec.BinaryFile)
	if e != nil {
		t.Fatal(e)
	}
	binary = append(binary, []byte("compatible-second-binary")...)
	if e = os.WriteFile(spec.BinaryFile, binary, 0700); e != nil {
		t.Fatal(e)
	}
	spec.BinarySHA256 = managedDigest(binary)
	updated, err := i.prepare(ctx, "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(updated.SignalVerification) != protocol.Hash(m.SignalVerification) {
		t.Fatal("update dropped Signal binding")
	}
	if _, _, _, err = reviewManagedRollback(spec.Root, m.Revision); err != nil {
		t.Fatal("compatible rollback denied", err)
	}
	rolled, e := i.rollback(ctx, spec.Root, m.Revision)
	if e != nil {
		t.Fatal(e)
	}
	if rolled.Revision == updated.Revision || rolled.BinaryHash != m.BinaryHash || protocol.Hash(rolled.SignalVerification) != protocol.Hash(m.SignalVerification) {
		t.Fatal("compatible rollback lost Signal binding")
	}
	i.signalCheck = func(context.Context, ManagedManifest) error {
		t.Fatal("uninstall required endpoint liveness")
		return errors.New("offline")
	}
	removed, err := i.uninstall(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(removed.SignalVerification) != protocol.Hash(m.SignalVerification) {
		t.Fatal("uninstall lost evidence")
	}
	if _, err = os.Stat(filepath.Join(p.Spec.Root, signalVerificationComplete)); err != nil {
		t.Fatal("uninstall removed separate endpoint", err)
	}
}
func TestManagedSignalAdoptionInterrupted(t *testing.T) {
	for _, point := range []string{"prepared", "stop-returned", "stopped", "unit-published", "manifest-published", "published", "reload-returned", "start-returned", "complete"} {
		t.Run(point, func(t *testing.T) {
			spec, i, _, p, _ := managedSignalFixture(t)
			stop := errors.New("interrupted")
			i.fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if _, err := i.prepare(context.Background(), "adopt-signal", spec); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			if m.SignalVerification == nil || m.SignalVerification.ManifestHash != p.ManifestHash {
				t.Fatal(m)
			}
		})
	}
}
func TestManagedSignalAdoptionRejectsChanges(t *testing.T) {
	spec, i, f, p, _ := managedSignalFixture(t)
	stops := f.stops
	bad := spec
	bad.SignalVerification = &SignalVerificationReference{Root: p.Spec.Root, ManifestHash: protocol.Hash("wrong")}
	if _, err := i.prepare(context.Background(), "adopt-signal", bad); err == nil || stops != f.stops {
		t.Fatal("changed endpoint stopped gateway", err)
	}
	bad = spec
	bad.RuntimeFile = p.Spec.GatewayConfig
	if _, err := i.prepare(context.Background(), "adopt-signal", bad); err == nil || stops != f.stops {
		t.Fatal("old config admitted", err)
	}
	nop := i.signalProbe
	i.signalProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old executable") }
	if _, err := i.prepare(context.Background(), "adopt-signal", spec); err == nil || stops != f.stops {
		t.Fatal("old capability admitted", err)
	}
	i.signalProbe = nop
	check := i.signalCheck
	i.signalCheck = func(context.Context, ManagedManifest) error { return errors.New("offline endpoint") }
	if _, err := i.prepare(context.Background(), "adopt-signal", spec); err == nil || stops != f.stops {
		t.Fatal("offline endpoint stopped gateway", err)
	}
	i.signalCheck = check
	m, err := i.prepare(context.Background(), "adopt-signal", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(spec.Root, "operations", m.SignalVerification.Operation, "signal-endpoint.json")); err != nil {
		t.Fatal(err)
	}
	stops = f.stops
	if _, err = i.resume(context.Background(), spec.Root); err == nil || stops != f.stops {
		t.Fatal("lost retained endpoint repaired", err)
	}
}
func TestManagedSignalAdoptionDriftDuringStop(t *testing.T) {
	spec, i, f, p, _ := managedSignalFixture(t)
	f.onStop = func() {
		if err := os.Remove(filepath.Join(p.Spec.Root, signalVerificationIntent)); err != nil {
			t.Fatal(err)
		}
	}
	starts := f.starts
	if _, err := i.prepare(context.Background(), "adopt-signal", spec); err == nil {
		t.Fatal("lost endpoint proof admitted")
	}
	if f.starts != starts {
		t.Fatal("gateway restarted with lost proof")
	}
}

func TestManagedSignalPreservesTelegramRenewal(t *testing.T) {
	spec, i, _, _, before := managedSignalFixtureProviders(t, true)
	original := managedTelegramReceiptForTest(t, before)
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(c.Telegram.TokenFile); err != nil {
		t.Fatal(err)
	}
	m, err := i.prepare(context.Background(), "adopt-signal", spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.TelegramRenewalID != before.TelegramRenewalID {
		t.Fatal("Signal adoption dropped Telegram renewal")
	}
	if got := managedTelegramReceiptForTest(t, m); protocol.Hash(got) != protocol.Hash(original) {
		t.Fatal("Signal adoption rewrote Telegram evidence")
	}
	if _, err = i.resume(context.Background(), spec.Root); err != nil {
		t.Fatal(err)
	}
	spec.SignalVerification = nil
	m, err = i.prepare(context.Background(), "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := managedTelegramReceiptForTest(t, m); protocol.Hash(got) != protocol.Hash(original) {
		t.Fatal("update lost Telegram bridge")
	}
	transport := i.telegramTransport.(*managedTelegramTransport)
	if transport.calls != 3 {
		t.Fatal("adoption or update contacted provider", transport.calls)
	}
	if err = os.WriteFile(c.Telegram.TokenFile, []byte("123:fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	renewed, err := i.prepareTelegramRenewal(context.Background(), spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.TelegramRenewalID == m.TelegramRenewalID || transport.calls != 6 || protocol.Hash(renewed.SignalVerification) != protocol.Hash(m.SignalVerification) {
		t.Fatal("renewal lost Signal binding", renewed, transport.calls)
	}
	managedTelegramReceiptForTest(t, renewed)
}
