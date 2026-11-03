package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type ledRecoveryFixture struct {
	r                *recoveryReconciler
	path             string
	config           Config
	api              *inventoryLEDObserver
	shared, old, new string
	now              time.Time
	boot             string
	uptime           float64
}

type inventoryLEDObserver struct {
	*fakeLED
	extra     []led.Entry
	calls     int
	read      func()
	statusErr error
}

func (f *inventoryLEDObserver) Status(ctx context.Context) (led.DaemonStatus, error) {
	f.calls++
	if f.statusErr != nil {
		return led.DaemonStatus{}, f.statusErr
	}
	return f.fakeLED.Status(ctx)
}
func (f *inventoryLEDObserver) List(ctx context.Context) ([]led.Entry, error) {
	f.calls++
	if f.read != nil {
		f.read()
	}
	entries, err := f.fakeLED.List(ctx)
	return append(entries, f.extra...), err
}

func newLEDRecoveryFixture(t *testing.T, extraBindings ...LEDBinding) *ledRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	bindings := append([]LEDBinding{binding("selected", "backup")}, extraBindings...)
	s, _, path := bindingFixture(t, bindings...)
	f := &ledRecoveryFixture{path: path, config: s.config, now: time.Now().UTC(), boot: "boot-one", uptime: 100}
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		f.shared, err = s.currentLEDState(tx, workerLEDBinding, "critical")
		if err != nil {
			return err
		}
		f.old, err = s.currentLEDState(tx, "selected", "critical")
		return err
	}))
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err := s.Snapshot(ctx, artifact)
	must(t, err)
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		if _, e := tx.Exec("DELETE FROM led_states WHERE id=?", f.old); e != nil {
			return e
		}
		if _, e := tx.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", f.shared); e != nil {
			return e
		}
		var e error
		f.new, e = s.currentLEDState(tx, "selected", "warning")
		return e
	}))
	identity := s.identity
	must(t, s.Close())
	_, err = prepareSnapshotRecovery(ctx, path, artifact, f.config, nil)
	must(t, err)
	f.r, err = openRecoveryReconciler(ctx, path, f.config, nil)
	must(t, err)
	f.r.now = func() time.Time { return f.now }
	f.r.clock = func() (string, float64, error) { return f.boot, f.uptime, nil }
	t.Cleanup(func() { f.r.Close() })
	states := map[string]led.State{}
	for _, id := range []string{f.shared, f.old, f.new} {
		states[id] = led.State{ID: id, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true, Message: "private application diagnostic"}
	}
	ttl := 60
	healthy := ledBindingNamespace(identity.Worker, workerLEDBinding) + ".healthy"
	states[healthy] = led.State{ID: healthy, LED: "green", Pattern: "heartbeat", Priority: 10, TTL: &ttl}
	// A convincing-looking prefix is never sufficient ownership evidence.
	states["deploy.f.unrecognized"] = led.State{ID: "deploy.f.unrecognized", LED: "red", Pattern: "blink-fast", Priority: 95, Persistent: true}
	f.api = &inventoryLEDObserver{fakeLED: &fakeLED{states: states}, extra: []led.Entry{{State: led.State{ID: f.shared, LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true, Message: "foreign private message"}, OwnerUID: os.Getuid() + 1}}}
	return f
}

func inventoryItem(t *testing.T, v RecoveryLEDReview, id string) RecoveryLEDItem {
	t.Helper()
	for _, item := range v.Items {
		if item.ID == id && item.UID == os.Getuid() {
			return item
		}
	}
	t.Fatal("missing inventory item", id)
	return RecoveryLEDItem{}
}

