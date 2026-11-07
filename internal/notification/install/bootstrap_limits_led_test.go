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

func TestManagedBootstrapLimitsAfterLEDOutput(t *testing.T) {
	testManagedBootstrapPolicyAfterLEDOutput(t, "bootstrap-limits")
}
func TestManagedBootstrapRoutesAfterLEDOutput(t *testing.T) {
	testManagedBootstrapPolicyAfterLEDOutput(t, "bootstrap-routes")
}
func testManagedBootstrapPolicyAfterLEDOutput(t *testing.T, operation string) {
	for _, boundary := range []string{"prepared", "stop-returned", operation + "-migrated", "published", "start-returned", "complete"} {
		for _, disabled := range []bool{true, false} {
			t.Run(boundary+map[bool]string{true: "/disabled", false: "/enabled"}[disabled], func(t *testing.T) {
				ctx := context.Background()
				spec, i, services, before := unboundUpdateFixture(t, true)
				probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
				i.ledOutputProbe, i.bootstrapLimitsProbe = probe, probe
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
				input := changeBootstrapLimitsInput(t, spec, operation == "bootstrap-routes")
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
			})
		}
	}
}
