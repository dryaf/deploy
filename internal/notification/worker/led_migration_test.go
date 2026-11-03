package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const migrationSocket = "/tmp/deploy-test-led.sock"

func mappingFixture(t *testing.T) (string, Config, Config, *fakeLED) {
	t.Helper()
	s, now, path := bindingFixture(t, binding("selected", "backup"))
	must(t, s.Replace(context.Background(), snapshot(*now, 1, 1)))
	api := &fakeLED{states: map[string]led.State{}}
	must(t, (&LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}).Step(context.Background()))
	before, after := s.config, s.config
	after.LEDBindings = []LEDBinding{binding("selected", "other"), binding("added", "backup")}
	after.LEDBindings[0].LED, after.LEDBindings[0].AllowPower = "red", true
	must(t, s.Close())
	return path, before, after, api
}

func latestMapping(t *testing.T, s *Store) ledMappingRecord {
	t.Helper()
	var id string
	must(t, s.db.QueryRow("SELECT id FROM led_migrations ORDER BY position DESC LIMIT 1").Scan(&id))
	m, err := readLEDMapping(context.Background(), s.db, id)
	must(t, err)
	return m
}

func TestLEDMappingMigrationArchivesHistoryAndChangesOnlyBindings(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	foreign := "other-application-fault"
	api.states[foreign] = led.State{ID: foreign, LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}
	result, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
	must(t, err)
	if result.Operation == "" || result.BeforeHash == result.AfterHash {
		t.Fatal(result)
	}
	if _, ok := api.states[foreign]; !ok {
		t.Fatal("foreign state removed")
	}
	if old, err := Open(path, before, false); err == nil {
		old.Close()
		t.Fatal("old config reopened after migration")
	}
	s, err := Open(path, after, false)
	must(t, err)
	m := latestMapping(t, s)
	if !m.Completed || m.Next != len(m.Intent.Steps) {
		t.Fatal(m)
	}
	archived, err := readLEDOperationHistory(ctx, s.db, s.identity, m.Intent.Before, m.Intent.ID)
	must(t, err)
	if len(archived) != 2 || archived["selected"].LastAcknowledgedRevision == 0 {
		t.Fatal("acknowledgement history lost", archived)
	}
	active, err := s.LEDOperations(ctx)
	must(t, err)
	if len(active) != 0 {
		t.Fatal("old acknowledgement carried into new mapping")
	}
	for _, st := range api.states {
		if st.Pattern == "heartbeat" {
			t.Fatal("migration published healthy output")
		}
	}
	must(t, verifyLEDMappingOutput(ctx, api, m.Intent))
	// The unchanged worker retains its complete domain/queue state; only a fresh
	// publication can establish health under the new mapping.
	var subjects int
	must(t, s.db.QueryRow("SELECT count(*) FROM subjects").Scan(&subjects))
	if subjects != 1 {
		t.Fatal("domain subjects rewritten")
	}
	must(t, s.Close())
	calls := len(api.cleared)
	again, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
	must(t, err)
	if again != result || len(api.cleared) != calls {
		t.Fatal("completed migration was replayed")
	}
}

func TestLEDMappingMigrationRefusesUnresolvedFaultsAndTombstones(t *testing.T) {
	for _, obsolete := range []int{0, 1} {
		t.Run(string(rune('0'+obsolete)), func(t *testing.T) {
			path, before, after, api := mappingFixture(t)
			s, err := Open(path, before, false)
			must(t, err)
			must(t, s.transaction(context.Background(), func(tx *sql.Tx) error {
				id, e := s.currentLEDState(tx, "selected", "critical")
				if e != nil {
					return e
				}
				_, e = tx.Exec("UPDATE led_states SET obsolete=? WHERE id=?", obsolete, id)
				return e
			}))
			must(t, s.Close())
			calls := len(api.cleared)
			if _, err = MigrateLEDBindings(context.Background(), path, before, after, migrationSocket, api); err == nil {
				t.Fatal("unresolved fault removed by configuration change")
			}
			if len(api.cleared) != calls {
				t.Fatal("failed precondition changed LEDs")
			}
			if _, err = os.Stat(filepath.Join(path, "led-migration.json")); !os.IsNotExist(err) {
				t.Fatal("rejected migration fenced worker", err)
			}
			s, err = Open(path, before, false)
			must(t, err)
			s.Close()
		})
	}
}

