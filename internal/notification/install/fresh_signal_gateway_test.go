package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func freshSignalGatewayFixture(t *testing.T, unbound bool, telegram ...bool) (ManagedSpec, managedInstaller, Runtime, FreshSignalManifest) {
	t.Helper()
	binary := freshSignalTestBinary(t)
	fresh, installer, _ := freshSignalFixture(t, binary)
	spec, i, services, _ := managedFixture(t)
	fresh.UnitDirectory = spec.UnitDirectory
	installer.manager = &fakeFreshSignalManager{units: spec.UnitDirectory}
	signal, err := installer.setup(context.Background(), fresh)
	testutil.Must(t, err)
	spec.FreshSignal = &FreshSignalReference{Root: signal.Spec.Root, ManifestHash: signal.ManifestHash}
	health, err := freshSignalHealth(signal)
	testutil.Must(t, err)
	raw, err := os.ReadFile(spec.RuntimeFile)
	testutil.Must(t, err)
	var input RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
	input.Providers = map[string]provider.Profile{"signal": {Kind: "signal", Socket: filepath.Join(fresh.Root, "api.sock"), Destination: health.Destination}}
	for name, route := range input.Worker.Routes {
		route.Primary = "signal"
		route.Fallback = ""
		input.Worker.Routes[name] = route
	}
	if len(telegram) > 0 && telegram[0] {
		p := provider.Profile{Kind: "telegram", TokenFile: filepath.Join(t.TempDir(), "token"), ChatFile: filepath.Join(t.TempDir(), "chat")}
		testutil.Must(t, os.WriteFile(p.TokenFile, []byte("123:retained_native_fixture"), 0600))
		testutil.Must(t, os.WriteFile(p.ChatFile, []byte("chat-one"), 0600))
		p.Destination = provider.TelegramIdentity("123:retained_native_fixture", "chat-one")
		input.Providers["telegram"] = p
		for name, route := range input.Worker.Routes {
			route.Primary, route.Fallback = "telegram", "signal"
			input.Worker.Routes[name] = route
		}
	}
	input.Bootstrap = unbound
	if unbound {
		input.Worker.Producers = nil
	}
	c, err := input.Resolve()
	testutil.Must(t, err)
	raw, err = input.Bytes()
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
	spec.State = filepath.Join(filepath.Dir(spec.State), "fresh-state")
	if unbound {
		_, err = worker.BootstrapGateway(context.Background(), spec.State, testutil.ID(t, "n"), c.Worker)
		testutil.Must(t, err)
	} else {
		testutil.Must(t, os.Mkdir(spec.State, 0700))
		store, err := worker.Open(spec.State, c.Worker, true)
		testutil.Must(t, err)
		for _, p := range c.Worker.Producers {
			testutil.Must(t, store.Stage(p.Producer, "one"))
		}
		testutil.Must(t, store.Close())
	}
	i.bootstrapProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.signalProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.signalCheck = func(ctx context.Context, m ManagedManifest) error {
		_, err := InspectFreshSignal(ctx, *m.FreshSignal, false)
		return err
	}
	delivery := &fakeDeliveryServices{services}
	i.services = delivery
	i.deliveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.deliveryInspect = func(ctx context.Context, m ManagedManifest, _ bool) error {
		_, err := inspectManagedService(ctx, delivery, m, false)
		return err
	}
	return spec, i, c, signal
}
func TestFreshSignalGatewayQualificationAndDelivery(t *testing.T) {
	spec, i, c, signal := freshSignalGatewayFixture(t, false)
	ctx := context.Background()
	before, err := i.ensureVerification(ctx, spec)
	testutil.Must(t, err)
	if before.RecoveryCommitHash != "" || before.SignalVerification != nil || before.FreshSignal == nil {
		t.Fatal("fresh gateway claimed recovered ownership")
	}
	_, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(before))
	testutil.Must(t, err)
	if ready {
		t.Fatal("service readiness fabricated delivery qualification")
	}
	session, err := BeginQualification(ctx, spec.Root, protocol.Hash(before))
	testutil.Must(t, err)
	for _, check := range session.Checks {
		_, err = ObserveQualification(ctx, spec.Root, protocol.Hash(before), QualificationObservation{Session: session.ID, Scope: check.Scope, ID: check.ID, Result: "passed", EvidenceHash: protocol.Hash("synthetic test observation")})
		testutil.Must(t, err)
	}
	session, err = CommitQualification(ctx, spec.Root, protocol.Hash(before), session.ID)
	testutil.Must(t, err)
	if !session.EvidenceComplete {
		t.Fatal("exact observations were not complete")
	}
	review, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(before))
	testutil.Must(t, err)
	if !ready {
		t.Fatal("qualified first-install Signal gateway remained blocked")
	}
	after, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, review.ManifestHash, review.QualificationHash, false)
	testutil.Must(t, err)
	if after.Mode != "delivery" || after.FreshSignal.ManifestHash != signal.ManifestHash {
		t.Fatal("delivery changed fresh service ownership")
	}
	store, err := openManagedStore(after, c)
	testutil.Must(t, err)
	testutil.Must(t, store.BindFreshSignalService(signal.ManifestHash))
	h, err := store.Health(ctx)
	testutil.Must(t, err)
	if h.FreshSignalServiceHash != signal.ManifestHash || h.SignalVerificationHash != "" || h.RecoveryCommitHash != "" {
		t.Fatal("fresh health fabricated recovery")
	}
	testutil.Must(t, store.Close())
	// Deactivation keeps the original service/account reference.
	deactivated, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(after), "", false)
	testutil.Must(t, err)
	if protocol.Hash(deactivated.FreshSignal) != protocol.Hash(before.FreshSignal) {
		t.Fatal("deactivation discarded native account owner")
	}
}
func TestFreshSignalUnboundGatewayRetainsServiceReference(t *testing.T) {
	spec, i, _, _ := freshSignalGatewayFixture(t, true)
	before, err := i.ensureVerification(context.Background(), spec)
	testutil.Must(t, err)
	services := i.services.(*fakeDeliveryServices)
	starts := services.starts
	got, err := i.ensureVerification(context.Background(), spec)
	testutil.Must(t, err)
	if protocol.Hash(got) != protocol.Hash(before) || services.starts != starts {
		t.Fatal("unbound retry changed gateway")
	}
	changed := spec
	changed.FreshSignal = nil
	if _, err = i.ensureVerification(context.Background(), changed); err == nil {
		t.Fatal("omitted Signal owner accepted")
	}
	if _, err = i.prepareExpected(context.Background(), "update", changed, protocol.Hash(before)); err == nil {
		t.Fatal("update discarded native service")
	}
	_, ready, err := ReviewDeliveryAdmission(context.Background(), spec.Root, protocol.Hash(before))
	testutil.Must(t, err)
	if ready {
		t.Fatal("unbound Signal gateway admitted delivery")
	}
}
func TestFreshSignalGatewayRejectsForeignProfile(t *testing.T) {
	spec, i, _, _ := freshSignalGatewayFixture(t, true)
	raw, err := os.ReadFile(spec.RuntimeFile)
	testutil.Must(t, err)
	var input RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
	profile := input.Providers["signal"]
	profile.Destination = protocol.Hash("foreign destination")
	input.Providers["signal"] = profile
	raw, err = input.Bytes()
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
	if _, err = i.ensureVerification(context.Background(), spec); err == nil {
		t.Fatal("foreign profile admitted")
	}
	if _, err = os.Stat(spec.Root); !os.IsNotExist(err) {
		t.Fatal(fmt.Sprintf("invalid profile mutated installation: %v", err))
	}
}

