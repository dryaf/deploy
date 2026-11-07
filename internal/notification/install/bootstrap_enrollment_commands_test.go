package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func managedCommandEnrollmentFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeManagedServices, ManagedManifest, RuntimeInput) {
	t.Helper()
	spec, i, service, _ := managedFixture(t)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	raw, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var input RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	producers := input.Worker.Producers
	input.Bootstrap, input.Worker.Producers = true, nil
	c, err := input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	spec.State = filepath.Join(filepath.Dir(spec.State), "unbound")
	op, _ := safefs.NewID("n")
	if _, err = worker.BootstrapGateway(context.Background(), spec.State, op, c.Worker); err != nil {
		t.Fatal(err)
	}
	raw, _ = input.Bytes()
	if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.bootstrapProbe, i.bootstrapEnrollmentCommandsProbe, i.commandProbe = probe, probe, probe
	before, err := i.ensureVerification(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	input.Bootstrap, input.Worker.Producers = false, producers
	p := producers[0].Producer
	d := interaction.Destination{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}
	input.Telegram = &provider.TelegramAccount{Account: "123", TokenFile: input.Providers["telegram"].TokenFile, BotName: "ExampleBot"}
	input.Worker.Interaction = &interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/manager", Producer: p, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "backup.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "status"}}}}, Destinations: []interaction.Destination{d}, Permissions: []interaction.Permission{{Actor: "789", Conversation: d.ID, Worker: p.ID, Permissions: []string{"read"}, Scope: interaction.Scope{WholeWorker: true}}}}
	return spec, i, service, before, input
}

func TestManagedBootstrapCommandEnrollment(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "bootstrap-enrolled", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, service, before, input := managedCommandEnrollmentFixture(t)
			raw, _ := input.Bytes()
			if err := os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			probe := i.bootstrapEnrollmentCommandsProbe
			i.bootstrapEnrollmentCommandsProbe = func(context.Context, *safefs.Directory, string, string) error {
				return errors.New("unsupported helper")
			}
			stops := service.stops
			if _, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); err == nil || service.stops != stops {
				t.Fatal("old helper stopped service", err)
			}
			i.bootstrapEnrollmentCommandsProbe = probe
			stop := errors.New("interrupted enrollment")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); !errors.Is(err, stop) {
				t.Fatal("missed boundary", err)
			}
			i.fault = nil
			input.Worker.Interaction.Workers[0].Alias = "backup/other"
			changed, _ := input.Bytes()
			if err := os.WriteFile(spec.RuntimeFile, changed, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); err == nil {
				t.Fatal("retargeted command aliases")
			}
			input.Worker.Interaction.Workers[0].Alias = "backup/manager"
			if err := os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			after, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before))
			if err != nil {
				t.Fatal(err)
			}
			starts := service.starts
			if again, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); err != nil || protocol.Hash(again) != protocol.Hash(after) || service.starts != starts {
				t.Fatal("completed retry changed service", err)
			}
			if after.Identity != before.Identity || after.Mode != "verification" || after.Commands != nil || after.DeliveryActivation != nil {
				t.Fatal("enrollment admitted activity")
			}
			c, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := openManagedStore(after, c)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RequireInactiveProducers(); err != nil {
				t.Fatal(err)
			}
			d := c.Worker.Interaction.Destinations[0]
			in := interaction.Inbound{Provider: d.Provider, Account: d.Account, NativeID: "first", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup/manager status"}
			if _, err = s.AdmitCommand(ctx, in); err == nil {
				t.Fatal("enrollment admitted command")
			}
			s.Close()
			if _, ready, err := ReviewCommandAdmission(ctx, spec.Root, protocol.Hash(after), "read"); err == nil || ready {
				t.Fatal("verification admitted commands")
			}
			if _, err = i.inspectInstallation(ctx, spec.Root); err != nil {
				t.Fatal(err)
			}
			// Even a completed retry must reject a helper that cannot interpret v3.
			i.bootstrapEnrollmentCommandsProbe = func(context.Context, *safefs.Directory, string, string) error {
				return errors.New("unsupported helper")
			}
			if _, err = i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); err == nil {
				t.Fatal("completed enrollment accepted old helper")
			}
		})
	}
}

func TestManagedBootstrapCommandEnrollmentRejectsMixedPolicy(t *testing.T) {
	for _, change := range []string{"credential", "route", "budget", "account", "intake"} {
		t.Run(change, func(t *testing.T) {
			spec, i, service, before, input := managedCommandEnrollmentFixture(t)
			switch change {
			case "credential":
				input.Telegram.TokenFile = filepath.Join(filepath.Dir(spec.RuntimeFile), "new-token")
			case "route":
				r := input.Worker.Routes["ops"]
				r.Revision = "other"
				input.Worker.Routes["ops"] = r
			case "budget":
				input.Worker.MaxMessages++
			case "account":
				input.Worker.Interaction.Destinations[0].Account = "other"
			case "intake":
				input.Telegram = nil
			}
			raw, _ := input.Bytes()
			if err := os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			stops := service.stops
			if _, err := i.enrollVerificationConsumers(context.Background(), spec, protocol.Hash(before)); err == nil || service.stops != stops {
				t.Fatal("mixed policy changed service", err)
			}
		})
	}
}

func TestManagedBootstrapCommandEnrollmentRequiresSeparateAdmission(t *testing.T) {
	ctx := context.Background()
	spec, i, service, before, input := managedCommandEnrollmentFixture(t)
	raw, _ := input.Bytes()
	if err := os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	enrolled, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before))
	if err != nil {
		t.Fatal(err)
	}
	i.services = &fakeDeliveryServices{service}
	i.deliveryProbe = i.bootstrapProbe
	i.deliveryInspect = func(ctx context.Context, m ManagedManifest, _ bool) error {
		_, err := inspectManagedService(ctx, i.services, m, false)
		return err
	}
	qualification := freshDeliveryQualification(t, spec.Root)
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(enrolled), protocol.Hash(qualification), false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	destination := c.Worker.Interaction.Destinations[0]
	inbound := interaction.Inbound{Provider: destination.Provider, Account: destination.Account, NativeID: "first", Actor: "789", Conversation: destination.ID, DestinationFingerprint: destination.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup/manager status"}
	s, err := openManagedStore(active, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitCommand(ctx, inbound); err == nil {
		t.Fatal("delivery alone admitted command")
	}
	s.Close()
	request, ready, err := ReviewCommandAdmission(ctx, spec.Root, protocol.Hash(active), "read")
	if err != nil || !ready {
		t.Fatal(ready, err)
	}
	admitted, err := i.ensureCommandPolicy(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Identity != before.Identity || admitted.DeliveryActivation.Hash != active.DeliveryActivation.Hash {
		t.Fatal("command admission changed identity or delivery")
	}
	s, err = openManagedStore(admitted, c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindManagedCommandAuthority(func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	view, err := s.AdmitCommand(ctx, inbound)
	if err != nil {
		t.Fatal(err)
	}
	if view.Command.Producer != c.Worker.Producers[0].Producer || view.Command.Descriptor.Effect != "read" {
		t.Fatal("command assigned wrong authority")
	}
}
