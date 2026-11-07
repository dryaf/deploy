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

func TestManagedEnrolledPolicyAfterLEDOutput(t *testing.T) {
	for _, operation := range []string{"enrolled-limits", "enrolled-freshness", "enrolled-routes", "enrolled-producer"} {
		t.Run(operation, func(t *testing.T) { testManagedEnrolledPolicyAfterLEDOutput(t, operation) })
	}
}
func testManagedEnrolledPolicyAfterLEDOutput(t *testing.T, operation string) {
	for _, boundary := range []string{"prepared", "stop-returned", operation + "-migrated", "published", "start-returned", "complete"} {
		for _, disabled := range []bool{true, false} {
			t.Run(boundary+map[bool]string{true: "/disabled", false: "/enabled"}[disabled], func(t *testing.T) {
				ctx := context.Background()
				spec, i, services, before := unboundUpdateFixture(t, true)
				probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
				i.ledOutputProbe, i.bootstrapLimitsProbe = probe, probe
				i.bootstrapEnrollmentLEDProbe, i.enrolledLimitsProbe = probe, probe
				i.ledProbe = probe
				api := &managedTestLED{states: map[string]led.State{"foreign": {ID: "foreign", LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}}}
				i.ledAPI = func(string) worker.LEDAPI { return api }
				var err error
				before, err = i.prepareLEDOutput(ctx, spec.Root, protocol.Hash(before), true)
				if err != nil {
					t.Fatal(err)
				}
				if !disabled {
					before, err = i.prepareLEDOutput(ctx, spec.Root, protocol.Hash(before), false)
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
				before, err = i.enrollVerificationConsumers(ctx, spec, protocol.Hash(before))
				if err != nil {
					t.Fatal(err)
				}
				input.Worker.LEDBindings = []worker.LEDBinding{{ID: "backup", Producer: input.Worker.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
				raw, err = input.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
				before, err = i.migrateVerificationLEDBindings(ctx, spec, protocol.Hash(before))
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "enrolled-limits":
					input.Worker.MaxMessages++
				case "enrolled-freshness":
					input.Worker.Producers[0].MaxEvidenceSeconds++
				case "enrolled-routes":
					input.Worker.Routes["audit"] = worker.Route{Revision: "audit-1", Primary: input.Worker.Routes["ops"].Primary}
				case "enrolled-producer":
					input.Worker.Producers[0].Required = false
				}
				raw, err = input.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
				stops := services.stops
				i.enrolledLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old helper") }
				if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), operation); err == nil || stops != services.stops {
					t.Fatal("old helper stopped service", err)
				}
				i.enrolledLimitsProbe = probe
				ledFile := filepath.Join(spec.State, "led-output.json")
				original, err := os.ReadFile(ledFile)
				if err != nil {
					t.Fatal(err)
				}
				mutations := api.mutations
				stop := errors.New("lost limits LED handoff response")
				i.fault = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), operation); !errors.Is(err, stop) {
					t.Fatal(boundary, err)
				}
				i.fault = nil
				after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), operation)
				if err != nil {
					t.Fatal(err)
				}
				starts := services.starts
				if again, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), operation); err != nil || protocol.Hash(again) != protocol.Hash(after) || starts != services.starts {
					t.Fatal("retry restarted verification", err)
				}
				if protocol.Hash(after.LEDOutput) != protocol.Hash(before.LEDOutput) || after.Identity != before.Identity || api.mutations != mutations {
					t.Fatal("limits changed output authority")
				}
				got, err := os.ReadFile(ledFile)
				if err != nil || string(got) != string(original) {
					t.Fatal("limits rewrote LED disposition", err)
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
				s.Close()
				if _, err = i.inspectInstallation(ctx, spec.Root); err != nil {
					t.Fatal("post-handoff inspection", err)
				}
				if boundary == "complete" && operation == "enrolled-limits" {
					for _, subject := range []string{"source", "*"} {
						input.Worker.LEDBindings[0].Subjects = []string{subject}
						raw, err = input.Bytes()
						if err != nil {
							t.Fatal(err)
						}
						if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
							t.Fatal(err)
						}
						after, err = i.migrateVerificationLEDBindings(ctx, spec, protocol.Hash(after))
						if err != nil {
							t.Fatal("mapping after policy", err)
						}
						input.Worker.MaxMessages++
						raw, err = input.Bytes()
						if err != nil {
							t.Fatal(err)
						}
						if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
							t.Fatal(err)
						}
						after, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(after), operation)
						if err != nil {
							t.Fatal("policy after mapping", err)
						}
						if after.Identity != before.Identity || protocol.Hash(after.LEDOutput) != protocol.Hash(before.LEDOutput) {
							t.Fatal("sequence replaced authority")
						}
						if _, err = i.inspectInstallation(ctx, spec.Root); err != nil {
							t.Fatal("sequence inspection", err)
						}
					}
				}
			})
		}
	}
}
