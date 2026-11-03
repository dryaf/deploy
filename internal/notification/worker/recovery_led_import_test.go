package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func importFixture(t *testing.T, outcome string) (*ledEvidenceFixture, RecoveryLEDEpochView, RecoveryLEDClearResult) {
	t.Helper()
	f := newLEDEvidenceFixture(t, true, false)
	ctx := context.Background()
	evidence := cleanupProposal(t, f)
	p, err := f.r.preserveLED(ctx, migrationSocket, f.inventory.InventoryID, f.inventory.InventoryHash, f.api)
	must(t, err)
	a, err := f.r.adoptLED(ctx, migrationSocket, RecoveryLEDAdoptionRequest{f.inventory.InventoryID, f.inventory.InventoryHash, p.RecordHash}, f.api)
	must(t, err)
	epoch, err := f.r.handoffLED(ctx, migrationSocket, a.RecordHash)
	must(t, err)
	var api LEDRecoveryCleaner = f.api
	guard := RecoveryLEDDomainGuard(acceptLEDDomain)
	switch outcome {
	case "observed_absent":
		delete(f.api.states, f.old)
	case "uncertain":
		api = failingRecoveryClear{LEDRecoveryCleaner: f.api}
	case "not_sent":
		calls := 0
		guard = func(context.Context, RecoveryLEDEvidence) error {
			calls++
			if calls == 2 {
				return errors.New("producer changed")
			}
			return nil
		}
	}
	result, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, api, guard)
	if outcome == "acknowledged" || outcome == "observed_absent" {
		must(t, err)
	}
	if result.Outcome != outcome {
		t.Fatal(result, err)
	}
	return f, epoch, result
}

func TestRecoveryLEDImportExactCompletedCleanup(t *testing.T) {
	for _, outcome := range []string{"acknowledged", "observed_absent"} {
		t.Run(outcome, func(t *testing.T) {
			f, epoch, result := importFixture(t, outcome)
			ctx := context.Background()
			// This imports a historical result, not a new evidence lease. It remains
			// valid after the original proof expires or the machine restarts.
			f.now = f.now.Add(time.Hour)
			f.boot = "later-boot"
			before := protocol.Hash(f.api.states)
			v, err := f.r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash(result))
			must(t, err)
			if v.State != "committed" || v.FaultID != f.old || v.ActivationAllowed || v.ClearAllowed || !v.Quarantined {
				t.Fatal(v)
			}
			var removed, retained, archived int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM led_states WHERE id=?", f.old).Scan(&removed))
			must(t, f.r.db.QueryRow("SELECT count(*) FROM led_states WHERE id=?", f.new).Scan(&retained))
			must(t, f.r.db.QueryRow("SELECT count(*) FROM "+ledEpochPrefix+"led_states WHERE id=?", f.old).Scan(&archived))
			if removed != 0 || retained != 1 || archived != 1 || protocol.Hash(f.api.states) != before {
				t.Fatal("import changed unrelated state or lost history", removed, retained, archived)
			}
			runtime := &Store{dir: f.r.dir, db: f.r.db, identity: f.r.journal.After, config: f.config, now: func() time.Time { return f.now }}
			must(t, runtime.transaction(ctx, func(tx *sql.Tx) error {
				faults, e := runtime.recoveredLEDFaults(ctx, tx)
				if e != nil {
					return e
				}
				if _, ok := faults[f.old]; ok {
					t.Fatal("imported fault still blocks runtime")
				}
				if _, ok := faults[f.new]; !ok {
					t.Fatal("newer fault lost its fence")
				}
				return nil
			}))
			view, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
			must(t, err)
			if len(view.CleanupImports) != 1 || view.CleanupImports[0] != v || !view.SourcesMatch {
				t.Fatal(view.CleanupImports)
			}
			must(t, f.r.Close())
			r, err := openRecoveryReconciler(ctx, f.path, f.config, nil)
			must(t, err)
			defer r.Close()
			again, err := r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash(result))
			must(t, err)
			if again != v {
				t.Fatal("retry replaced original import")
			}
			if live, err := Open(f.path, f.config, false); err == nil {
				live.Close()
				t.Fatal("import released quarantine")
			}
		})
	}
}