func TestLEDMappingMigrationPreservesUnchangedFaults(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	s, err := Open(path, before, false)
	must(t, err)
	var id string
	must(t, s.transaction(context.Background(), func(tx *sql.Tx) error {
		var e error
		id, e = s.currentLEDState(tx, workerLEDBinding, "critical")
		return e
	}))
	api.states[id] = led.State{ID: id, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}
	must(t, s.Close())
	_, err = MigrateLEDBindings(context.Background(), path, before, after, migrationSocket, api)
	must(t, err)
	if _, ok := api.states[id]; !ok {
		t.Fatal("unchanged fault erased")
	}
	s, err = Open(path, after, false)
	must(t, err)
	defer s.Close()
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM led_states WHERE id=?", id).Scan(&count))
	if count != 1 {
		t.Fatal("fault ledger rewritten")
	}
}

func TestLEDMappingMigrationResumeAtEveryBoundary(t *testing.T) {
	for _, boundary := range []string{"mapping_intent", "mapping_prepared", "mapping_rpc_intent", "mapping_rpc_returned", "mapping_rpc_ack", "mapping_before_commit", "mapping_committed", "mapping_unfenced"} {
		t.Run(boundary, func(t *testing.T) {
			path, before, after, api := mappingFixture(t)
			ctx := context.Background()
			stopped := false
			_, err := migrateLEDBindings(ctx, path, before, after, migrationSocket, api, func(stage string) error {
				if stage == boundary && !stopped {
					stopped = true
					return errors.New("interrupted")
				}
				return nil
			})
			if err == nil || !stopped {
				t.Fatal("boundary not exercised", boundary, err)
			}
			if boundary != "mapping_unfenced" {
				for _, c := range []Config{before, after} {
					if s, e := Open(path, c, false); e == nil {
						s.Close()
						t.Fatal("incomplete migration admitted runtime")
					}
				}
			}
			_, err = MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
			must(t, err)
			s, err := Open(path, after, false)
			must(t, err)
			defer s.Close()
			m := latestMapping(t, s)
			must(t, verifyLEDMappingOutput(ctx, api, m.Intent))
		})
	}
}

func TestLEDMappingMigrationUncertainRPCAndMissingFence(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	_, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, uncertainLED{api})
	if err == nil {
		t.Fatal("lost acknowledgement accepted")
	}
	// Removing the filesystem gate cannot bypass the independently durable DB gate.
	must(t, os.Remove(filepath.Join(path, "led-migration.json")))
	if s, e := Open(path, before, false); e == nil {
		s.Close()
		t.Fatal("deleted file gate admitted startup")
	}
	if _, e := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api); e == nil {
		t.Fatal("missing migration intent silently recreated")
	}
}

func TestLEDMappingMigrationRejectsDriftAndCorruptArchives(t *testing.T) {
	for _, damage := range []string{"config", "socket", "archive", "chain", "pin"} {
		t.Run(damage, func(t *testing.T) {
			path, before, after, api := mappingFixture(t)
			ctx := context.Background()
			if damage == "config" || damage == "socket" {
				_, err := migrateLEDBindings(ctx, path, before, after, migrationSocket, api, func(stage string) error {
					if stage == "mapping_prepared" {
						return errors.New("stop")
					}
					return nil
				})
				if err == nil {
					t.Fatal("missing interruption")
				}
				socket := migrationSocket
				if damage == "socket" {
					socket += ".other"
				} else {
					after.LEDBindings[0].Subjects = []string{"different"}
				}
				calls := len(api.cleared)
				if _, err = MigrateLEDBindings(ctx, path, before, after, socket, api); err == nil {
					t.Fatal("drift accepted")
				}
				if len(api.cleared) != calls {
					t.Fatal("drift sent RPC")
				}
				return
			}
			_, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
			must(t, err)
			s, err := Open(path, after, false)
			must(t, err)
			switch damage {
			case "archive":
				_, err = s.db.Exec("DELETE FROM led_migration_operations; DELETE FROM led_migration_heads")
			case "chain":
				_, err = s.db.Exec("DELETE FROM led_migration_operations; DELETE FROM led_migration_heads; DELETE FROM led_migrations")
			case "pin":
				_, err = s.db.Exec("UPDATE metadata SET value=json_set(value,'$.revision','n-00000000000000000000000000000000') WHERE key='led-layout'")
			}
			must(t, err)
			if _, err = s.Snapshot(ctx, filepath.Join(t.TempDir(), "snapshot")); err == nil {
				t.Fatal("corrupt mapping history snapshotted")
			}
			must(t, s.Close())
			if s, err = Open(path, after, false); err == nil {
				s.Close()
				t.Fatal("corrupt mapping history admitted")
			}
		})
	}
}

