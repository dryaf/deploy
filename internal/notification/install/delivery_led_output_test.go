package install

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func productionOutputFixture(t *testing.T, commands, disabled bool) (ManagedSpec, managedInstaller, Runtime, ManagedManifest, QualificationRecord, *managedTestLED) {
	t.Helper()
	var configure func(*Runtime)
	if commands {
		configure = func(c *Runtime) {
			p := c.Worker.Producers[0].Producer
			d := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}
			c.Telegram = &provider.TelegramAccount{Account: "123", BotName: "ExampleBot", TokenFile: c.Providers["telegram"].TokenFile}
			c.Worker.Interaction = &interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup", Producer: p, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "worker.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "status"}}}}, Destinations: []interaction.Destination{d}, Permissions: []interaction.Permission{{Actor: "789", Conversation: "ops", Worker: p.ID, Permissions: []string{"read"}, Scope: interaction.Scope{WholeWorker: true}}}}
		}
	}
	spec, i, _, c, before, q := managedLEDDeliveryFixtureUsing(t, configure, disabled)
	i.commandProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.commandPolicyProbe = i.commandProbe
	api := &managedTestLED{states: map[string]led.State{"foreign": {ID: "foreign", LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}}}
	i.ledAPI = func(string) worker.LEDAPI { return api }
	m, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(q), false)
	if err != nil {
		t.Fatal(err)
	}
	request := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Producer: c.Worker.Producers[0].Producer, Route: c.Worker.Producers[0].Routes[0], Generation: "one"}
	if err = i.withManagedProducer(context.Background(), request, func(s ManagedProducerSession) error {
		if e := s.Stage(); e != nil {
			return e
		}
		return s.Activate()
	}); err != nil {
		t.Fatal(err)
	}
	return spec, i, c, m, q, api
}

func TestDeliveryLEDOutputPreservesAdmissionAcrossCommandChanges(t *testing.T) {
	for _, commands := range []bool{false, true} {
		t.Run(fmt.Sprint(commands), func(t *testing.T) {
			spec, i, c, m, q, api := productionOutputFixture(t, commands, false)
			ctx := context.Background()
			original := m
			var err error
			if commands {
				m, err = i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "enable-commands", Mode: "read", QualificationHash: protocol.Hash(q)})
				if err != nil {
					t.Fatal(err)
				}
			}
			s, err := openManagedStore(m, c)
			if err != nil {
				t.Fatal(err)
			}
			reconcile := worker.LEDReconciler{Store: s, API: deliveryTestLED{api}, Budget: 2 * time.Second}
			if err = reconcile.Step(ctx); err != nil {
				s.Close()
				t.Fatal(err)
			}
			s.Close()
			if api.mutations == 0 {
				t.Fatal("fixture has no owned output")
			}
			foreign := api.states["foreign"]
			for _, disabled := range []bool{true, false, true} {
				prior := protocol.Hash(m)
				tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
				reviewed, err := ReviewDeliveryLEDOutput(ctx, spec.Root, prior, disabled)
				if err != nil || protocol.Hash(reviewed) != prior {
					t.Fatal("review", err)
				}
				if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
					t.Fatal("review mutated state")
				}
				m, err = i.ensureLEDOutput(ctx, spec.Root, prior, disabled)
				if err != nil {
					t.Fatal("apply", err)
				}
				calls := api.mutations
				again, err := i.ensureLEDOutput(ctx, spec.Root, prior, disabled)
				if err != nil || protocol.Hash(again) != protocol.Hash(m) || api.mutations != calls {
					t.Fatal("retry repeated output transition", err)
				}
				if m.Identity != original.Identity || m.DeliveryOwnerHash != original.DeliveryOwnerHash || *m.DeliveryActivation != *original.DeliveryActivation || m.RuntimeHash != original.RuntimeHash || m.UnitHash != original.UnitHash || api.states["foreign"] != foreign {
					t.Fatal("output change replaced ownership, configuration or foreign output")
				}
				store, e := openManagedStore(m, c)
				if e != nil {
					t.Fatal(e)
				}
				owner, e := store.OwnershipStatus(c.Worker.Producers[0].Producer, "one")
				store.Close()
				if e != nil || !owner.Active {
					t.Fatal("output revoked reporting", owner, e)
				}
				if _, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(m), protocol.Hash(original)); err != nil {
					t.Fatal("reporting continuation", err)
				}
				if commands {
					m, err = i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "disable-commands"})
					if err != nil {
						t.Fatal(err)
					}
					m, err = i.ensureCommandPolicy(ctx, CommandPolicyRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Operation: "enable-commands", Mode: "read", QualificationHash: protocol.Hash(q)})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(m), protocol.Hash(original)); err != nil {
						t.Fatal(err)
					}
				}
			}
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = reviewDeliveryLEDOutput(d, m, c, true, time.Now().Add(31*24*time.Hour)); err == nil {
				t.Fatal("expired evidence would strand the service after stopping")
			}
			if err = reviewDeliveryLEDOutput(d, m, c, false, time.Now().Add(31*24*time.Hour)); err == nil {
				t.Fatal("expired evidence enabled output")
			}
		})
	}
}

func TestDeliveryLEDOutputRefusesUnqualifiedEnableAndOldHelper(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			spec, i, _, m, _, api := productionOutputFixture(t, false, disabled)
			tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
			if disabled {
				if _, err := ReviewDeliveryLEDOutput(context.Background(), spec.Root, protocol.Hash(m), false); err == nil {
					t.Fatal("enabled output not covered by qualification")
				}
			} else {
				i.ledOutputProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old helper") }
			}
			if _, err := i.ensureLEDOutput(context.Background(), spec.Root, protocol.Hash(m), !disabled); err == nil {
				t.Fatal("unsupported transition succeeded")
			}
			if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) || api.mutations != 0 {
				t.Fatal("refusal retained effects")
			}
		})
	}
}

func TestDeliveryLEDOutputResumesWithoutRepeatingClear(t *testing.T) {
	for _, point := range []string{"prepared", "stop-returned", "led-output-complete", "manifest-published", "complete"} {
		t.Run(point, func(t *testing.T) {
			spec, i, c, m, _, api := productionOutputFixture(t, false, false)
			ctx := context.Background()
			s, err := openManagedStore(m, c)
			if err != nil {
				t.Fatal(err)
			}
			r := worker.LEDReconciler{Store: s, API: deliveryTestLED{api}, Budget: 2 * time.Second}
			if err = r.Step(ctx); err != nil {
				s.Close()
				t.Fatal(err)
			}
			s.Close()
			original := m
			lost := errors.New("lost output acknowledgement")
			i.fault = func(at string) error {
				if at == point {
					return lost
				}
				return nil
			}
			if _, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(original), true); !errors.Is(err, lost) {
				t.Fatal("missed boundary", err)
			}
			i.fault = nil
			m, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(original), true)
			if err != nil {
				t.Fatal(err)
			}
			calls := api.mutations
			again, err := i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(original), true)
			if err != nil || protocol.Hash(again) != protocol.Hash(m) || api.mutations != calls || !m.LEDOutput.Disabled || len(api.states) != 1 || api.states["foreign"].ID != "foreign" {
				t.Fatal("resume changed output or repeated clear", err)
			}
			if _, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(m), protocol.Hash(original)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
