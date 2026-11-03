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
)

func epochFixture(t *testing.T, migrated bool) (*recoveryReconciler, string, Config, *fakeLED, string) {
	t.Helper()
	ctx := context.Background()
	var s *Store
	var c Config
	var path string
	var err error
	api := &fakeLED{states: map[string]led.State{}}
	if migrated {
		var before Config
		path, before, c, api = mappingFixture(t)
		_, err = MigrateLEDBindings(ctx, path, before, c, migrationSocket, api)
		must(t, err)
		s, err = Open(path, c, false)
		must(t, err)
	} else {
		s, _, path = bindingFixture(t, binding("selected", "backup"))
		c = s.config
	}
	v := snapshot(time.Now().UTC(), 2, 2)
	v.Subjects[0].Condition = "critical"
	s.now = time.Now
	must(t, s.Replace(ctx, v))
	must(t, (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(ctx))
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	must(t, s.Close())
	must(t, PrepareRecoveryQuarantine(ctx, path, artifact, c))
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	must(t, err)
	t.Cleanup(func() { r.Close() })
	i, err := r.reviewLEDInventory(ctx, migrationSocket, api, false)
	must(t, err)
	p, err := r.preserveLED(ctx, migrationSocket, i.InventoryID, i.InventoryHash, api)
	must(t, err)
	a, err := r.adoptLED(ctx, migrationSocket, RecoveryLEDAdoptionRequest{i.InventoryID, i.InventoryHash, p.RecordHash}, api)
	must(t, err)
	return r, path, c, api, a.RecordHash
}

func TestRecoveryLEDEpochArchivesHistoryAndRetainsFaults(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "migrated"}[migrated], func(t *testing.T) {
			r, path, c, api, hash := epochFixture(t, migrated)
			ctx := context.Background()
			before := protocol.Hash(api.states)
			e, err := r.readLEDAdoption(ctx, r.db)
			must(t, err)
			v, err := r.handoffLED(ctx, migrationSocket, hash)
			must(t, err)
			if v.State != "committed" || v.Faults != len(e.After.Faults) || !v.Quarantined || v.ClearAllowed || v.ActivationAllowed {
				t.Fatal(v)
			}
			if before != protocol.Hash(api.states) {
				t.Fatal("handoff contacted daemon")
			}
			archive, err := r.readLEDSourceLimit(ctx, ledEpochReader{r.db}, r.journal.After, 2*maxRecoveryLEDFaults)
			must(t, err)
			if protocol.Hash(archive) != protocol.Hash(e.After) {
				t.Fatal("archive lost original source")
			}
			var active, mappings int
			must(t, r.db.QueryRow("SELECT count(*) FROM led_operations").Scan(&active))
			must(t, r.db.QueryRow("SELECT count(*) FROM "+ledEpochPrefix+"led_migrations").Scan(&mappings))
			if active != 0 || (migrated && mappings != 1) {
				t.Fatal(active, mappings)
			}
			review, err := r.reviewLEDInventory(ctx, migrationSocket, nil, false)
			must(t, err)
			if !review.SourcesMatch || review.EpochHandoff == nil || *review.EpochHandoff != v {
				t.Fatal(review.EpochHandoff)
			}
			s := &Store{dir: r.dir, db: r.db, identity: r.journal.After, config: c, now: time.Now}
			base := ledBaseline(s.identity.Worker, ledLayoutOutputs(desiredLEDLayout(c, s.identity.Worker, os.Getuid()))[0])
			_, _, err = (&LEDReconciler{Store: s, API: api, Budget: time.Second}).mutateLED(ctx, "set", base.ID, &base, nil)
			must(t, err)
			// Retrying cannot reset new-epoch history that followed the handoff.
			again, err := r.handoffLED(ctx, migrationSocket, hash)
			must(t, err)
			if again != v {
				t.Fatal("retry renewed epoch transition")
			}
			must(t, r.db.QueryRow("SELECT count(*) FROM led_operations").Scan(&active))
			if active != 1 {
				t.Fatal("retry erased current history")
			}
			must(t, r.Close())
			reopened, err := openRecoveryReconciler(ctx, path, c, nil)
			must(t, err)
			defer reopened.Close()
			again, err = reopened.handoffLED(ctx, migrationSocket, hash)
			must(t, err)
			if again != v {
				t.Fatal("reopen changed handoff")
			}
			if live, err := Open(path, c, false); err == nil {
				live.Close()
				t.Fatal("handoff released quarantine")
			}
		})
	}
}

