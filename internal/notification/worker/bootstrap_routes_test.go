package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestBootstrapRoutesMixedPolicyAndOutputHistory(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		for _, boundary := range []string{"intent", "committed", "complete"} {
			t.Run(boundary+map[bool]string{false: "/enabled", true: "/disabled"}[disabled], func(t *testing.T) {
				ctx := context.Background()
				path, before, output, api := bootstrapLimitsOutputFixture(t, disabled)
				original, err := os.ReadFile(filepath.Join(path, "bootstrap.json"))
				must(t, err)
				states, clears := protocol.Hash(api.states), len(api.cleared)
				for round := 0; round < 3; round++ {
					after := before
					route := Route{Revision: "changed", Primary: "profile-next"}
					if round == 2 {
						route = Route{Revision: "again", Primary: "profile-third"}
					}
					after.Routes = map[string]Route{"alerts": route}
					kind := "routes"
					if round == 1 {
						after.Routes = before.Routes
						after.MaxMessages++
						kind = ""
					}
					op, _ := safefs.NewID("n")
					must(t, preflightBootstrapPolicy(ctx, path, output.Identity, before, after, kind))
					stop := errors.New("lost policy reply")
					r, err := migrateBootstrapPolicy(ctx, path, output.Identity, before, after, op, kind, func(at string) error {
						if at == boundary {
							return stop
						}
						return nil
					})
					if !errors.Is(err, stop) || r.Kind != kind || kind == "routes" && r.Version != 4 {
						t.Fatal(r, err)
					}
					if boundary != "complete" {
						if s, e := OpenBootstrapVerification(path, after, output.Identity, ""); e == nil {
							s.Close()
							t.Fatal("pending route admitted runtime")
						}
					}
					for range 2 {
						again, e := migrateBootstrapPolicy(ctx, path, output.Identity, before, after, op, kind, nil)
						must(t, e)
						if protocol.Hash(again) != protocol.Hash(r) {
							t.Fatal("policy receipt changed")
						}
					}
					before = after
					s, err := OpenBootstrapVerification(path, before, output.Identity, "")
					must(t, err)
					must(t, s.RequireLEDOutput(ctx, output.Operation, disabled))
					must(t, s.Close())
				}
				got, err := os.ReadFile(filepath.Join(path, "bootstrap.json"))
				must(t, err)
				if string(got) != string(original) || states != protocol.Hash(api.states) || clears != len(api.cleared) {
					t.Fatal("route migration rewrote reservation or mutated daemon")
				}
				enrolled := bootstrapConsumerConfig(before)
				enrolled.Producers[0].Routes = []string{"alerts"}
				op, _ := safefs.NewID("n")
				_, err = EnrollBootstrap(ctx, path, output.Identity, enrolled, op)
				must(t, err)
				s, err := OpenManagedVerification(path, enrolled, output.Identity)
				must(t, err)
				defer s.Close()
				must(t, s.RequireInactiveProducers())
				must(t, s.RequireLEDOutput(ctx, output.Operation, disabled))
			})
		}
	}
}

func TestBootstrapRoutesRefusesProductionAndMixedChanges(t *testing.T) {
	for _, damage := range []string{"limits", "enrolled", "missing-history", "relabeled"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, before, output, _ := bootstrapLimitsOutputFixture(t, false)
			after := before
			after.Routes = map[string]Route{"alerts": {Revision: "next", Primary: "new-provider"}}
			op, _ := safefs.NewID("n")
			switch damage {
			case "limits":
				after.MaxMessages++
			case "enrolled":
				_, err := EnrollBootstrap(ctx, path, output.Identity, bootstrapConsumerConfig(before), op)
				must(t, err)
			case "missing-history", "relabeled":
				_, err := MigrateBootstrapRoutes(ctx, path, output.Identity, before, after, op)
				must(t, err)
				d, err := safefs.Open(path)
				must(t, err)
				defer d.Close()
				if damage == "missing-history" {
					must(t, os.Remove(filepath.Join(path, bootstrapLimitsFile)))
				} else {
					var v bootstrapLimits
					must(t, d.ReadJSON(bootstrapLimitsFile, &v))
					v.Receipt.Kind = ""
					v.Receipt.Version = 2
					must(t, d.WriteJSON(bootstrapLimitsFile, v, true))
				}
			}
			if err := PreflightBootstrapRoutes(ctx, path, output.Identity, before, after); err == nil {
				t.Fatal("invalid route passed preflight")
			}
			if _, err := MigrateBootstrapRoutes(ctx, path, output.Identity, before, after, op); err == nil {
				t.Fatal("invalid route migrated")
			}
		})
	}
}
