package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func managedOutputFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeManagedServices, *managedTestLED, ManagedManifest) {
	t.Helper()
	spec, i, services, api, _, _ := managedLEDSetup(t)
	m, err := i.prepare(context.Background(), "migrate-led", spec)
	if err != nil {
		t.Fatal(err)
	}
	i.ledOutputProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	return spec, i, services, api, m
}

func TestManagedLEDOutputLifecycle(t *testing.T) {
	spec, i, services, api, before := managedOutputFixture(t)
	ctx := context.Background()
	foreign := led.State{ID: "another-application", LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}
	api.states[foreign.ID] = foreign
	disabled, err := i.prepareLEDOutput(ctx, spec.Root, protocol.Hash(before), true)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.LEDOutput == nil || !disabled.LEDOutput.Disabled || disabled.Revision != before.Revision || disabled.ConfigHash != before.ConfigHash || disabled.Identity != before.Identity {
		t.Fatal(disabled)
	}
	if len(api.states) != 1 || api.states[foreign.ID] != foreign {
		t.Fatal("changed foreign output or retained owned output", api.states)
	}
	starts, stops, calls := services.starts, services.stops, api.mutations
	again, err := i.prepareLEDOutput(ctx, spec.Root, protocol.Hash(disabled), true)
	if err != nil || protocol.Hash(again) != protocol.Hash(disabled) || services.starts != starts || services.stops != stops || api.mutations != calls {
		t.Fatal("unchanged apply repeated effects", err)
	}
	enabled, err := i.prepareLEDOutput(ctx, spec.Root, protocol.Hash(disabled), false)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.LEDOutput.Disabled || enabled.LEDOutput.Operation == disabled.LEDOutput.Operation || api.mutations != calls {
		t.Fatal("re-enable emitted output or reused intent")
	}
	c, err := ReadRuntime(enabled.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openManagedStore(enabled, c)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(ctx)
	s.Close()
	if err != nil || h.LEDOutputDisposition == nil || !h.LEDOutputDisposition.Complete || h.LEDOutputDisposition.Disabled {
		t.Fatal(h, err)
	}
	if _, err = i.prepare(ctx, "update", spec); err != nil {
		t.Fatal("unchanged runtime upgrade lost output history", err)
	}
}

func TestManagedLEDOutputInterruptions(t *testing.T) {
	for _, point := range []string{"prepared", "stop-returned", "led-output-complete", "stopped", "files", "unit-published", "manifest-published", "reloaded", "started", "complete"} {
		t.Run(point, func(t *testing.T) {
			spec, i, _, api, before := managedOutputFixture(t)
			i.fault = func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := i.prepareLEDOutput(context.Background(), spec.Root, protocol.Hash(before), true); err == nil {
				t.Fatal("missing interruption")
			}
			i.fault = nil
			if point != "complete" {
				if _, err := i.uninstall(context.Background(), spec.Root); err == nil {
					t.Fatal("unfinished cleanup discarded")
				}
			}
			after, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			if after.LEDOutput == nil || !after.LEDOutput.Disabled || len(api.states) != 0 {
				t.Fatal("cleanup did not complete")
			}
			calls := api.mutations
			if _, err = i.resume(context.Background(), spec.Root); err != nil || calls != api.mutations {
				t.Fatal("completed resume repeated cleanup", err)
			}
		})
	}
}

func TestManagedLEDOutputLostAuthorityRefusesRepair(t *testing.T) {
	for _, phase := range []string{"stopped", "complete"} {
		t.Run(phase, func(t *testing.T) {
			spec, i, _, api, before := managedOutputFixture(t)
			i.fault = func(at string) error {
				if at == phase {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := i.prepareLEDOutput(context.Background(), spec.Root, protocol.Hash(before), true); err == nil {
				t.Fatal("missing interruption")
			}
			i.fault = nil
			if err := os.Remove(filepath.Join(spec.State, "led-output.json")); err != nil {
				t.Fatal(err)
			}
			calls := api.mutations
			if _, err := i.resume(context.Background(), spec.Root); err == nil {
				t.Fatal("lost output authority recreated")
			}
			if calls != api.mutations {
				t.Fatal("lost authority caused daemon mutations")
			}
		})
	}
}

type unavailableOutputLED struct{ worker.LEDAPI }

func (a unavailableOutputLED) List(context.Context) ([]led.Entry, error) {
	return nil, errors.New("daemon unavailable")
}

func TestManagedLEDOutputUnavailableDaemonRemainsPending(t *testing.T) {
	spec, i, services, api, before := managedOutputFixture(t)
	i.ledAPI = func(string) worker.LEDAPI { return unavailableOutputLED{api} }
	starts := services.starts
	if _, err := i.prepareLEDOutput(context.Background(), spec.Root, protocol.Hash(before), true); err == nil {
		t.Fatal("unavailable daemon accepted")
	}
	if services.starts != starts || services.state.Active != "inactive" {
		t.Fatal("pending cleanup restarted gateway")
	}
	i.ledAPI = func(string) worker.LEDAPI { return api }
	if _, err := i.resume(context.Background(), spec.Root); err != nil {
		t.Fatal(err)
	}
}

func TestManagedLEDOutputPreflightPreservesService(t *testing.T) {
	for _, kind := range []string{"fingerprint", "capability", "foreign-unit"} {
		t.Run(kind, func(t *testing.T) {
			spec, i, services, api, before := managedOutputFixture(t)
			hash := protocol.Hash(before)
			switch kind {
			case "fingerprint":
				hash = protocol.Hash("wrong")
			case "capability":
				i.ledOutputProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old helper") }
			case "foreign-unit":
				if err := os.WriteFile(filepath.Join(spec.UnitDirectory, managedUnit), []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stops, calls := services.stops, api.mutations
			if _, err := i.prepareLEDOutput(context.Background(), spec.Root, hash, true); err == nil {
				t.Fatal("unsafe preflight accepted")
			}
			if services.stops != stops || api.mutations != calls {
				t.Fatal("failed preflight changed service or output")
			}
		})
	}
}