func TestLEDMappingMigrationRepeatedMappingGetsNewRevisionAndRestoresHistory(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	first, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
	must(t, err)
	second, err := MigrateLEDBindings(ctx, path, after, before, migrationSocket, api)
	must(t, err)
	if first.BeforeHash == second.AfterHash {
		t.Fatal("return to old configuration reused mapping revision")
	}
	s, err := Open(path, before, false)
	must(t, err)
	// Operational limits may change independently; old migration audit does not
	// permanently pin unrelated configuration fields.
	relaxed := before
	relaxed.MaxDatabaseBytes *= 2
	must(t, s.Close())
	s, err = Open(path, relaxed, false)
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	id := s.Identity()
	must(t, s.Close())
	j, err := prepareSnapshotRecovery(ctx, path, artifact, relaxed, nil)
	must(t, err)
	db := recoveryCandidate(t, path, j)
	must(t, checkLEDMigrations(ctx, db, relaxed, id, false))
	if err = checkLEDMigrations(ctx, db, relaxed, j.After, false); err == nil {
		t.Fatal("restored mapping archive acquired a new recovery epoch")
	}
}

func TestLEDMappingMigrationPreflightPreservesUnfamiliarNamespaces(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	s, err := Open(path, before, false)
	must(t, err)
	worker := s.identity.Worker
	must(t, s.Close())
	id := ledBindingNamespace(worker, "added") + ".baseline"
	api.states[id] = led.State{ID: id, LED: "green", Pattern: "blink-fast", Priority: 95, Persistent: true}
	original := protocol.Hash(api.states)
	calls := len(api.cleared)
	if _, err = MigrateLEDBindings(context.Background(), path, before, after, migrationSocket, api); err == nil {
		t.Fatal("unfamiliar reserved namespace adopted")
	}
	if protocol.Hash(api.states) != original || len(api.cleared) != calls {
		t.Fatal("preflight changed daemon state")
	}
}

func TestLEDMappingMigrationSchemaEightUpgrade(t *testing.T) {
	s, _, path := fixture(t)
	_, err := s.db.Exec("DROP TABLE led_migration_operations; DROP TABLE led_migration_heads; DROP TABLE led_migrations; PRAGMA user_version=8")
	must(t, err)
	must(t, s.Close())
	reopened, err := Open(path, configuration(), false)
	must(t, err)
	defer reopened.Close()
	var version int
	must(t, reopened.db.QueryRow("PRAGMA user_version").Scan(&version))
	if version != currentSchema {
		t.Fatal(version)
	}
	must(t, checkLEDMigrations(context.Background(), reopened.db, reopened.config, reopened.identity, false))
}

func TestLEDMappingMigrationFenceCannotBeRelabeled(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	_, err := migrateLEDBindings(ctx, path, before, after, migrationSocket, api, func(stage string) error {
		if stage == "mapping_prepared" {
			return errors.New("stop")
		}
		return nil
	})
	if err == nil {
		t.Fatal("boundary not reached")
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var v ledMappingIntent
	must(t, d.ReadJSON("led-migration.json", &v))
	v.Steps[0].ID = "other-application"
	must(t, d.WriteJSON("led-migration.json", v, true))
	if _, err = MigrateLEDBindings(ctx, path, before, after, migrationSocket, api); err == nil {
		t.Fatal("foreign clear admitted")
	}
}

func TestLEDMappingMigrationRecreatesLostBaselinesWithoutReplayingOldClears(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	root := 0
	// Simulate daemon persistence loss immediately after the last baseline ACK.
	_, err := migrateLEDBindings(ctx, path, before, after, migrationSocket, api, func(stage string) error {
		if stage == "mapping_rpc_ack" {
			root++
			if root == 6 {
				api.states = map[string]led.State{}
			}
		}
		return nil
	})
	if err == nil {
		t.Fatal("lost baselines accepted", root)
	}
	clears := len(api.cleared)
	_, err = MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
	must(t, err)
	if len(api.cleared) != clears {
		t.Fatal("baseline recovery replayed pre-remapping clears")
	}
	s, err := Open(path, after, false)
	must(t, err)
	defer s.Close()
	m := latestMapping(t, s)
	if m.Reconciliations != 1 {
		t.Fatal(m.Reconciliations)
	}
	must(t, verifyLEDMappingOutput(ctx, api, m.Intent))
}

func TestLEDMappingMigrationCancellationRetainsAcknowledgement(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.onClear = func(string) { cancel() }
	if _, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api); err == nil {
		t.Fatal("cancelled migration completed")
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var v ledMappingIntent
	must(t, d.ReadJSON("led-migration.json", &v))
	db, err := snapshotDB(d, "state.db", true)
	must(t, err)
	m, err := readLEDMapping(context.Background(), db, v.ID)
	must(t, err)
	must(t, db.Close())
	if m.Next != 1 || m.Outcome != "acknowledged" {
		t.Fatal("cancellation erased received acknowledgement", m)
	}
	clears := len(api.cleared)
	api.onClear = nil
	_, err = MigrateLEDBindings(context.Background(), path, before, after, migrationSocket, api)
	must(t, err)
	if len(api.cleared)-clears != 2 {
		t.Fatal("acknowledged healthy clear replayed")
	}
}

func TestLEDMappingMigrationBoundedHistoryRefusesBeforeRPC(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	for i := 0; i < maxLEDMigrations; i++ {
		_, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
		must(t, err)
		before, after = after, before
	}
	calls := len(api.cleared)
	if _, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api); err == nil {
		t.Fatal("migration history limit bypassed")
	}
	if len(api.cleared) != calls {
		t.Fatal("rejected migration touched daemon")
	}
	if _, err := os.Stat(filepath.Join(path, "led-migration.json")); !os.IsNotExist(err) {
		t.Fatal("capacity rejection fenced worker")
	}
}

