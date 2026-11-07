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

func enrolledLimitsOutputFixture(t *testing.T, disabled bool) (string, Config, LEDOutputRequest, *fakeLED) {
	t.Helper()
	path, before, output, api := bootstrapLimitsOutputFixture(t, disabled)
	after := bootstrapConsumerConfig(before)
	op, err := safefs.NewID("n")
	must(t, err)
	_, err = EnrollBootstrap(context.Background(), path, output.Identity, after, op)
	must(t, err)
	mapped := after
	mapped.LEDBindings = []LEDBinding{{ID: "backup", Producer: after.Producers[0].Producer, Subjects: []string{"*"}, LED: "green", Styles: standardLEDStyles()}}
	op, err = safefs.NewID("n")
	must(t, err)
	_, err = MigrateManagedLEDBindings(context.Background(), path, after, mapped, migrationSocket, api, output.Identity, op)
	must(t, err)
	return path, mapped, output, api
}

func TestEnrolledLimitsLEDHandoffRetainsOutputAndHistory(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		for _, boundary := range []string{"intent", "committed", "complete"} {
			t.Run(boundary+map[bool]string{true: "/disabled", false: "/enabled"}[disabled], func(t *testing.T) {
				ctx := context.Background()
				path, c, output, api := enrolledLimitsOutputFixture(t, disabled)
				original, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				states, clears := protocol.Hash(api.states), len(api.cleared)
				for round := 0; round < 3; round++ {
					after := c
					after.MaxMessages++
					op, err := safefs.NewID("n")
					must(t, err)
					must(t, PreflightEnrolledLimits(ctx, path, output.Identity, c, after))
					stop := errors.New("lost handoff response")
					r, err := migrateEnrolledLimits(ctx, path, output.Identity, c, after, op, func(at string) error {
						if at == boundary {
							return stop
						}
						return nil
					})
					if !errors.Is(err, stop) || r.Version != 6 || r.LEDOutputHash == "" {
						t.Fatal("handoff boundary", r, err)
					}
					for range 2 {
						again, e := MigrateEnrolledLimits(ctx, path, output.Identity, c, after, op)
						must(t, e)
						if protocol.Hash(again) != protocol.Hash(r) {
							t.Fatal("handoff receipt changed")
						}
					}
					c = after
					s, err := OpenManagedVerification(path, c, output.Identity)
					must(t, err)
					must(t, s.RequireLEDOutput(ctx, output.Operation, disabled))
					// The original critical ledger remains unresolved after migration.
					var faults int
					must(t, s.db.QueryRow("SELECT count(*) FROM led_states WHERE condition='critical' AND obsolete=0").Scan(&faults))
					if faults == 0 {
						t.Fatal("limits erased unresolved LED fault")
					}
					must(t, s.Close())
				}
				got, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				if string(got) != string(original) || protocol.Hash(api.states) != states || len(api.cleared) != clears {
					t.Fatal("limits rewrote output or contacted daemon")
				}
				// A later explicit output transition still consumes the old record;
				// another limits change pins the new head rather than erasing history.
				output.Operation, err = safefs.NewID("n")
				must(t, err)
				output.Disabled = !disabled
				_, err = ConfigureLEDOutput(ctx, path, c, output, api)
				must(t, err)
				after := c
				after.MaxMessages++
				op, err := safefs.NewID("n")
				must(t, err)
				_, err = MigrateEnrolledLimits(ctx, path, output.Identity, c, after, op)
				must(t, err)
				s, err := OpenManagedVerification(path, after, output.Identity)
				must(t, err)
				must(t, s.RequireLEDOutput(ctx, output.Operation, output.Disabled))
				must(t, s.Close())
			})
		}
	}
}