func TestRecoveryLEDEpochInterruptedAndExactRetry(t *testing.T) {
	for _, point := range []string{"led_epoch_intent", "led_epoch_archive", "led_epoch_reset", "led_epoch_transaction", "led_epoch_commit"} {
		t.Run(point, func(t *testing.T) {
			r, path, c, _, hash := epochFixture(t, true)
			r.fault = func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := r.handoffLED(context.Background(), migrationSocket, hash); err == nil {
				t.Fatal("boundary missed")
			}
			var original recoveryLEDEpoch
			must(t, r.stage.ReadJSON(ledEpochFile, &original))
			must(t, r.Close())
			r, err := openRecoveryReconciler(context.Background(), path, c, nil)
			must(t, err)
			defer r.Close()
			v, err := r.handoffLED(context.Background(), migrationSocket, hash)
			must(t, err)
			if v.RecordHash != protocol.Hash(original) || !v.PreparedAt.Equal(original.PreparedAt) {
				t.Fatal("retry replaced original intent")
			}
			if _, err = r.handoffLED(context.Background(), migrationSocket, protocol.Hash("other")); err == nil {
				t.Fatal("conflicting retry accepted")
			}
		})
	}
}

func TestRecoveryLEDEpochRefusesDamagedEvidence(t *testing.T) {
	for _, change := range []string{"file", "ack", "archive-state", "archive-operation", "archive-mapping", "archive-metadata", "layout", "missing-table", "extra-table", "fault-row"} {
		t.Run(change, func(t *testing.T) {
			r, _, c, api, hash := epochFixture(t, true)
			_, err := r.handoffLED(context.Background(), migrationSocket, hash)
			must(t, err)
			query := ""
			switch change {
			case "file":
				must(t, os.Remove(filepath.Join(r.stage.File().Name(), ledEpochFile)))
			case "ack":
				query = "DELETE FROM reconciliation_led_epoch"
			case "archive-state":
				query = "DELETE FROM " + ledEpochPrefix + "led_states"
			case "archive-operation":
				query = "DELETE FROM " + ledEpochPrefix + "led_operations"
			case "archive-mapping":
				query = "DELETE FROM " + ledEpochPrefix + "led_migrations"
			case "archive-metadata":
				query = "UPDATE " + ledEpochPrefix + "metadata SET value='{}' WHERE key='identity'"
			case "layout":
				query = "UPDATE metadata SET value='{}' WHERE key='led-layout'"
			case "missing-table":
				query = "DROP TABLE " + ledEpochPrefix + "led_states"
			case "extra-table":
				query = "CREATE TABLE " + ledEpochPrefix + "unowned(id TEXT)"
			case "fault-row":
				query = "DELETE FROM led_states"
			}
			if query != "" {
				_, err = r.db.Exec(query)
				must(t, err)
			}
			if _, err = r.handoffLED(context.Background(), migrationSocket, hash); err == nil {
				t.Fatal("damaged handoff accepted", change)
			}
			before := protocol.Hash(api.states)
			s := &Store{dir: r.dir, db: r.db, identity: r.journal.After, config: c, now: time.Now}
			if err = (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(context.Background()); err == nil {
				t.Fatal("runtime accepted damaged archive", change)
			}
			if protocol.Hash(api.states) != before {
				t.Fatal("runtime mutated daemon with damaged authority", change)
			}
		})
	}
}

func TestRecoveryLEDEpochPreservedSourceDrift(t *testing.T) {
	for _, phase := range []string{"before", "intent", "committed"} {
		t.Run(phase, func(t *testing.T) {
			r, _, _, _, hash := epochFixture(t, true)
			ctx := context.Background()
			if phase == "committed" {
				_, err := r.handoffLED(ctx, migrationSocket, hash)
				must(t, err)
			}
			change := func() {
				original, err := snapshotDB(r.dir, "state.db", false)
				must(t, err)
				_, err = original.Exec("UPDATE led_states SET obsolete=1-obsolete")
				must(t, err)
				must(t, original.Close())
			}
			if phase == "intent" {
				r.fault = func(point string) error {
					if point == "led_epoch_intent" {
						change()
					}
					return nil
				}
			} else {
				change()
			}
			if _, err := r.handoffLED(ctx, migrationSocket, hash); err == nil {
				t.Fatal("handoff accepted changed original")
			}
			exists, err := ledEpochSchema(ctx, r.db)
			must(t, err)
			if exists {
				var count int
				must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_led_epoch").Scan(&count))
				if (count == 1) != (phase == "committed") {
					t.Fatal("failed source guard changed commit", phase, count)
				}
			}
		})
	}
}

func TestRecoveryLEDEpochPreflightRefusesWithoutIntent(t *testing.T) {
	for _, failure := range []string{"socket", "hash", "canceled", "storage", "lease"} {
		t.Run(failure, func(t *testing.T) {
			r, _, _, _, hash := epochFixture(t, false)
			ctx := context.Background()
			socket := migrationSocket
			switch failure {
			case "socket":
				socket = "/tmp/other-led.sock"
			case "hash":
				hash = protocol.Hash("other adoption")
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "storage":
				_, err := r.db.Exec("CREATE TABLE test_epoch_capacity(data BLOB); INSERT INTO test_epoch_capacity VALUES(zeroblob(?))", r.config.MaxDatabaseBytes)
				must(t, err)
			case "lease":
				must(t, os.Rename(filepath.Join(r.dir.File().Name(), "worker.lock"), filepath.Join(r.dir.File().Name(), "old-worker.lock")))
				must(t, os.WriteFile(filepath.Join(r.dir.File().Name(), "worker.lock"), nil, 0600))
			}
			if _, err := r.handoffLED(ctx, socket, hash); err == nil {
				t.Fatal("invalid preflight accepted", failure)
			}
			if _, err := os.Stat(filepath.Join(r.stage.File().Name(), ledEpochFile)); !os.IsNotExist(err) {
				t.Fatal("failed preflight published intent", err)
			}
			exists, err := ledEpochSchema(context.Background(), r.db)
			must(t, err)
			if exists {
				t.Fatal("failed preflight created archive schema")
			}
		})
	}
}

func TestRecoveryLEDEpochRuntimePreservesOldFaultFence(t *testing.T) {
	r, _, c, api, hash := epochFixture(t, true)
	ctx := context.Background()
	_, err := r.handoffLED(ctx, migrationSocket, hash)
	must(t, err)
	_, err = r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	s := &Store{dir: r.dir, db: r.db, identity: r.journal.After, config: c, now: time.Now}
	old, err := readLEDEpochFaults(ctx, r.db)
	must(t, err)
	before := protocol.Hash(api.states)
	must(t, (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(ctx))
	if before == protocol.Hash(api.states) {
		t.Fatal("runtime never established new baselines/faults")
	}
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		for _, f := range old {
			if err := s.checkLEDState(tx, f.ID, f.Binding, f.Identity); !errors.Is(err, errLEDRecoveryPending) {
				t.Fatal("handoff enabled old clear", err)
			}
		}
		return nil
	}))
	must(t, checkLEDOperations(ctx, r.db, r.journal.After))
	archive, err := r.readLEDAdoptionAuthority(ctx, r.db)
	must(t, err)
	_, phase, err := r.readLEDEpoch(ctx, r.db, archive)
	must(t, err)
	if phase != "committed" {
		t.Fatal(phase)
	}
}