func TestLEDMappingMigrationResourceAndDaemonPreflight(t *testing.T) {
	for _, failure := range []string{"space", "capacity", "moved", "pattern"} {
		t.Run(failure, func(t *testing.T) {
			path, before, after, api := mappingFixture(t)
			switch failure {
			case "space":
				before.MinFreeBytes, after.MinFreeBytes = 1<<62, 1<<62
			case "capacity":
				for i := 0; i < 128; i++ {
					id := fmt.Sprintf("foreign-%03d", i)
					api.states[id] = led.State{ID: id, LED: "green", Pattern: "blink-fast", Priority: 95, Persistent: true}
				}
			case "moved":
				for id, st := range api.states {
					if strings.HasSuffix(id, ".healthy") {
						st.LED = "red"
						api.states[id] = st
						break
					}
				}
			case "pattern":
				after.LEDBindings[0].Styles = map[string]LEDStyle{"critical": {Pattern: "not-installed", Priority: 90}}
			}
			old := protocol.Hash(api.states)
			calls := len(api.cleared)
			if _, err := MigrateLEDBindings(context.Background(), path, before, after, migrationSocket, api); err == nil {
				t.Fatal("failed preflight accepted")
			}
			if protocol.Hash(api.states) != old || len(api.cleared) != calls {
				t.Fatal("preflight modified daemon")
			}
			if _, err := os.Stat(filepath.Join(path, "led-migration.json")); !os.IsNotExist(err) {
				t.Fatal("preflight failure fenced worker")
			}
		})
	}
}

func TestLEDMappingSchemaUpgradeRejectsPinDriftAndRollsBack(t *testing.T) {
	for _, failure := range []string{"mapping", "ddl"} {
		t.Run(failure, func(t *testing.T) {
			s, _, path := fixture(t)
			_, err := s.db.Exec("DROP TABLE led_migration_operations; DROP TABLE led_migration_heads; DROP TABLE led_migrations; PRAGMA user_version=8")
			must(t, err)
			if failure == "mapping" {
				_, err = s.db.Exec("UPDATE metadata SET value=json_set(value,'$.uid',?) WHERE key='led-layout'", os.Getuid()+1)
			} else {
				_, err = s.db.Exec("CREATE TABLE led_migration_heads(occupied INTEGER)")
			}
			must(t, err)
			must(t, s.Close())
			if reopened, err := Open(path, configuration(), false); err == nil {
				reopened.Close()
				t.Fatal("invalid upgrade succeeded")
			}
			db, err := sql.Open("sqlite", filepath.Join(path, "state.db"))
			must(t, err)
			defer db.Close()
			var version, migrations int
			must(t, db.QueryRow("PRAGMA user_version").Scan(&version))
			must(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='led_migrations'").Scan(&migrations))
			if version != 8 || migrations != 0 {
				t.Fatal("partial schema migration committed", version, migrations)
			}
		})
	}
}

func TestLEDMappingLiveReconciliationRequiresPublishedRevision(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	_, err := MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
	must(t, err)
	s, err := Open(path, after, false)
	must(t, err)
	defer s.Close()
	revision, err := safefs.NewID("n")
	must(t, err)
	_, err = s.db.Exec("UPDATE metadata SET value=json_set(value,'$.revision',?) WHERE key='led-layout'", revision)
	must(t, err)
	prior := protocol.Hash(api.states)
	clears := len(api.cleared)
	if err = (&LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}).Step(ctx); err == nil {
		t.Fatal("unpublished mapping revision admitted LED RPCs")
	}
	if protocol.Hash(api.states) != prior || len(api.cleared) != clears {
		t.Fatal("invalid publication changed daemon state")
	}
}