func TestRecoveryLEDInventoryPreservesPostSnapshotFaultsAndForeignStates(t *testing.T) {
	f := newLEDRecoveryFixture(t)
	before := protocol.Hash(f.api.states)
	v, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, false)
	must(t, err)
	if !v.Quarantined || v.ClearAllowed || v.ActivationAllowed || v.VisualVerified || !v.ObservationCurrent || !v.SourcesMatch || !v.MappingsMatch || !v.RuntimeUIDMatches {
		t.Fatal(v)
	}
	if protocol.Hash(f.api.states) != before || len(f.api.cleared) != 0 {
		t.Fatal("inventory mutated LEDs")
	}
	if v.Counts["post_snapshot_faults"] != 1 || v.Counts["snapshot_only_faults"] != 1 || v.Counts["foreign"] != 1 || v.Counts["unrecognized"] != 1 {
		t.Fatal(v.Counts)
	}
	shared := inventoryItem(t, v, f.shared)
	if shared.Provenance != "both" || shared.Snapshot.CleanupPending || !shared.Latest.CleanupPending || !shared.OwnershipVerified {
		t.Fatal(shared)
	}
	if item := inventoryItem(t, v, f.old); item.Provenance != "snapshot" || item.Classification != "known_fault" {
		t.Fatal(item)
	}
	if item := inventoryItem(t, v, f.new); item.Provenance != "latest" || item.Latest.Condition != "warning" {
		t.Fatal(item)
	}
	if item := inventoryItem(t, v, "deploy.f.unrecognized"); item.OwnershipVerified || item.Classification != "unrecognized" {
		t.Fatal(item)
	}
	raw, err := json.Marshal(v)
	must(t, err)
	if strings.Contains(string(raw), "private application") || strings.Contains(string(raw), "foreign private") {
		t.Fatal("free-form daemon messages leaked into review")
	}
	calls := f.api.calls
	again, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, nil, false)
	must(t, err)
	if again.InventoryID != v.InventoryID || again.InventoryHash != v.InventoryHash || f.api.calls != calls {
		t.Fatal("reading inventory renewed observation")
	}
	must(t, f.r.Close())
	if s, err := Open(f.path, f.config, false); err == nil {
		s.Close()
		t.Fatal("review activated quarantined worker")
	}
	r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil)
	must(t, err)
	defer r.Close()
	saved, err := r.reviewLEDInventory(context.Background(), migrationSocket, nil, false)
	must(t, err)
	if saved.InventoryHash != v.InventoryHash {
		t.Fatal("restart changed inventory")
	}
}

func TestRecoveryLEDInventoryFreshnessAndExplicitRefresh(t *testing.T) {
	f := newLEDRecoveryFixture(t)
	ctx := context.Background()
	initial, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, false)
	must(t, err)
	f.now = f.now.Add(31 * time.Second)
	f.uptime += 31
	old, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if old.ObservationCurrent || old.InventoryID != initial.InventoryID {
		t.Fatal("read renewed expired inventory")
	}
	refreshed, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true)
	must(t, err)
	if !refreshed.ObservationCurrent || refreshed.InventoryID == initial.InventoryID || refreshed.Sequence != 2 {
		t.Fatal(refreshed)
	}
	f.boot = "boot-two"
	f.uptime = 1
	old, err = f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if old.ObservationCurrent {
		t.Fatal("observation survived boot change")
	}
	f.boot = "boot-one"
	f.uptime = 132
	f.now = initial.ObservedAt
	old, err = f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if old.ObservationCurrent {
		t.Fatal("wall-clock rollback renewed observation")
	}
}

func TestRecoveryLEDInventoryDetectsSourceDriftAndConflicts(t *testing.T) {
	f := newLEDRecoveryFixture(t)
	ctx := context.Background()
	initial, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, false)
	must(t, err)
	_, err = f.r.db.Exec("UPDATE led_states SET condition='warning' WHERE id=?", f.shared)
	must(t, err)
	stale, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if stale.SourcesMatch || stale.ObservationCurrent || stale.InventoryHash != initial.InventoryHash {
		t.Fatal("source drift hidden")
	}
	next, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true)
	must(t, err)
	if !next.SourcesChangedSincePrevious || next.Counts["conflict"] != 1 {
		t.Fatal(next.Counts)
	}
	item := inventoryItem(t, next, f.shared)
	if item.Conflict != "fault_identity_changed" || item.OwnershipVerified {
		t.Fatal(item)
	}
}

func TestRecoveryLEDInventoryRejectsChangedSourceDuringRPCWithoutHoldingMutex(t *testing.T) {
	f := newLEDRecoveryFixture(t)
	ctx := context.Background()
	f.api.read = func() {
		must(t, f.r.transaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE led_states SET condition='warning' WHERE id=?", f.shared)
			return err
		}))
	}
	if _, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, false); err == nil {
		t.Fatal("changed source sealed as one observation")
	}
	var count int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_led_inventories").Scan(&count))
	if count != 0 {
		t.Fatal("stale capture persisted")
	}
	var head recoveryLEDInventory
	if err := f.r.stage.ReadJSON(recoveryLEDHead, &head); !os.IsNotExist(err) {
		t.Fatal("stale capture published intent", err)
	}
}

