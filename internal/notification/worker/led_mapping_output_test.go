package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func mappingOutputFixture(t *testing.T, disabled bool) (string, Config, LEDOutputRequest, *fakeLED) {
	t.Helper()
	ctx := context.Background()
	path, before, output, api := bootstrapLimitsOutputFixture(t, disabled)
	after := before
	after.MaxMessages++
	op, _ := safefs.NewID("n")
	_, err := MigrateBootstrapLimits(ctx, path, output.Identity, before, after, op)
	must(t, err)
	before = bootstrapConsumerConfig(after)
	op, _ = safefs.NewID("n")
	_, err = EnrollBootstrap(ctx, path, output.Identity, before, op)
	must(t, err)
	return path, before, output, api
}

func TestLEDMappingOutputHandoff(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		boundaries := []string{"mapping_intent", "mapping_prepared", "mapping_before_commit", "mapping_committed", "mapping_unfenced"}
		if !disabled {
			boundaries = append(boundaries, "mapping_rpc_intent", "mapping_rpc_returned", "mapping_rpc_ack")
		}
		for _, boundary := range boundaries {
			t.Run(map[bool]string{true: "disabled/", false: "enabled/"}[disabled]+boundary, func(t *testing.T) {
				ctx := context.Background()
				path, before, output, api := mappingOutputFixture(t, disabled)
				original, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				states, clears := protocol.Hash(api.states), len(api.cleared)
				for round := 0; round < 2; round++ {
					after := before
					after.LEDBindings = []LEDBinding{{ID: "backup", Producer: before.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
					if round == 1 {
						after.LEDBindings[0].Subjects = []string{"db"}
					}

					must(t, PreflightManagedLEDBindings(ctx, path, before, after, output.Identity))
					op, _ := safefs.NewID("n")
					stop := errors.New("interrupt mapping output")
					_, err = migrateLEDBindingsPinned(ctx, path, before, after, migrationSocket, api, func(at string) error {
						if at == boundary {
							return stop
						}
						return nil
					}, &managedLEDAuthority{output.Identity, op})
					if !errors.Is(err, stop) {
						t.Fatal("boundary", err)
					}
					for range 2 {
						_, err = MigrateManagedLEDBindings(ctx, path, before, after, migrationSocket, api, output.Identity, op)
						must(t, err)
					}
					s, err := OpenManagedVerification(path, after, output.Identity)
					must(t, err)
					must(t, s.RequireLEDOutput(ctx, output.Operation, disabled))
					must(t, s.RequireInactiveProducers())
					m := latestMapping(t, s)
					if m.Intent.Version != 2 || m.Intent.LEDOutputOperation != output.Operation || m.Intent.LEDOutputDisabled != disabled {
						t.Fatal("lost pin")
					}
					var faults int
					must(t, s.db.QueryRow("SELECT count(*) FROM led_states WHERE condition='critical' AND obsolete=0").Scan(&faults))
					if faults == 0 {
						t.Fatal("lost fault history")
					}
					must(t, s.Close())
					before = after
				}
				if disabled && (states != protocol.Hash(api.states) || clears != len(api.cleared)) {
					t.Fatal("disabled mapping mutated daemon")
				}
				raw, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				if string(raw) != string(original) {
					t.Fatal("rewrote output intent")
				}
				required, err := MappingOutputRequired(path)
				must(t, err)
				if !required {
					t.Fatal("helper downgrade allowed")
				}
				output.Operation, _ = safefs.NewID("n")
				output.Disabled = !disabled
				_, err = ConfigureLEDOutput(ctx, path, before, output, api)
				must(t, err)
				// A later output head does not erase the mapping's historical pin.
				required, err = MappingOutputRequired(path)
				must(t, err)
				if !required {
					t.Fatal("lost historical capability")
				}
			})
		}
	}
}

func TestLEDMappingOutputRefusesLostAuthority(t *testing.T) {
	for _, damage := range []string{"file", "sql-head", "sql-record", "mapping-pin", "mapping-history", "pending-output"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, before, output, api := mappingOutputFixture(t, false)
			after := before
			after.LEDBindings = []LEDBinding{{ID: "backup", Producer: before.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
			op, _ := safefs.NewID("n")
			_, err := MigrateManagedLEDBindings(ctx, path, before, after, migrationSocket, api, output.Identity, op)
			must(t, err)
			s, err := OpenManagedVerification(path, after, output.Identity)
			must(t, err)
			switch damage {
			case "file":
				must(t, os.Remove(filepath.Join(path, ledOutputFile)))
			case "sql-head":
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", ledOutputHead)
			case "sql-record":
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", ledOutputPrefix+output.Operation)
			case "mapping-pin":
				m := latestMapping(t, s)
				m.Intent.LEDOutputHash = protocol.Hash("wrong")
				err = s.transaction(ctx, func(tx *sql.Tx) error { return writeLEDMapping(ctx, tx, m) })
			case "mapping-history":
				_, err = s.db.Exec("DELETE FROM led_migration_heads; DELETE FROM led_migration_operations; DELETE FROM led_migrations")
			}
			must(t, err)
			must(t, s.Close())
			if damage == "pending-output" {
				output.Operation, _ = safefs.NewID("n")
				output.Disabled = true
				_, err = configureLEDOutput(ctx, path, after, output, api, func(at string) error {
					if at == "output-intent" {
						return errors.New("interrupt")
					}
					return nil
				})
				if err == nil {
					t.Fatal("missing interruption")
				}
			}
			next := after
			next.LEDBindings = nil
			states, clears := protocol.Hash(api.states), len(api.cleared)
			if err = PreflightManagedLEDBindings(ctx, path, after, next, output.Identity); err == nil {
				t.Fatal("damaged output passed preflight")
			}
			op, _ = safefs.NewID("n")
			if _, err = MigrateManagedLEDBindings(ctx, path, after, next, migrationSocket, api, output.Identity, op); err == nil {
				t.Fatal("damaged output repaired itself")
			}
			if states != protocol.Hash(api.states) || clears != len(api.cleared) {
				t.Fatal("refusal touched daemon")
			}
		})
	}
}
