package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func TestManagedSignalRepeatedRecovery(t *testing.T) {
	testManagedSignalRepeatedRecovery(t, nil)
}
func testManagedSignalRepeatedRecovery(t *testing.T, configure func(*ManagedSpec, *SignalServiceSpec, *managedInstaller, *fakeManagedServices, SignalVerificationManifest) signalServiceInstaller) {
	ctx := context.Background()
	var signalInstaller signalServiceInstaller
	spec, installer, _, m, service, qualification := managedSignalDeliveryFixtureUsing(t, func(spec *ManagedSpec, s *SignalServiceSpec, i *managedInstaller, f *fakeManagedServices, endpoint SignalVerificationManifest) signalServiceInstaller {
		signalInstaller = signalServiceInstaller{manager: &fakeSignalServiceManager{units: s.UnitDirectory}, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
		if configure != nil {
			signalInstaller = configure(spec, s, i, f, endpoint)
		}
		return signalInstaller
	})
	installer.nextRecoveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	originalProfile := filepath.Join(m.Root, "releases", m.Revision, "signal-profiles.json")
	if configure != nil {
		installer.probe = probeManagedBinary
		installer.signalProbe, installer.signalProfilesProbe, installer.deliveryProbe, installer.deliveryRecoveryProbe, installer.commandProbe, installer.commandRecoveryProbe, installer.nextRecoveryProbe = nil, nil, nil, nil, nil, nil, nil
	}
	for epoch := 2; epoch <= 3; epoch++ {
		active, err := installer.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(qualification), false)
		if err != nil {
			t.Fatal(err)
		}
		m = active
		m, err = installer.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(qualification), "read", false)
		if err != nil {
			t.Fatal("enable", epoch, err)
		}
		if err = installer.services.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		c, _ := effectiveRuntimeForTest(t, m)
		s, err := openManagedStore(m, c)
		if err != nil {
			t.Fatal(err)
		}
		artifact := filepath.Join(t.TempDir(), "snapshot")
		snapshot, err := s.Snapshot(ctx, artifact)
		s.Close()
		if err != nil {
			t.Fatal("snapshot", err)
		}
		m, err = installer.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
		if err != nil {
			t.Fatal("disable", epoch, err)
		}
		m, err = installer.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(m), "", false)
		if err != nil {
			t.Fatal(err)
		}
		q, err := installer.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false)
		if err != nil {
			t.Fatal("prepare", err)
		}
		if _, err = signalInstaller.uninstall(ctx, service.Spec.Root); err != nil {
			t.Fatal("remove Signal", err)
		}
		args := []string{"recovery-signal-history", "--local", "--config", spec.RuntimeFile, "--state-dir", spec.State, "--operation", q.Operation, "--relay-state", service.Endpoint.Installation.Destination, "--installation-hash", service.Endpoint.Installation.InstallationHash, "--receivers-stopped"}
		if err = runRecoverySignalIntakeCLI(args, &bytes.Buffer{}); err != nil {
			t.Fatal("history", err)
		}
		for _, p := range c.Worker.Producers {
			if _, err = worker.FenceRecoveryOwnership(ctx, spec.State, c.Worker, p.Producer); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = worker.SealPinnedRecoveryRequests(ctx, spec.State, c.Worker, q.Operation); err != nil {
			t.Fatal("seal", err)
		}
		args[0] = "recovery-signal-cursor"
		if err = runRecoverySignalIntakeCLI(args, &bytes.Buffer{}); err != nil {
			t.Fatal("cursor", err)
		}
		publication := publishPreparedManagedRecovery(t, spec.State, c.Worker, q.Operation)
		op, err := safefs.NewID("n")
		if err != nil {
			t.Fatal(err)
		}
		ref := m.SignalVerification.SignalVerificationReference
		handoff := ManagedSignalRecoverySpec{Version: 1, Operation: op, GatewayConfig: spec.RuntimeFile, GatewaySHA256: m.RuntimeHash, GatewayState: spec.State, CommitHash: publication.Commit.CommitHash, RelayDestination: t.TempDir(), Previous: ref}
		if err = os.Chmod(handoff.RelayDestination, 0700); err != nil {
			t.Fatal(err)
		}
		c, previous, request, err := handoff.prepare()
		if err != nil {
			t.Fatal("handoff input", err)
		}
		svc := managedSignalRecoveryService{ref: ref, manager: signalInstaller.manager, probe: func(context.Context, *safefs.Directory, string, string, string) error { return nil }}
		installed, status, err := InstallNextSignalRecoveryWithService(ctx, c, request, previous.Endpoint.Spec.Recovery, svc)
		if err != nil {
			t.Fatal("install next", err)
		}
		if installed.Version != 2 || installed.Ancestor != service.Endpoint.Installation.InstallationHash {
			t.Fatal("missing installed ancestor", installed)
		}
		if err = signalaccount.VerifyRecoveryAncestor(request.AccountState, status, previous.Endpoint.Spec.Recovery); err != nil {
			t.Fatal(err)
		}
		endpointSpec := previous.Endpoint.Spec
		endpointSpec.Operation, err = safefs.NewID("n")
		if err != nil {
			t.Fatal(err)
		}
		endpointSpec.Root = t.TempDir()
		if err = os.Chmod(endpointSpec.Root, 0700); err != nil {
			t.Fatal(err)
		}
		endpointSpec.GatewayConfig, endpointSpec.GatewaySHA256 = spec.RuntimeFile, m.RuntimeHash
		endpointSpec.CommitHash, endpointSpec.InboxDestination, endpointSpec.Recovery = request.CommitHash, request.RelayDestination, status
		endpoint, err := PrepareSignalVerification(ctx, endpointSpec)
		if err != nil {
			t.Fatal("endpoint", err)
		}
		nextService := signalServiceSpecFor(t, endpoint, spec.UnitDirectory, spec.BinaryFile)
		nextService.Runtime = service.Spec.Runtime
		nextManifest, err := signalInstaller.setup(ctx, nextService)
		if err != nil {
			t.Fatal("next service", err)
		}
		installer.signalCheck = func(ctx context.Context, manifest ManagedManifest) error {
			current, err := inspectSignalService(manifest.SignalVerification.SignalVerificationReference)
			if err != nil {
				return err
			}
			return signalInstaller.manager.Verify(ctx, current)
		}
		spec.RecoveryCommitHash = publication.Commit.CommitHash
		spec.SignalVerification = &SignalVerificationReference{Root: endpoint.Spec.Root, ManifestHash: endpoint.ManifestHash, ServiceRoot: nextManifest.Spec.Root, ServiceHash: nextManifest.ManifestHash}
		spec.RuntimeFile = filepath.Join(endpoint.Spec.Root, signalVerificationGateway)
		interrupted := errors.New("next Signal adoption interrupted")
		if configure == nil {
			point := "recovery-adopted"
			if epoch == 3 {
				point = "published"
			}
			installer.fault = func(at string) error {
				if at == point {
					return interrupted
				}
				return nil
			}
		}
		after, err := installer.prepare(ctx, "adopt-recovery", spec)
		installer.fault = nil
		if configure == nil {
			if !errors.Is(err, interrupted) {
				t.Fatal("missing adoption interruption", epoch, err)
			}
			after, err = installer.resume(ctx, spec.Root)
		}
		if err != nil {
			t.Fatal("next adoption", err)
		}
		canonical, effective := effectiveRuntimeForTest(t, after)
		if after.SignalProfiles == nil || after.SignalProfiles.Hash == m.SignalProfiles.Hash || effective.Providers["signal"].Socket != canonical.Signal.Socket || after.Identity == m.Identity || after.DeliveryRecovery.Owner != m.DeliveryRecovery.Owner {
			t.Fatal("lost next Signal profile/owner transition")
		}
		if _, err = installer.resume(ctx, spec.Root); err != nil {
			t.Fatal("resume", err)
		}
		m, service = after, nextManifest
		var out bytes.Buffer
		if err = runQualificationCLI([]string{"qualification-template", "--local", "--root", spec.Root}, &out); err != nil {
			t.Fatal(err)
		}
		var report QualificationReport
		if err = json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		qualificationPass(&report)
		report.Environment.SignalCLISHA256 = service.Spec.Runtime.CLISHA256
		statusRecord, err := qualifyFixture(t, spec.Root, report)
		if err != nil {
			t.Fatal("qualify next", epoch, err)
		}
		qualification = *statusRecord.Record
	}
	final, err := installer.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(qualification), false)
	if err != nil {
		t.Fatal("activate final", err)
	}
	final, err = installer.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(final), protocol.Hash(qualification), "read", false)
	if err != nil {
		t.Fatal("enable final", err)
	}
	if err = installer.services.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	canonical, _ := effectiveRuntimeForTest(t, final)
	store, err := openManagedStore(final, canonical)
	if err != nil {
		t.Fatal("open final", err)
	}
	store.Close()
	if configure == nil {
		if err = os.Remove(originalProfile); err != nil {
			t.Fatal(err)
		}
		if reopened, err := openManagedStore(final, canonical); err == nil {
			reopened.Close()
			t.Fatal("lost original Signal profile proof accepted in third epoch")
		}
	}

}