func TestRecoveryLEDInventoryBoundsAndEndpointPin(t *testing.T) {
	f := newLEDRecoveryFixture(t)
	ctx := context.Background()
	for i := 0; i < maxRecoveryLEDInventories; i++ {
		v, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, i > 0)
		must(t, err)
		if v.Sequence != i+1 {
			t.Fatal(v.Sequence)
		}
	}
	calls := f.api.calls
	if _, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true); err == nil {
		t.Fatal("unbounded inventory history")
	}
	if _, err := f.r.reviewLEDInventory(ctx, migrationSocket+".changed", f.api, false); err == nil {
		t.Fatal("endpoint pin bypassed")
	}
	if f.api.calls != calls {
		t.Fatal("rejected request contacted daemon")
	}
	_, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
}

func TestRecoveryLEDInventoryFailsClosedOnCorruption(t *testing.T) {
	for _, damage := range []string{"row", "archive", "head", "marker", "epoch", "lost-row"} {
		t.Run(damage, func(t *testing.T) {
			f := newLEDRecoveryFixture(t)
			ctx := context.Background()
			v, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, false)
			must(t, err)
			switch damage {
			case "row":
				_, err = f.r.db.Exec("UPDATE reconciliation_led_inventories SET hash='bad'")
			case "archive":
				err = f.r.stage.WriteJSON(recoveryLEDInventoryFile(v.InventoryID), map[string]any{"version": 99}, true)
			case "head":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), recoveryLEDHead))
			case "marker":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='led-inventory-version'")
			case "epoch":
				var head recoveryLEDInventory
				must(t, f.r.stage.ReadJSON(recoveryLEDHead, &head))
				head.Recovery.After.Recovery = "other-epoch"
				err = f.r.stage.WriteJSON(recoveryLEDHead, head, true)
			case "lost-row":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_led_inventories; DELETE FROM metadata WHERE key='led-inventory-version'")
			}
			must(t, err)
			must(t, f.r.Close())
			if r, err := openRecoveryReconciler(ctx, f.path, f.config, nil); err == nil {
				r.Close()
				t.Fatal("damaged inventory admitted recovery session")
			}
		})
	}
}

func TestRecoveryLEDInventoryResumesHistoricalCaptureWithoutRenewal(t *testing.T) {
	for _, boundary := range []string{"led_inventory_schema", "led_inventory_intent", "led_inventory_archive", "led_inventory_before_commit", "led_inventory_committed"} {
		t.Run(boundary, func(t *testing.T) {
			f := newLEDRecoveryFixture(t)
			ctx := context.Background()
			stopped := false
			f.r.fault = func(stage string) error {
				if stage == boundary && !stopped {
					stopped = true
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, false); err == nil || !stopped {
				t.Fatal("boundary not reached", err)
			}
			f.r.fault = nil
			calls := f.api.calls
			var original recoveryLEDInventory
			err := f.r.stage.ReadJSON(recoveryLEDHead, &original)
			if boundary != "led_inventory_schema" {
				must(t, err)
			}
			f.now = f.now.Add(time.Minute)
			f.uptime += 60
			v, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, false)
			must(t, err)
			if boundary != "led_inventory_schema" && (v.InventoryID != original.ID || v.ObservationCurrent || f.api.calls != calls) {
				t.Fatal("resume renewed historical observation", v)
			}
		})
	}
}

func TestRecoveryLEDInventoryRejectsBadRegistryAndDamagedOwnership(t *testing.T) {
	for _, failure := range []string{"duplicate", "bounds", "identity", "source-epoch"} {
		t.Run(failure, func(t *testing.T) {
			f := newLEDRecoveryFixture(t)
			switch failure {
			case "duplicate":
				f.api.extra = append(f.api.extra, f.api.extra[0])
			case "bounds":
				for i := 0; i < 1025; i++ {
					f.api.extra = append(f.api.extra, led.Entry{})
				}
			case "identity":
				_, err := f.r.db.Exec("UPDATE led_states SET identity='{}' WHERE id=?", f.shared)
				must(t, err)
			case "source-epoch":
				d, err := snapshotDB(f.r.dir, "state.db", false)
				must(t, err)
				_, err = d.Exec("UPDATE metadata SET value='{}' WHERE key='identity'")
				must(t, err)
				must(t, d.Close())
			}
			if _, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, false); err == nil {
				t.Fatal("invalid inventory accepted")
			}
			if (failure == "identity" || failure == "source-epoch") && f.api.calls != 0 {
				t.Fatal("invalid source contacted daemon")
			}
		})
	}
}

func TestRecoveryLEDInventoryTargetConflictIsDiagnosticOnly(t *testing.T) {
	f := newLEDRecoveryFixture(t)
	f.api.onStatus = func() {
		for id, st := range f.api.states {
			if id == f.shared {
				st.LED = "red"
				f.api.states[id] = st
			}
		}
	}
	v, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, false)
	must(t, err)
	item := inventoryItem(t, v, f.shared)
	if item.Classification != "conflict" || item.Conflict != "daemon_target_differs" || v.ClearAllowed || v.ActivationAllowed {
		t.Fatal(item)
	}
}

