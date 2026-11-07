package install

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedBootstrapEnrollmentAfterLEDOutput(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		for _, boundary := range []string{"prepared", "stop-returned", "bootstrap-enrolled", "published", "start-returned", "complete"} {
			t.Run(boundary+map[bool]string{true: "/disabled", false: "/enabled"}[disabled], func(t *testing.T) {
				ctx := context.Background()
				spec, i, services, before := unboundUpdateFixture(t, true)
				probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
				i.ledOutputProbe, i.bootstrapEnrollmentLEDProbe = probe, probe
				api := &managedTestLED{states: map[string]led.State{}}
				i.ledAPI = func(string) worker.LEDAPI { return api }
				var err error
				before, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(before), true)
				if err != nil {
					t.Fatal(err)
				}
				if !disabled {
					before, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(before), false)
					if err != nil {
						t.Fatal(err)
					}
				}
				raw, err := os.ReadFile(spec.RuntimeFile)
				if err != nil {
					t.Fatal(err)
				}
				var input RuntimeInput
				if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
					t.Fatal(err)
				}
				input.Bootstrap = false
				input.Worker.Producers = []worker.Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 10}}
				raw, err = input.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
				// An older helper must fail before stopping the current service.
				i.bootstrapEnrollmentLEDProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old helper") }
				stops := services.stops
				if _, err = i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); err == nil || services.stops != stops {
					t.Fatal("incapable helper stopped service", err)
				}
				i.bootstrapEnrollmentLEDProbe = probe
				stop := errors.New("lost enrollment reply")
				i.fault = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				if _, err = i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); !errors.Is(err, stop) {
					t.Fatal(boundary, err)
				}
				i.fault = nil
				after, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before))
				if err != nil {
					t.Fatal(err)
				}
				starts, mutations := services.starts, api.mutations
				if again, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before)); err != nil || protocol.Hash(again) != protocol.Hash(after) || services.starts != starts || api.mutations != mutations {
					t.Fatal("completed enrollment replayed", err)
				}
				if after.Identity != before.Identity || protocol.Hash(after.LEDOutput) != protocol.Hash(before.LEDOutput) {
					t.Fatal("enrollment changed output or identity")
				}
				c, err := input.Resolve()
				if err != nil {
					t.Fatal(err)
				}
				s, err := openManagedStore(after, c)
				if err != nil {
					t.Fatal(err)
				}
				if err = requireManagedLEDOutput(ctx, s, after); err != nil {
					t.Fatal(err)
				}
				if err = s.RequireInactiveProducers(); err != nil {
					t.Fatal(err)
				}
				s.Close()
				if _, err = i.inspectInstallation(ctx, spec.Root); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