func TestEnrolledLimitsLEDHandoffRejectsLostAuthority(t *testing.T) {
	for _, damage := range []string{"pending", "output-file", "output-head", "output-record", "all-output-authority", "historical-output", "limits-pin", "limits-history"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, before, output, api := enrolledLimitsOutputFixture(t, true)
			if damage == "pending" {
				output.Operation, _ = safefs.NewID("n")
				output.Disabled = false
				_, err := configureLEDOutput(ctx, path, before, output, api, func(at string) error {
					if at == "output-intent" {
						return errors.New("interrupted")
					}
					return nil
				})
				if err == nil {
					t.Fatal("missing interruption")
				}
			} else {
				for range 2 {
					after := before
					after.MaxMessages++
					op, _ := safefs.NewID("n")
					_, err := MigrateEnrolledLimits(ctx, path, output.Identity, before, after, op)
					must(t, err)
					before = after
				}
			}
			d, err := safefs.Open(path)
			must(t, err)
			defer d.Close()
			switch damage {
			case "output-file":
				must(t, os.Remove(filepath.Join(path, ledOutputFile)))
			case "output-head", "output-record", "all-output-authority", "historical-output":
				s, err := OpenManagedVerification(path, before, output.Identity)
				must(t, err)
				if damage == "historical-output" {
					must(t, s.Close())
					priorOperation := output.Operation
					output.Operation, _ = safefs.NewID("n")
					output.Disabled = false
					_, err = ConfigureLEDOutput(ctx, path, before, output, api)
					must(t, err)
					s, err = OpenManagedVerification(path, before, output.Identity)
					must(t, err)
					_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", ledOutputPrefix+priorOperation)
					must(t, err)
					must(t, s.Close())
					break
				}
				if damage == "all-output-authority" {
					_, err = s.db.Exec("DELETE FROM metadata WHERE key=? OR key LIKE ?", ledOutputHead, ledOutputPrefix+"%")
					must(t, err)
					must(t, os.Remove(filepath.Join(path, ledOutputFile)))
					must(t, s.Close())
					break
				}
				key := ledOutputHead
				if damage == "output-record" {
					key = ledOutputPrefix + output.Operation
				}
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", key)
				must(t, err)
				must(t, s.Close())
			case "limits-pin", "limits-history":
				var v enrolledLimits
				must(t, d.ReadJSON(enrolledLimitsFile, &v))
				if damage == "limits-pin" {
					v.Receipt.LEDOutputHash = protocol.Hash("wrong")
					must(t, d.WriteJSON(enrolledLimitsFile, v, true))
				} else {
					must(t, os.Remove(filepath.Join(path, "enrolled-limits-history", v.Receipt.Previous+".json")))
				}
			}
			after := before
			after.MaxMessages++
			if PreflightEnrolledLimits(ctx, path, output.Identity, before, after) == nil {
				t.Fatal("damaged history passed preflight")
			}
			op, _ := safefs.NewID("n")
			if _, err = MigrateEnrolledLimits(ctx, path, output.Identity, before, after, op); err == nil {
				t.Fatal("damaged history migrated")
			}
		})
	}
}

func TestEnrolledLEDPolicyKindsRetainMappedHistory(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "disabled"}[disabled], func(t *testing.T) {
			ctx := context.Background()
			path, c, output, api := enrolledLimitsOutputFixture(t, disabled)
			original, err := os.ReadFile(filepath.Join(path, ledOutputFile))
			must(t, err)
			states, clears := protocol.Hash(api.states), len(api.cleared)
			for _, version := range []int{1, 2, 3, 4, 5, 1} {
				after := c
				after.Producers = append([]Enrollment(nil), c.Producers...)
				switch version {
				case 1:
					after.MaxMessages++
				case 2:
					after.Producers[0].MaxSilenceSeconds++
				case 3:
					after.Routes = map[string]Route{}
					for k, v := range c.Routes {
						after.Routes[k] = v
					}
					after.Routes["audit"] = Route{Revision: "audit-1", Primary: "telegram"}
				case 4:
					after.Producers[0].Required = false
				case 5: // Same-destination token replacement still records an exact handoff.
				}
				op, err := safefs.NewID("n")
				must(t, err)
				stop := errors.New("lost policy reply")
				r, err := migrateEnrolledPolicy(ctx, path, output.Identity, c, after, op, version, func(at string) error {
					if at == "committed" {
						return stop
					}
					return nil
				})
				if !errors.Is(err, stop) || r.Version != version+5 {
					t.Fatal(version, r, err)
				}
				_, err = migrateEnrolledPolicy(ctx, path, output.Identity, c, after, op, version, nil)
				must(t, err)
				s, err := OpenManagedVerification(path, after, output.Identity)
				must(t, err)
				must(t, s.RequireLEDOutput(ctx, output.Operation, disabled))
				if protocol.Hash(s.config.LEDBindings) != protocol.Hash(c.LEDBindings) {
					t.Fatal("policy changed bindings")
				}
				must(t, s.Close())
				c = after
			}
			got, err := os.ReadFile(filepath.Join(path, ledOutputFile))
			must(t, err)
			if string(got) != string(original) || protocol.Hash(api.states) != states || len(api.cleared) != clears {
				t.Fatal("policy changed LED output or contacted daemon")
			}
		})
	}
}