func TestRecoveryLEDInventoryRejectsExpiredCaptureBeforeIntent(t *testing.T) {
	for _, change := range []string{"elapsed", "reboot", "clock-error", "status-error", "capacity"} {
		t.Run(change, func(t *testing.T) {
			f := newLEDRecoveryFixture(t)
			f.api.read = func() {
				switch change {
				case "elapsed":
					f.now = f.now.Add(30 * time.Second)
					f.uptime += 30
				case "reboot":
					f.boot = "new-boot"
				case "clock-error":
					f.r.clock = func() (string, float64, error) { return "", 0, errors.New("clock unavailable") }
				case "capacity":
					// Preserve configuration identity; make the actual DB exceed its budget.
					_, err := f.r.db.Exec("CREATE TABLE test_capacity(data BLOB); INSERT INTO test_capacity VALUES(zeroblob(?))", f.config.MaxDatabaseBytes)
					must(t, err)
				}
			}
			if change == "status-error" {
				f.api.statusErr = errors.New("daemon unavailable")
			}
			if _, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, false); err == nil {
				t.Fatal("invalid capture accepted")
			}
			var head recoveryLEDInventory
			if err := f.r.stage.ReadJSON(recoveryLEDHead, &head); !os.IsNotExist(err) {
				t.Fatal("invalid capture published intent", err)
			}
		})
	}
}

func TestRecoveryLEDInventoryPreservesDistinctMappingRevisions(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	s, err := Open(path, before, false)
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	must(t, s.Close())
	_, err = MigrateLEDBindings(ctx, path, before, after, migrationSocket, api)
	must(t, err)
	_, err = MigrateLEDBindings(ctx, path, after, before, migrationSocket, api)
	must(t, err)
	_, err = prepareSnapshotRecovery(ctx, path, artifact, before, nil)
	must(t, err)
	v, err := ReviewRecoveryLED(ctx, path, before, migrationSocket, api, false)
	must(t, err)
	if v.MappingsMatch || !v.RuntimeUIDMatches || !v.SourcesMatch || v.ActivationAllowed || v.ClearAllowed {
		t.Fatal("return to previous config concealed newer mapping history", v)
	}
}

func TestRecoveryLEDInventoryLegacySourceCannotProveEpochOwnership(t *testing.T) {
	s, _, path := fixture(t)
	ctx := context.Background()
	downgradeLEDLayout(t, s)
	_, err := s.db.Exec("INSERT INTO led_states(id,condition) VALUES('legacy-fault','critical'); PRAGMA user_version=6")
	must(t, err)
	must(t, s.Close())
	s, err = Open(path, configuration(), false)
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	m, err := s.Snapshot(ctx, artifact)
	must(t, err)
	d, err := safefs.Open(artifact)
	must(t, err)
	defer d.Close()
	db, err := snapshotDB(d, "state.db", false)
	must(t, err)
	_, err = db.Exec("DROP TABLE led_migration_operations; DROP TABLE led_migration_heads; DROP TABLE led_migrations; DROP TABLE led_operations; DROP TABLE led_operation_heads; DROP INDEX led_binding_states; ALTER TABLE led_states DROP COLUMN binding; ALTER TABLE led_states DROP COLUMN identity; DELETE FROM metadata WHERE key='led-layout'; PRAGMA user_version=6")
	must(t, err)
	must(t, db.Close())
	file, err := snapshotFile(d, "state.db", s.config.MaxDatabaseBytes)
	must(t, err)
	info, err := file.Stat()
	must(t, err)
	m.DatabaseHash, err = safefs.DigestFile(file)
	must(t, err)
	must(t, file.Close())
	m.Schema, m.DatabaseBytes = 6, info.Size()
	must(t, d.WriteJSON("snapshot.json", m, true))
	must(t, s.Close())
	_, err = prepareSnapshotRecovery(ctx, path, artifact, configuration(), nil)
	must(t, err)
	api := &fakeLED{states: map[string]led.State{"legacy-fault": {ID: "legacy-fault", LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}}}
	v, err := ReviewRecoveryLED(ctx, path, configuration(), migrationSocket, api, false)
	must(t, err)
	item := inventoryItem(t, v, "legacy-fault")
	if item.Classification != "legacy_fault" || item.OwnershipVerified || v.MappingsMatch || v.RuntimeUIDMatches {
		t.Fatal("legacy observation acquired exact ownership", item)
	}
}
