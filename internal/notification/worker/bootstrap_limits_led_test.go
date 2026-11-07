package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func bootstrapLimitsOutputFixture(t *testing.T, disabled bool) (string, Config, LEDOutputRequest, *fakeLED) {
	t.Helper()
	ctx := context.Background()
	path, op := bootstrapTarget(t)
	c := bootstrapConfig()
	b, err := BootstrapGateway(ctx, path, op, c)
	must(t, err)
	api := &fakeLED{states: map[string]led.State{"foreign": {ID: "foreign", LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}}}
	s, err := OpenBootstrapVerification(path, c, b.Identity, "")
	must(t, err)
	observe, err := s.MonitorRuntimeLoop("fixture-fault")
	must(t, err)
	observe(errors.New("retained fault"))
	must(t, (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(ctx))
	// Seed a retained owned fault through the domain ledger. Verification does
	// not manufacture production faults from transient runtime observations.
	var fault string
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		var e error
		fault, e = s.currentLEDState(tx, (&LEDReconciler{}).bindingID(), "critical")
		return e
	}))
	api.states[fault] = led.State{ID: fault, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true, Message: "Notification state: critical"}
	must(t, s.Close())
	op, err = safefs.NewID("n")
	must(t, err)
	r := LEDOutputRequest{Identity: b.Identity, Operation: op, Socket: migrationSocket, Disabled: true}
	_, err = ConfigureLEDOutput(ctx, path, c, r, api)
	must(t, err)
	if !disabled {
		r.Operation, err = safefs.NewID("n")
		must(t, err)
		r.Disabled = false
		_, err = ConfigureLEDOutput(ctx, path, c, r, api)
		must(t, err)
	}
	return path, c, r, api
}

func TestBootstrapLimitsLEDHandoffRetainsOutputAndHistory(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		for _, boundary := range []string{"intent", "committed", "complete"} {
			t.Run(boundary+map[bool]string{true: "/disabled", false: "/enabled"}[disabled], func(t *testing.T) {
				ctx := context.Background()
				path, c, output, api := bootstrapLimitsOutputFixture(t, disabled)
				original, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				states, clears := protocol.Hash(api.states), len(api.cleared)
				for round := 0; round < 3; round++ {
					after := c
					after.MaxMessages++
					op, err := safefs.NewID("n")
					must(t, err)
					must(t, PreflightBootstrapLimits(ctx, path, output.Identity, c, after))
					stop := errors.New("lost handoff response")
					r, err := migrateBootstrapLimits(ctx, path, output.Identity, c, after, op, func(at string) error {
						if at == boundary {
							return stop
						}
						return nil
					})
					if !errors.Is(err, stop) || r.Version != 2 || r.LEDOutputHash == "" {
						t.Fatal("handoff boundary", r, err)
					}
					for range 2 {
						again, e := MigrateBootstrapLimits(ctx, path, output.Identity, c, after, op)
						must(t, e)
						if protocol.Hash(again) != protocol.Hash(r) {
							t.Fatal("handoff receipt changed")
						}
					}
					c = after
					s, err := OpenBootstrapVerification(path, c, output.Identity, "")
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
				_, err = MigrateBootstrapLimits(ctx, path, output.Identity, c, after, op)
				must(t, err)
				s, err := OpenBootstrapVerification(path, after, output.Identity, "")
				must(t, err)
				must(t, s.RequireLEDOutput(ctx, output.Operation, output.Disabled))
				must(t, s.Close())
			})
		}
	}
}

func TestBootstrapLimitsLEDHandoffRejectsLostAuthority(t *testing.T) {
	for _, damage := range []string{"pending", "output-file", "output-head", "output-record", "all-output-authority", "historical-output", "limits-pin", "limits-history"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, before, output, api := bootstrapLimitsOutputFixture(t, true)
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
					_, err := MigrateBootstrapLimits(ctx, path, output.Identity, before, after, op)
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
				s, err := OpenBootstrapVerification(path, before, output.Identity, "")
				must(t, err)
				if damage == "historical-output" {
					must(t, s.Close())
					priorOperation := output.Operation
					output.Operation, _ = safefs.NewID("n")
					output.Disabled = false
					_, err = ConfigureLEDOutput(ctx, path, before, output, api)
					must(t, err)
					s, err = OpenBootstrapVerification(path, before, output.Identity, "")
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
				var v bootstrapLimits
				must(t, d.ReadJSON(bootstrapLimitsFile, &v))
				if damage == "limits-pin" {
					v.Receipt.LEDOutputHash = protocol.Hash("wrong")
					must(t, d.WriteJSON(bootstrapLimitsFile, v, true))
				} else {
					must(t, os.Remove(filepath.Join(path, "bootstrap-limits-history", v.Receipt.Previous+".json")))
				}
			}
			after := before
			after.MaxMessages++
			if PreflightBootstrapLimits(ctx, path, output.Identity, before, after) == nil {
				t.Fatal("damaged history passed preflight")
			}
			op, _ := safefs.NewID("n")
			if _, err = MigrateBootstrapLimits(ctx, path, output.Identity, before, after, op); err == nil {
				t.Fatal("damaged history migrated")
			}
		})
	}
}