func TestManagedSignalNextRecoveryService(t *testing.T) {
	s, installer, manager := signalServiceFixture(t)
	ctx := context.Background()
	m, err := installer.setup(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	ref := SignalVerificationReference{Root: m.Endpoint.Spec.Root, ManifestHash: m.Endpoint.ManifestHash, ServiceRoot: m.Spec.Root, ServiceHash: m.ManifestHash}
	op, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	spec := ManagedSignalRecoverySpec{Version: 1, Operation: op, GatewayConfig: filepath.Join(ref.Root, signalVerificationGateway), GatewaySHA256: m.Endpoint.NextGatewaySHA256, GatewayState: m.Endpoint.Spec.GatewayState, CommitHash: protocol.Hash("next gateway publication"), RelayDestination: t.TempDir(), Previous: ref}
	if _, _, _, err = spec.prepare(); err == nil {
		t.Fatal("active predecessor accepted")
	}
	if _, err = installer.uninstall(ctx, m.Spec.Root); err != nil {
		t.Fatal(err)
	}
	c, prior, req, err := spec.prepare()
	if err != nil || c.Signal == nil || prior.ManifestHash != m.ManifestHash || req.RelaySource != m.Endpoint.Installation.Destination || req.AccountState != m.Endpoint.Spec.AccountState {
		t.Fatal(c.Signal, req, err)
	}
	probeCalls := 0
	svc := managedSignalRecoveryService{ref: ref, manager: manager, probe: func(_ context.Context, _ *safefs.Directory, name, hash, capability string) error {
		probeCalls++
		if name != "deploy" || hash != m.Spec.BinarySHA256 || capability != "__signal-next-recovery-probe" {
			return errors.New("wrong capability")
		}
		return nil
	}}
	if err = svc.VerifyNextRecoverySupport(ctx); err != nil || probeCalls != 1 {
		t.Fatal(err)
	}
	state, err := svc.Inspect(ctx)
	if err != nil || state.Active || state.Binding != m.ManifestHash {
		t.Fatal(state, err)
	}
	stops := manager.stops
	if err = svc.Stop(ctx); err != nil || manager.stops != stops || svc.Start(ctx) == nil {
		t.Fatal("handoff changed service lifecycle", err)
	}
	manager.state.Loaded = true
	if _, err = svc.Inspect(ctx); err == nil {
		t.Fatal("stale loaded service accepted")
	}
	manager.state.Loaded = false
	input := filepath.Join(t.TempDir(), "spec.json")
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	var out bytes.Buffer
	if err = runRecoverySignalManagedCLI([]string{"--local", "--spec", input}, &out); err != nil || !strings.Contains(out.String(), "[DRY]") {
		t.Fatal(out.String(), err)
	}
	status, err := signalaccount.InspectRecovery(req.AccountState)
	if err != nil || status != m.Endpoint.Spec.Recovery {
		t.Fatal("dry-run changed account", status, err)
	}
	spec.GatewaySHA256 = protocol.Hash("different config")
	if _, _, _, err = spec.prepare(); err == nil {
		t.Fatal("unrelated gateway config accepted")
	}
}

func TestSignalRecoveryIntakeDryRun(t *testing.T) {
	_, config := signalRuntimeCLIConfiguration(t)
	op, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state, relay := filepath.Join(root, "missing-gateway"), filepath.Join(root, "missing-inbox")
	args := []string{"recovery-signal-history", "--local", "--config", config, "--state-dir", state, "--operation", op, "--relay-state", relay, "--receivers-stopped"}
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	for _, command := range []string{"recovery-signal-history", "recovery-signal-cursor"} {
		args[0] = command
		var out bytes.Buffer
		if err = runRecoverySignalIntakeCLI(args, &out); err != nil || !strings.Contains(out.String(), "[DRY]") {
			t.Fatal(out.String(), err)
		}
		if err = runRecoverySignalIntakeCLI(args[:len(args)-1], &out); err == nil {
			t.Fatal("missing receiver acknowledgement accepted")
		}
		if err = runRecoverySignalIntakeCLI(append(args, "--restored", "--installation-hash", protocol.Hash("install")), &out); err == nil {
			t.Fatal("conflicting source types accepted")
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("dry-run created state", err)
	}
}
