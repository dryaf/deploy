package install

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func managedCommandsFixture(t *testing.T) (ManagedSpec, managedInstaller, Runtime, ManagedManifest, QualificationRecord) {
	return managedCommandsFixtureUsing(t, nil)
}

func managedCommandsFixtureUsing(t *testing.T, configure func(*Runtime)) (ManagedSpec, managedInstaller, Runtime, ManagedManifest, QualificationRecord) {
	t.Helper()
	spec, i, f, _ := managedFixture(t)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Worker.Producers[0].Producer
	d := interaction.Destination{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}
	c.Telegram = &provider.TelegramAccount{Account: "123", TokenFile: c.Providers["telegram"].TokenFile, BotName: "ExampleBot"}
	c.Worker.Interaction = &interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "worker/local", Producer: p, Generation: "one", Commands: map[string]interaction.Descriptor{
		"status": {ID: "worker.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "status"},
		"retry":  {ID: "worker.retry", Version: 1, Permission: "job.request", Effect: "mutation", Scoped: true, TargetRequired: true, Arguments: map[string]interaction.Argument{"subject": {Kind: "token", Required: true, Max: 256}, "target": {Kind: "token", Required: true, Max: 256}}, Help: "retry"},
	}}}, Destinations: []interaction.Destination{d}, Permissions: []interaction.Permission{{Actor: "789", Conversation: d.ID, Worker: p.ID, Permissions: []string{"read", "job.request"}, Scope: interaction.Scope{WholeWorker: true}}}}
	if configure != nil {
		configure(&c)
	}
	writeRuntime(t, spec.RuntimeFile, c)
	c, err = ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	i.services = &fakeDeliveryServices{f}
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	// The fixture uses /bin/true; capability execution is covered separately.
	i.commandProbe, i.deliveryProbe, i.commandPauseProbe = nop, nop, nop
	i.deliveryInspect = func(ctx context.Context, m ManagedManifest, _ bool) error {
		_, err := inspectManagedService(ctx, i.services, m, false)
		return err
	}
	b, err := i.prepare(context.Background(), "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	record := freshDeliveryQualification(t, spec.Root)
	active, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(b), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	return spec, i, c, active, record
}

func TestManagedCommandsLifecyclePreservesOutboundDelivery(t *testing.T) {
	spec, i, c, active, record := managedCommandsFixture(t)
	ctx := context.Background()
	s, err := openManagedStore(active, c)
	if err != nil {
		t.Fatal(err)
	}
	d := c.Worker.Interaction.Destinations[0]
	in := interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "first", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"}
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("outbound delivery enabled command intake")
	}
	s.Close()
	enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err = openManagedStore(enabled, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindManagedCommandAuthority(func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	v, err := s.AdmitCommand(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ImportCommand(ctx, v.Command.Producer, v.Command.ID, protocol.Hash(v.Command)); err != nil {
		t.Fatal(err)
	}
	grant, err := s.GrantCommand(ctx, v.Command.Producer, v.Command.ID)
	if err != nil || grant.ID == "" {
		t.Fatal(grant, err)
	}
	in.NativeID = "mutation"
	in.Text = "worker/local retry subject=a target=incident-one"
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("read admission accepted mutation")
	}
	s.Close()
	r := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(enabled), Producer: c.Worker.Producers[0].Producer, Route: c.Worker.Producers[0].Routes[0], Generation: "one"}
	if err = i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error { return s.Activate() }); err != nil {
		t.Fatal(err)
	}
	drained, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if time.Now().Before(grant.ExpiresAt) {
		t.Fatal("disable returned before grant admission window expired")
	}
	if drained.Mode != "delivery" || drained.DeliveryActivation.Hash != active.DeliveryActivation.Hash || drained.Commands.Mode != "drain" {
		t.Fatal("command disable changed outbound activation")
	}
	s, err = openManagedStore(drained, c)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.OwnershipStatus(r.Producer, r.Generation)
	if err != nil || !status.Active {
		t.Fatal("command disable revoked reporting", status, err)
	}
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("drain accepted intake")
	}
	s.Close()
	all, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(drained), protocol.Hash(record), "all", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err = openManagedStore(all, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindManagedCommandAuthority(func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	v, err = s.AdmitCommand(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ImportCommand(ctx, v.Command.Producer, v.Command.ID, protocol.Hash(v.Command)); err != nil {
		t.Fatal(err)
	}
	grant, err = s.GrantCommand(ctx, v.Command.Producer, v.Command.ID)
	if err != nil || grant.ID == "" {
		t.Fatal("qualified mutation did not receive grant", grant, err)
	}
	s.Close()
	r.ManifestHash = protocol.Hash(all)
	r.Revoke = true
	if err = i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error { return s.Revoke(ctx) }); err != nil {
		t.Fatal(err)
	}
	deactivated, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(all), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if deactivated.Commands != nil {
		t.Fatal("deactivation retained command admission")
	}
}

func TestManagedCommandsInterruptedAndQualificationPins(t *testing.T) {
	for _, point := range []string{"prepared", "manifest-published"} {
		t.Run(point, func(t *testing.T) {
			spec, i, c, active, record := managedCommandsFixture(t)
			if _, err := i.prepareCommands(context.Background(), "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash("wrong"), "all", false); err == nil {
				t.Fatal("wrong qualification accepted")
			}
			stop := errors.New("interrupted")
			i.fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if _, err := i.prepareCommands(context.Background(), "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			s, err := openManagedStore(m, c)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h, err := s.Health(context.Background())
			if err != nil || h.CommandMode != "read" || h.CommandAdmissionHash != m.Commands.Hash {
				t.Fatal(h, err)
			}
		})
	}
}

func TestManagedCommandsInterruptedActivationCanDeactivateWithoutCredentials(t *testing.T) {
	for _, point := range []string{"prepared", "command-admission-committed", "manifest-published"} {
		t.Run(point, func(t *testing.T) {
			spec, i, c, active, record := managedCommandsFixture(t)
			ctx := context.Background()
			r := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(active), Producer: c.Worker.Producers[0].Producer, Generation: "one", Route: c.Worker.Producers[0].Routes[0]}
			if err := i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error { return s.Activate() }); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("interrupted")
			i.fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if _, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "all", false); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			j, err := readManagedJournal(d)
			d.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(c.Telegram.TokenFile); err != nil {
				t.Fatal(err)
			}
			r.ManifestHash = protocol.Hash(j.Intent.After)
			r.Revoke = true
			if err = i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error { return s.Revoke(ctx) }); err != nil {
				t.Fatal("pending-operation revocation failed", err)
			}
			m, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(j.Intent.After), "", false)
			if err != nil {
				t.Fatal("cannot cancel command activation", err)
			}
			if m.Mode != "verification" || m.Commands != nil || m.CommandHistoryHash != j.Intent.After.CommandHistoryHash {
				t.Fatal("cancellation lost history or retained commands", m)
			}
			s, err := openManagedStore(m, c)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			status, err := s.OwnershipStatus(r.Producer, r.Generation)
			if err != nil || status.State != "revoked" {
				t.Fatal(status, err)
			}
		})
	}
}