func TestRecoveryLEDImportRefusesUncertainAndNotSent(t *testing.T) {
	for _, outcome := range []string{"uncertain", "not_sent"} {
		t.Run(outcome, func(t *testing.T) {
			f, epoch, result := importFixture(t, outcome)
			if _, err := f.r.importLEDCleanup(context.Background(), migrationSocket, epoch.RecordHash, protocol.Hash(result)); err == nil {
				t.Fatal("unsuccessful cleanup imported")
			}
			if _, err := os.Stat(filepath.Join(f.r.stage.File().Name(), ledImportHead)); !os.IsNotExist(err) {
				t.Fatal("failed import published intent", err)
			}
		})
	}
}

func TestRecoveryLEDImportChainAndPendingIsolation(t *testing.T) {
	f, epoch, first := importFixture(t, "acknowledged")
	ctx := context.Background()
	second, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.new, first.EvidenceHash, f.api, acceptLEDDomain)
	must(t, err)
	one, err := f.r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash(first))
	must(t, err)
	f.r.fault = func(at string) error {
		if at == "led_import_intent" {
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err = f.r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash(second)); err == nil {
		t.Fatal("boundary missed")
	}
	f.r.fault = nil
	runtime := &Store{dir: f.r.dir, db: f.r.db, identity: f.r.journal.After, config: f.config, now: func() time.Time { return f.now }}
	if err = runtime.transaction(ctx, func(tx *sql.Tx) error { _, e := runtime.recoveredLEDFaults(ctx, tx); return e }); err == nil {
		t.Fatal("runtime accepted incomplete cleanup import")
	}
	if _, err = f.r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash("different")); err == nil {
		t.Fatal("pending import replaced")
	}
	two, err := f.r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash(second))
	must(t, err)
	replay, err := f.r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash(first))
	must(t, err)
	if replay != one || one.RecordHash == two.RecordHash {
		t.Fatal("historical import replaced")
	}
	v, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if len(v.CleanupImports) != 2 || v.CleanupImports[0] != one || v.CleanupImports[1] != two || len(v.ImportableCleanup) != 0 {
		t.Fatal(v.CleanupImports, v.ImportableCleanup)
	}
	// Restoring an old independent head cannot truncate acknowledged later imports.
	var old recoveryLEDImport
	must(t, f.r.stage.ReadJSON("led-import-1.json", &old))
	must(t, f.r.stage.WriteJSON(ledImportHead, old, true))
	if _, err = f.r.reviewLEDInventory(ctx, migrationSocket, nil, false); err == nil {
		t.Fatal("truncated import head accepted")
	}
}

func TestRecoveryLEDImportInterrupted(t *testing.T) {
	for _, point := range []string{"led_import_intent", "led_import_file", "led_import_rows", "led_import_transaction", "led_import_commit"} {
		t.Run(point, func(t *testing.T) {
			f, epoch, result := importFixture(t, "acknowledged")
			f.r.fault = func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := f.r.importLEDCleanup(context.Background(), migrationSocket, epoch.RecordHash, protocol.Hash(result)); err == nil {
				t.Fatal("boundary missed")
			}
			var intent recoveryLEDImport
			must(t, f.r.stage.ReadJSON(ledImportHead, &intent))
			must(t, f.r.Close())
			r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil)
			must(t, err)
			defer r.Close()
			v, err := r.importLEDCleanup(context.Background(), migrationSocket, epoch.RecordHash, protocol.Hash(result))
			must(t, err)
			if v.RecordHash != protocol.Hash(intent) || !v.PreparedAt.Equal(intent.PreparedAt) {
				t.Fatal("intent changed on resume")
			}
		})
	}
}