func TestFreshSignalGatewayRemovalRetainsHistoricalDependency(t *testing.T) {
	spec, i, _, service := freshSignalGatewayFixture(t, true)
	ctx := context.Background()
	m, err := i.ensureVerification(ctx, spec)
	testutil.Must(t, err)
	removed, err := i.uninstallExpected(ctx, spec.Root, protocol.Hash(m))
	testutil.Must(t, err)
	if removed.Mode != "uninstalled" {
		t.Fatal(removed.Mode)
	}
	manager := &fakeFreshSignalManager{units: service.Spec.UnitDirectory, state: signalServiceState{Loaded: true, Active: true, Enabled: true, PID: 123}}
	child := freshSignalInstaller{manager: manager}
	_, err = child.uninstall(ctx, service.reference())
	testutil.Must(t, err)
	// Retained gateway inspection verifies history without asking the removed
	// child to run; live gateway admission still refuses that child.
	observation, err := i.observeInstallation(ctx, spec.Root, false)
	testutil.Must(t, err)
	if observation.Manifest.Mode != "uninstalled" {
		t.Fatal("removed gateway reported live")
	}
	_, err = i.uninstallExpected(ctx, spec.Root, protocol.Hash(m))
	testutil.Must(t, err)
	if _, err = InspectFreshSignal(ctx, service.reference(), false); err == nil {
		t.Fatal("retired dependency admitted as live")
	}
}
