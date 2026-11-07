package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestDeclaredLEDMappingAfterBootstrapEnrollment(t *testing.T) {
	for _, output := range []string{"none", "disabled", "enabled"} {
		for _, boundary := range []string{"prepared", "stop-returned", "led-migrated", "files", "published", "complete", "daemon"} {
			if output == "disabled" && boundary == "daemon" {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", output, boundary), func(t *testing.T) {
				ctx := context.Background()
				spec, i, services, _ := managedFixture(t)
				i.bootstrapProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
				i.ledProbe = i.bootstrapProbe
				i.ledOutputProbe, i.bootstrapEnrollmentLEDProbe = i.bootstrapProbe, i.bootstrapProbe
				api := &managedTestLED{states: map[string]led.State{}}
				i.ledAPI = func(string) worker.LEDAPI { return api }
				spec.State = filepath.Join(filepath.Dir(spec.State), "unbound")
				raw, err := os.ReadFile(spec.RuntimeFile)
				if err != nil {
					t.Fatal(err)
				}
				var input RuntimeInput
				if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
					t.Fatal(err)
				}
				producers := input.Worker.Producers
				input.Bootstrap, input.Worker.Producers, input.LEDSocket = true, nil, "/private/fixture-led.sock"
				c, err := input.Resolve()
				if err != nil {
					t.Fatal(err)
				}
				op, _ := safefs.NewID("n")
				reserved, err := worker.BootstrapGateway(ctx, spec.State, op, c.Worker)
				if err != nil {
					t.Fatal(err)
				}
				write := func() {
					t.Helper()
					raw, err := input.Bytes()
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				write()
				before, err := i.ensureVerification(ctx, spec)
				if err != nil {
					t.Fatal(err)
				}
				if output != "none" {
					before, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(before), true)
					if err != nil {
						t.Fatal(err)
					}
					if output == "enabled" {
						before, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(before), false)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				input.Bootstrap, input.Worker.Producers = false, producers
				write()
				enrolled, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before))
				if err != nil {
					t.Fatal(err)
				}
				input.Worker.LEDBindings = []worker.LEDBinding{{ID: "backup", Producer: producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
				write()
				stops := services.stops
				if _, err = i.migrateVerificationLEDBindings(ctx, spec, protocol.Hash(before)); err == nil || services.stops != stops {
					t.Fatal("stale predecessor accepted", err)
				}
				stop := errors.New("lost mapping response")
				i.fault = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				api.lostSet = boundary == "daemon"
				if _, err = i.migrateVerificationLEDBindings(ctx, spec, protocol.Hash(enrolled)); err == nil {
					t.Fatal("boundary missed", boundary)
				}
				i.fault = nil
				mapped, err := i.migrateVerificationLEDBindings(ctx, spec, protocol.Hash(enrolled))
				if err != nil {
					t.Fatal(err)
				}
				if mapped.Identity != reserved.Identity || mapped.Mode != "verification" || mapped.Installation != enrolled.Installation {
					t.Fatal("mapping changed identity")
				}
				starts, mutations := services.starts, api.mutations
				if again, err := i.migrateVerificationLEDBindings(ctx, spec, protocol.Hash(enrolled)); err != nil || protocol.Hash(again) != protocol.Hash(mapped) || services.starts != starts || api.mutations != mutations {
					t.Fatal("completed mapping replayed effects", err)
				}
				c, err = input.Resolve()
				if err != nil {
					t.Fatal(err)
				}
				s, err := worker.OpenManagedVerification(spec.State, c.Worker, mapped.Identity)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if err = requireManagedLEDOutput(ctx, s, mapped); err != nil {
					t.Fatal(err)
				}
				if err = s.RequireInactiveProducers(); err != nil {
					t.Fatal("mapping activated producers", err)
				}
				for _, state := range api.states {
					if state.Pattern == "heartbeat" {
						t.Fatal("mapping invented healthy evidence")
					}
				}
			})
		}
	}

}