func TestRecoveryLEDImportDamagedProof(t *testing.T) {
	for _, damage := range []string{"head", "file", "ack", "receipt", "row-restored", "unrelated-row", "archive"} {
		t.Run(damage, func(t *testing.T) {
			f, epoch, result := importFixture(t, "acknowledged")
			ctx := context.Background()
			_, err := f.r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash(result))
			must(t, err)
			query := ""
			switch damage {
			case "head":
				must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), ledImportHead)))
			case "file":
				must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), "led-import-1.json")))
			case "ack":
				query = "DELETE FROM reconciliation_led_imports"
			case "receipt":
				query = "UPDATE reconciliation_led_clears SET result=''"
			case "row-restored":
				query = "INSERT INTO led_states SELECT * FROM " + ledEpochPrefix + "led_states WHERE id='" + f.old + "'"
			case "unrelated-row":
				query = "DELETE FROM led_states"
			case "archive":
				query = "DELETE FROM " + ledEpochPrefix + "led_states"
			}
			if query != "" {
				_, err = f.r.db.Exec(query)
				must(t, err)
			}
			if _, err = f.r.importLEDCleanup(ctx, migrationSocket, epoch.RecordHash, protocol.Hash(result)); err == nil {
				t.Fatal("damaged import accepted")
			}
			runtime := &Store{dir: f.r.dir, db: f.r.db, identity: f.r.journal.After, config: f.config, now: func() time.Time { return f.now }}
			before := protocol.Hash(f.api.states)
			if err = (&LEDReconciler{Store: runtime, API: f.api, Budget: time.Second}).Step(ctx); err == nil {
				t.Fatal("runtime accepted damaged import")
			}
			if protocol.Hash(f.api.states) != before {
				t.Fatal("damaged import allowed daemon mutation")
			}
		})
	}
}

func TestRecoveryLEDImportPreflight(t *testing.T) {
	for _, failure := range []string{"socket", "epoch", "result", "clock", "storage", "source", "canceled", "lease"} {
		t.Run(failure, func(t *testing.T) {
			f, epoch, result := importFixture(t, "acknowledged")
			ctx := context.Background()
			socket, epochHash, resultHash := migrationSocket, epoch.RecordHash, protocol.Hash(result)
			switch failure {
			case "socket":
				socket = "/tmp/other.sock"
			case "epoch":
				epochHash = protocol.Hash("other")
			case "result":
				resultHash = protocol.Hash("other")
			case "clock":
				f.now = f.now.Add(-time.Hour)
			case "storage":
				_, err := f.r.db.Exec("CREATE TABLE test_import_capacity(data BLOB); INSERT INTO test_import_capacity VALUES(zeroblob(?))", f.config.MaxDatabaseBytes)
				must(t, err)
			case "source":
				original, err := snapshotDB(f.r.dir, "state.db", false)
				must(t, err)
				_, err = original.Exec("UPDATE led_states SET obsolete=1-obsolete")
				must(t, err)
				must(t, original.Close())
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "lease":
				must(t, os.Rename(filepath.Join(f.path, "worker.lock"), filepath.Join(f.path, "old-worker.lock")))
				must(t, os.WriteFile(filepath.Join(f.path, "worker.lock"), nil, 0600))
			}
			if _, err := f.r.importLEDCleanup(ctx, socket, epochHash, resultHash); err == nil {
				t.Fatal("unsafe import accepted", failure)
			}
			if _, err := os.Stat(filepath.Join(f.r.stage.File().Name(), ledImportHead)); !os.IsNotExist(err) {
				t.Fatal("failed preflight published import intent", err)
			}
			var rows int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM led_states WHERE id=?", f.old).Scan(&rows))
			if rows != 1 {
				t.Fatal("failed preflight removed inherited row")
			}
		})
	}
}
