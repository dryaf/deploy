package install

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"time"
)

func managedSignalDeliveryFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeDeliveryServices, ManagedManifest, SignalServiceManifest, QualificationRecord) {
	return managedSignalDeliveryFixtureUsing(t, nil)
}
func managedSignalDeliveryFixtureUsing(t *testing.T, configure func(*ManagedSpec, *SignalServiceSpec, *managedInstaller, *fakeManagedServices, SignalVerificationManifest) signalServiceInstaller) (ManagedSpec, managedInstaller, *fakeDeliveryServices, ManagedManifest, SignalServiceManifest, QualificationRecord) {
	return managedSignalDeliveryFixtureEnrolled(t, configure, false)
}
func managedSignalDeliveryFixtureEnrolled(t *testing.T, configure func(*ManagedSpec, *SignalServiceSpec, *managedInstaller, *fakeManagedServices, SignalVerificationManifest) signalServiceInstaller, legacy bool) (ManagedSpec, managedInstaller, *fakeDeliveryServices, ManagedManifest, SignalServiceManifest, QualificationRecord) {
	t.Helper()
	spec, i, f, endpoint, _ := managedSignalFixtureWithProducerRecoveryBootstrap(t, false, func(c *Runtime) {
		signalProfilesConfiguration(c)
		if legacy {
			legacySignalProfile(c)
		}
		delete(c.Providers, "telegram")
		route := c.Worker.Routes["ops"]
		route.Primary = "signal"
		route.Fallback = ""
		c.Worker.Routes["ops"] = route
		dest := c.Worker.Interaction.Destinations[0]
		c.Worker.Interaction.Permissions = []interaction.Permission{{Actor: dest.NativeID, Conversation: dest.ID, Worker: c.Worker.Producers[0].Producer.ID, Permissions: []string{"read"}, Scope: interaction.Scope{WholeWorker: true}}}
	}, func(ctx context.Context, path string, c worker.Config, before protocol.Identity) {
		r, err := worker.OpenRecoveryGenerationSession(ctx, path, c)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		session := r.(protocol.RecoverySession)
		ownership := r.(worker.RecoveryOwnershipSession)
		for _, enrollment := range c.Producers {
			p := enrollment.Producer
			challenge, err := session.Challenge(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if err = session.Begin(ctx, challenge, protocol.RecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: before, Producer: p}}); err != nil {
				t.Fatal(err)
			}
			snapshot := protocol.Snapshot{Producer: p, Sequence: 1, BasisRevision: 1, ConfigRevision: "native", EvaluatedAt: challenge.IssuedAt, Subjects: []protocol.Subject{{Key: "fixture", Condition: "ok", Reason: "synthetic recovery", ValidUntil: challenge.IssuedAt.Add(time.Minute)}}}
			if _, err = session.Finish(ctx, challenge, snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err = ownership.PrepareRecoveryOwnership(ctx, p); err != nil {
				t.Fatal(err)
			}
			if _, err = r.PrepareRecoveryGeneration(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		if err = r.Close(); err != nil {
			t.Fatal(err)
		}
		commands, err := worker.OpenCommandRecoveryInstallSession(ctx, path, c)
		if err != nil {
			t.Fatal(err)
		}
		defer commands.Close()
		for _, enrollment := range c.Producers {
			db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "commands.db")+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			ledger, err := interaction.OpenLedger(ctx, db, enrollment.Producer, before, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = interaction.ReconcileCommands(ctx, ledger, commands); err != nil {
				t.Fatal(err)
			}
			if _, err = interaction.CommitCommandRecovery(ctx, ledger, commands); err != nil {
				t.Fatal(err)
			}
			if _, err = interaction.InstallCommandRecovery(ctx, ledger, commands); err != nil {
				t.Fatal(err)
			}
		}
	}, legacy)
	s := signalServiceSpecFor(t, endpoint, spec.UnitDirectory, spec.BinaryFile)
	c, err := decodeRuntime(endpoint.OriginalConfig)
	if err != nil {
		t.Fatal(err)
	}
	selector := filepath.Join(t.TempDir(), "account")
	raw := []byte(c.Signal.Account + "\n")
	if err = os.WriteFile(selector, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s.Runtime = &SignalServiceRuntime{CLIFile: spec.BinaryFile, CLISHA256: spec.BinarySHA256, CLIVersion: "0.14.9", AccountFile: selector, AccountSHA256: managedDigest(raw)}
	sm := &fakeSignalServiceManager{units: s.UnitDirectory}
	si := signalServiceInstaller{manager: sm, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
	if configure != nil {
		si = configure(&spec, &s, &i, f, endpoint)
	}
	service, err := si.setup(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	spec.SignalVerification.ServiceRoot, spec.SignalVerification.ServiceHash = s.Root, service.ManifestHash
	i.signalCheck = func(ctx context.Context, _ ManagedManifest) error { return si.manager.Verify(ctx, service) }
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.signalProfilesProbe, i.deliveryProbe, i.deliveryRecoveryProbe, i.commandProbe, i.commandRecoveryProbe = nop, nop, nop, nop, nop
	m, err := i.prepare(context.Background(), "adopt-signal", spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.SignalVerification = nil
	raw, err = os.ReadFile(m.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	spec.RuntimeFile = filepath.Join(t.TempDir(), "runtime.json")
	if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if legacy {
		spec.SignalConversations = map[string]SignalConversationEnrollment{"signal": {PreviousDestination: c.Providers["signal"].Destination, Conversation: c.Worker.Interaction.Destinations[0].ID}}
	}
	m, err = i.prepare(context.Background(), "migrate-signal-profiles", spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.SignalConversations = nil
	delivery := &fakeDeliveryServices{f}
	if i.services == f {
		i.services = delivery
	}
	if i.deliveryInspect == nil {
		i.deliveryInspect = func(ctx context.Context, m ManagedManifest, _ bool) error {
			_, err := inspectManagedService(ctx, delivery, m, false)
			return err
		}
	}
	var out bytes.Buffer
	if err = runQualificationCLI([]string{"qualification-template", "--local", "--root", spec.Root}, &out); err != nil {
		t.Fatal(err)
	}
	var report QualificationReport
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	qualificationPass(&report)
	report.Environment.SignalCLISHA256 = s.Runtime.CLISHA256
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil {
		t.Fatal(err)
	}
	return spec, i, delivery, m, service, *status.Record
}

func TestManagedSignalDeliveryLifecycle(t *testing.T) {
	spec, i, _, before, _, record := managedSignalDeliveryFixture(t)
	ctx := context.Background()
	after, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	c, effective := effectiveRuntimeForTest(t, after)
	s, err := openManagedStore(after, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = requireDeliveryOwner(s, after, c); err != nil {
		t.Fatal("effective routes invalidated ownership", err)
	}
	if s.VerificationOnly() || after.Commands != nil {
		t.Fatal("wrong delivery/command admission")
	}
	if err = s.RequireInactiveProducers(); err != nil {
		t.Fatal("global activation enabled producer", err)
	}
	if effective.Providers["signal"].Socket != c.Signal.Socket || effective.Worker.Routes["ops"].Revision == c.Worker.Routes["ops"].Revision {
		t.Fatal("effective routes not migrated")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
	commands, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(after), protocol.Hash(record), "read", false)
	if err != nil {
		t.Fatal("Signal command enable", err)
	}
	opened, err := openManagedStore(commands, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = opened.RequireManagedSignalHandoff(ctx, *c.Signal); err != nil {
		t.Fatal(err)
	}
	health, err := opened.Health(ctx)
	if err != nil || health.CommandMode != "read" {
		t.Fatal(health, err)
	}
	opened.Close()
	drain, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(commands), "", "", false)
	if err != nil {
		t.Fatal("Signal command disable", err)
	}
	if drain.Commands == nil || drain.Commands.Mode != "drain" {
		t.Fatal(drain)
	}
	inactive, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(drain), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if inactive.Mode != "verification" || inactive.DeliveryOwnerHash != after.DeliveryOwnerHash {
		t.Fatal(inactive)
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
}

func TestManagedSignalDeliveryRejectsVerifierAndCLIChanges(t *testing.T) {
	spec, i, _, before, service, record := managedSignalDeliveryFixture(t)
	if err := os.WriteFile(filepath.Join(service.Spec.Root, "signal-cli"), []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false); err == nil {
		t.Fatal("changed native CLI admitted")
	}
	_, _, _, _, verifier := managedSignalProfilesFixture(t, false)
	c := mustRuntime(t, verifier.Paths.Config)
	if err := deliveryPolicy(verifier, c); err == nil {
		t.Fatal("verification service granted delivery")
	}
}
