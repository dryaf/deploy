package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func signalProfilesConfiguration(c *Runtime) {
	destination := c.Worker.Interaction.Destinations[0]
	c.Providers["signal"] = provider.Profile{Kind: "signal", Conversation: &destination, Destination: destination.Fingerprint(), Socket: c.Signal.Socket, RelayIdentity: c.Signal.InboxIdentity}
	route := c.Worker.Routes["ops"]
	route.Fallback = "signal"
	c.Worker.Routes["ops"] = route
}

func managedSignalProfilesFixture(t *testing.T, telegram bool, unitDirectories ...string) (ManagedSpec, managedInstaller, *fakeManagedServices, *fakeSignalServiceManager, ManagedManifest) {
	return managedSignalProfilesFixtureUsing(t, telegram, signalProfilesConfiguration, unitDirectories...)
}
func managedSignalProfilesFixtureUsing(t *testing.T, telegram bool, configure func(*Runtime), unitDirectories ...string) (ManagedSpec, managedInstaller, *fakeManagedServices, *fakeSignalServiceManager, ManagedManifest) {
	t.Helper()
	spec, i, f, endpoint, _ := managedSignalFixtureConfigured(t, telegram, configure, unitDirectories...)
	s := signalServiceSpecFor(t, endpoint, spec.UnitDirectory, spec.BinaryFile)
	sm := &fakeSignalServiceManager{units: s.UnitDirectory}
	si := signalServiceInstaller{manager: sm, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
	service, err := si.setup(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	spec.SignalVerification.ServiceRoot, spec.SignalVerification.ServiceHash = s.Root, service.ManifestHash
	i.signalCheck = func(ctx context.Context, _ ManagedManifest) error { return sm.Verify(ctx, service) }
	i.signalProfilesProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	before, err := i.prepare(context.Background(), "adopt-signal", spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.SignalVerification = nil
	raw, err := os.ReadFile(before.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	spec.RuntimeFile = filepath.Join(t.TempDir(), "profile-input.json")
	if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return spec, i, f, sm, before
}

func effectiveRuntimeForTest(t *testing.T, m ManagedManifest) (Runtime, Runtime) {
	t.Helper()
	d, err := privateManagedDir(m.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := managedEffectiveRuntime(d, m, c)
	if err != nil {
		t.Fatal(err)
	}
	return c, effective
}

func TestManagedSignalProfilesLifecycle(t *testing.T) {
	spec, i, f, sm, before := managedSignalProfilesFixture(t, true)
	ctx := context.Background()
	canonical := mustRuntime(t, before.Paths.Config)
	stateBefore := activationTreeHash(t, spec.State)
	oldConfig, err := os.ReadFile(before.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	receiptBefore := managedTelegramReceiptForTest(t, before)
	calls := i.telegramTransport.(*managedTelegramTransport).calls
	after, err := i.prepare(ctx, "migrate-signal-profiles", spec)
	if err != nil {
		t.Fatal(err)
	}
	if after.SignalProfiles == nil || after.ConfigHash != before.ConfigHash || after.RuntimeHash != before.RuntimeHash || after.Identity != before.Identity || after.RecoveryCommitHash != before.RecoveryCommitHash || after.TelegramRenewalID != before.TelegramRenewalID || after.HelperRevision != after.Revision {
		t.Fatal("migration changed canonical authority", after)
	}
	_, effective := effectiveRuntimeForTest(t, after)
	p := effective.Providers["signal"]
	if p.Socket != effective.Signal.Socket || p.RelayIdentity != effective.Signal.InboxIdentity || p.Conversation == nil || p.Destination != canonical.Providers["signal"].Destination || protocol.Hash(p.Conversation) != protocol.Hash(canonical.Providers["signal"].Conversation) || effective.Worker.Routes["ops"].Revision == canonical.Worker.Routes["ops"].Revision {
		t.Fatal("effective Signal transport/route was not migrated", effective)
	}
	if stateBefore != activationTreeHash(t, spec.State) || managedTelegramReceiptForTest(t, after) != receiptBefore || calls != i.telegramTransport.(*managedTelegramTransport).calls {
		t.Fatal("migration changed canonical state or Telegram evidence")
	}
	retained, err := os.ReadFile(before.Paths.Config)
	if err != nil || string(retained) != string(oldConfig) {
		t.Fatal("original runtime changed", err)
	}
	newCanonical, err := os.ReadFile(after.Paths.Config)
	if err != nil || string(newCanonical) != string(oldConfig) {
		t.Fatal("canonical recovery runtime changed", err)
	}
	s, err := openManagedStore(after, canonical)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(ctx)
	if err != nil || !h.VerificationOnly || h.SignalProfilesHash != after.SignalProfiles.Hash {
		t.Fatal(h, err)
	}
	if err = s.PullSignal(ctx, provider.SignalInboxClient{}); err == nil {
		t.Fatal("migration enabled intake")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.ReviewPublishedRecovery(ctx, spec.State, canonical.Worker, after.RecoveryCommitHash); err != nil {
		t.Fatal("original recovery proof no longer verifies", err)
	}
	if _, _, _, err = reviewManagedRollback(spec.Root, before.Revision); err == nil {
		t.Fatal("rollback crossed profile transition")
	}
	stops := f.stops
	if _, err = i.resume(ctx, spec.Root); err != nil || f.stops != stops {
		t.Fatal("completed retry restarted service", err)
	}
	if _, err = i.prepare(ctx, "migrate-signal-profiles", spec); err == nil {
		t.Fatal("duplicate migration accepted")
	}
	binary, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	binary = append(binary, []byte("compatible-signal-profile-update")...)
	if err = os.WriteFile(spec.BinaryFile, binary, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(binary)
	if _, err = i.prepare(ctx, "update", spec); err != nil {
		t.Fatal("update lost binding", err)
	}
	rolled, err := i.rollback(ctx, spec.Root, after.Revision)
	if err != nil || rolled.BinaryHash != after.BinaryHash || protocol.Hash(rolled.SignalProfiles) != protocol.Hash(after.SignalProfiles) {
		t.Fatal("compatible binary rollback lost profile proof", err)
	}
	if err = f.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	review, err := reviewActivation(ctx, spec.Root, f, sm, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	activationCheck(t, review, "provider/signal", "binding", "satisfied")
	activationCheck(t, review, "signal", "profiles", "satisfied")
	activationCheck(t, review, "signal", "activation", "blocked")
	if review.EffectiveConfigHash != protocol.Hash(effective) {
		t.Fatal("review lost effective config identity")
	}
	renewed, err := i.prepareTelegramRenewal(ctx, spec.Root)
	if err != nil || renewed.TelegramRenewalID == after.TelegramRenewalID || protocol.Hash(renewed.SignalProfiles) != protocol.Hash(after.SignalProfiles) {
		t.Fatal("Telegram successor lost Signal profile proof", err)
	}
	_, currentEffective := effectiveRuntimeForTest(t, renewed)
	if protocol.Hash(currentEffective) != protocol.Hash(effective) {
		t.Fatal("Telegram renewal changed effective routes")
	}
	if _, err = i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal("uninstall lost profile proof", err)
	}
	si := signalServiceInstaller{manager: sm}
	if _, err = si.uninstall(ctx, sm.manifest.Spec.Root); err != nil {
		t.Fatal("verifier removal after profile gateway", err)
	}
	if _, err = i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal("retired gateway lost profile proof after verifier removal", err)
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal("retired gateway resume after verifier removal", err)
	}
}

func TestManagedSignalProfilesInterrupted(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "stopped", "unit-published", "manifest-published", "reload-returned", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			spec, i, _, _, _ := managedSignalProfilesFixture(t, false)
			interrupted := errors.New("profile interruption")
			i.fault = func(at string) error {
				if at == boundary {
					return interrupted
				}
				return nil
			}
			if _, err := i.prepare(context.Background(), "migrate-signal-profiles", spec); !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			if err := os.Remove(spec.BinaryFile); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(spec.RuntimeFile); err != nil {
				t.Fatal(err)
			}
			i.fault = nil
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			_, c := effectiveRuntimeForTest(t, m)
			if c.Providers["signal"].RelayIdentity != c.Signal.InboxIdentity {
				t.Fatal("resume did not retain effective pins")
			}
		})
	}
}

func TestManagedSignalProfilesEvidenceLoss(t *testing.T) {
	for _, artifact := range []string{"signal-runtime.json", "signal-profiles.json", "intent.json", "before-runtime"} {
		t.Run(artifact, func(t *testing.T) {
			spec, i, f, _, before := managedSignalProfilesFixture(t, false)
			m, err := i.prepare(context.Background(), "migrate-signal-profiles", spec)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(spec.Root, "releases", m.Revision, artifact)
			if artifact == "intent.json" {
				path = filepath.Join(spec.Root, "operations", m.SignalProfiles.Operation, artifact)
			}
			if artifact == "before-runtime" {
				path = before.Paths.Config
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			stops := f.stops
			if _, err = i.resume(context.Background(), spec.Root); err == nil {
				t.Fatal("resume rebuilt lost proof")
			}
			if _, err = i.prepare(context.Background(), "update", spec); err == nil {
				t.Fatal("update ignored lost proof")
			}
			if _, err = i.uninstall(context.Background(), spec.Root); err == nil {
				t.Fatal("uninstall ignored lost proof")
			}
			if stops != f.stops {
				t.Fatal("evidence loss stopped service before refusal")
			}
		})
	}
}

func TestManagedSignalProfilesRejectsOldCapability(t *testing.T) {
	for _, target := range []string{"deploy", "recovery-helper"} {
		t.Run(target, func(t *testing.T) {
			spec, i, f, _, _ := managedSignalProfilesFixture(t, false)
			i.signalProfilesProbe = func(_ context.Context, _ *safefs.Directory, name, _ string) error {
				if filepath.Base(name) == target {
					return errors.New("old executable")
				}
				return nil
			}
			stops := f.stops
			if _, err := i.prepare(context.Background(), "migrate-signal-profiles", spec); err == nil || stops != f.stops {
				t.Fatal("old executable was admitted or gateway stopped", err)
			}
		})
	}
}

func TestManagedSignalProfilesCLIDryRun(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "systemd"), 0700); err != nil {
		t.Fatal(err)
	}
	spec, _, f, _, _ := managedSignalProfilesFixture(t, false, filepath.Join(home, "systemd", "user"))
	t.Setenv("XDG_CONFIG_HOME", home)
	path := filepath.Join(t.TempDir(), "migration.json")
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	stops := f.stops
	if err = RunCLI([]string{"migrate-signal-profiles", "--local", "--spec", path}); err != nil {
		t.Fatal(err)
	}
	if before != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) || stops != f.stops {
		t.Fatal("dry-run modified installation")
	}
	if err = RunCLI([]string{"migrate-signal-profiles", "--spec", path}); err == nil {
		t.Fatal("missing local flag accepted")
	}
	if err = RunCLI([]string{"__managed-signal-profiles-probe", "--local"}); err == nil {
		t.Fatal("probe accepted flags")
	}
	var runtime Runtime
	input, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(input, &runtime); err != nil {
		t.Fatal(err)
	}
	p := runtime.Providers["signal"]
	p.Socket = "/unproven/socket"
	runtime.Providers["signal"] = p
	writeRuntime(t, spec.RuntimeFile, runtime)
	if err = RunCLI([]string{"migrate-signal-profiles", "--local", "--spec", path}); err == nil {
		t.Fatal("dry-run accepted changed canonical profile bytes")
	}
}

func TestManagedSignalProfilesRefusesUnprovenSource(t *testing.T) {
	for _, kind := range []string{"legacy", "foreign-socket", "foreign-inbox", "no-profile"} {
		t.Run(kind, func(t *testing.T) {
			configure := func(c *Runtime) {
				signalProfilesConfiguration(c)
				p := c.Providers["signal"]
				switch kind {
				case "legacy":
					p.Conversation = nil
				case "foreign-socket":
					p.Socket = "/foreign/relay.sock"
				case "foreign-inbox":
					p.RelayIdentity.Recovery = "unrelated-inbox"
				case "no-profile":
					delete(c.Providers, "signal")
					route := c.Worker.Routes["ops"]
					route.Fallback = ""
					c.Worker.Routes["ops"] = route
					return
				}
				c.Providers["signal"] = p
			}
			spec, i, f, _, _ := managedSignalProfilesFixtureUsing(t, false, configure)
			stops := f.stops
			if _, err := i.prepare(context.Background(), "migrate-signal-profiles", spec); err == nil || f.stops != stops {
				t.Fatal("unproven source profile migrated or service stopped", err)
			}
		})
	}
}

func TestManagedSignalProfilesUnpublishedStaging(t *testing.T) {
	spec, i, f, _, before := managedSignalProfilesFixture(t, false)
	stops := f.stops
	interrupted := errors.New("lost staging response")
	i.fault = func(at string) error {
		if at == "signal-profiles-retained" {
			return interrupted
		}
		return nil
	}
	if _, err := i.prepare(context.Background(), "migrate-signal-profiles", spec); !errors.Is(err, interrupted) || stops != f.stops {
		t.Fatal("pre-intent staging changed gateway", err)
	}
	i.fault = nil
	m, err := i.resume(context.Background(), spec.Root)
	if err != nil || protocol.Hash(m) != protocol.Hash(before) {
		t.Fatal("unpublished staging claimed migration", err)
	}
	m, err = i.prepare(context.Background(), "migrate-signal-profiles", spec)
	if err != nil || m.SignalProfiles == nil {
		t.Fatal("unpublished staging blocked exact fresh request", err)
	}
}

func TestManagedSignalProfilesRuntimeCoordination(t *testing.T) {
	for _, kind := range []string{"root", "lease"} {
		t.Run(kind, func(t *testing.T) {
			spec, i, _, _, _ := managedSignalProfilesFixture(t, false)
			m, err := i.prepare(context.Background(), "migrate-signal-profiles", spec)
			if err != nil {
				t.Fatal(err)
			}
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			lease, err := d.Lock("installation.lock", false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if err := checkManagedSignalProfilesRuntime(d, m, lease); err != nil {
				t.Fatal(err)
			}
			if kind == "root" {
				if err := os.Rename(spec.Root, spec.Root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(spec.Root, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(spec.Root, "installation.lock")
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkManagedSignalProfilesRuntime(d, m, lease); err == nil {
				t.Fatal("runtime accepted replaced coordination authority")
			}
		})
	}
}
