package worker

import (
	"context"
	"database/sql"
	"encoding/json"
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

func adoptionFixture(t *testing.T, extraBindings ...LEDBinding) (*ledRecoveryFixture, RecoveryLEDAdoptionRequest) {
	t.Helper()
	f := newLEDRecoveryFixture(t, extraBindings...)
	// A snapshot cleanup tombstone must not become a cleanup grant after adoption.
	_, err := f.r.db.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", f.old)
	must(t, err)
	v, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, false)
	must(t, err)
	p, err := f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, f.api)
	must(t, err)
	return f, RecoveryLEDAdoptionRequest{v.InventoryID, v.InventoryHash, p.RecordHash}
}
func TestRecoveryLEDAdoptionAppliesUnionAndPreservesEvidence(t *testing.T) {
	f, request := adoptionFixture(t)
	ctx := context.Background()
	before := protocol.Hash(f.api.states)
	f.api.read = func() {
		if !f.r.mu.TryLock() {
			t.Fatal("registry call held transaction mutex")
		}
		f.r.mu.Unlock()
	}
	view, err := f.r.adoptLED(ctx, migrationSocket, request, f.api)
	must(t, err)
	if view.State != "committed" || view.Faults != 3 || !view.SourcesMatch || !view.Quarantined || view.ActivationAllowed || view.ClearAllowed {
		t.Fatal(view)
	}
	var count, pending int
	must(t, f.r.db.QueryRow("SELECT count(*),sum(obsolete) FROM led_states").Scan(&count, &pending))
	if count != 3 || pending != 0 {
		t.Fatal("partial union or executable old tombstones", count, pending)
	}
	var identity string
	must(t, f.r.db.QueryRow("SELECT identity FROM led_states WHERE id=?", f.new).Scan(&identity))
	if !strings.Contains(identity, f.r.journal.Before.Recovery) || strings.Contains(identity, f.r.journal.After.Recovery) {
		t.Fatal("fault was relabeled", identity)
	}
	review, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if !review.SourcesMatch || review.Adoption == nil || *review.Adoption != view || review.Preservation == nil || !review.Preservation.SourcesMatch || !inventoryItem(t, review, f.old).Snapshot.CleanupPending {
		t.Fatal("adoption destroyed original evidence", review)
	}
	// A fresh capture must preserve the original distinction and evidence basis.
	fresh, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true)
	must(t, err)
	if fresh.Counts["post_snapshot_faults"] != 1 || fresh.Counts["snapshot_only_faults"] != 1 || !fresh.SourcesMatch {
		t.Fatal(fresh.Counts)
	}
	calls := f.api.calls
	f.now = f.now.Add(time.Hour)
	f.boot = "new-boot"
	f.uptime = 1
	repeated, err := f.r.adoptLED(ctx, migrationSocket, request, nil)
	must(t, err)
	if repeated != view || f.api.calls != calls {
		t.Fatal("retry renewed evidence", repeated)
	}
	p, err := f.r.preserveLED(ctx, migrationSocket, request.InventoryID, request.InventoryHash, nil)
	must(t, err)
	if p.RecordHash != request.PreservationHash || !p.SourcesMatch {
		t.Fatal(p)
	}
	if protocol.Hash(f.api.states) != before || len(f.api.cleared) != 0 {
		t.Fatal("adoption mutated daemon")
	}
	must(t, f.r.Close())
	reopened, err := openRecoveryReconciler(ctx, f.path, f.config, nil)
	must(t, err)
	must(t, reopened.Close())
	if live, err := Open(f.path, f.config, false); err == nil {
		live.Close()
		t.Fatal("adoption published/activated recovery")
	}
}
func TestRecoveryLEDAdoptionRequiresFreshExactAuthority(t *testing.T) {
	for _, change := range []string{"proof", "inventory", "socket", "expiry", "reboot", "registry", "source", "source-during-rpc", "no-observer", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			f, request := adoptionFixture(t)
			socket := migrationSocket
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var api LEDRecoveryObserver = f.api
			switch change {
			case "proof":
				request.PreservationHash = protocol.Hash("wrong")
			case "inventory":
				request.InventoryHash = protocol.Hash("wrong")
			case "socket":
				socket = "/another/led.sock"
			case "expiry":
				f.now = f.now.Add(time.Minute)
				f.uptime += 60
			case "reboot":
				f.boot = "other"
			case "registry":
				st := f.api.states[f.new]
				st.Message = "new incarnation"
				f.api.states[f.new] = st
			case "source":
				_, err := f.r.db.Exec("UPDATE led_states SET obsolete=0 WHERE id=?", f.old)
				must(t, err)
			case "source-during-rpc":
				f.api.read = func() { _, err := f.r.db.Exec("UPDATE led_states SET obsolete=0 WHERE id=?", f.old); must(t, err) }
			case "no-observer":
				api = nil
			case "cancelled":
				cancel()
			}
			if _, err := f.r.adoptLED(ctx, socket, request, api); err == nil {
				t.Fatal("invalid authority accepted")
			}
			if _, err := os.Stat(filepath.Join(f.r.stage.File().Name(), recoveryLEDAdoptionFile)); !os.IsNotExist(err) {
				t.Fatal("failed preflight wrote intent", err)
			}
			var count int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM led_states").Scan(&count))
			if count != 2 {
				t.Fatal("failed preflight changed ledger")
			}
		})
	}
}
func TestRecoveryLEDAdoptionInterruptedAndConflictingRetries(t *testing.T) {
	for _, point := range []string{"led_adoption_intent", "led_adoption_rows", "led_adoption_transaction", "led_adoption_commit"} {
		t.Run(point, func(t *testing.T) {
			f, request := adoptionFixture(t)
			f.r.fault = func(p string) error {
				if p == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := f.r.adoptLED(context.Background(), migrationSocket, request, f.api); err == nil {
				t.Fatal("missed fault")
			}
			record, err := f.r.readLEDAdoption(context.Background(), f.r.db)
			must(t, err)
			if record == nil {
				t.Fatal("lost intent")
			}
			var count int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM led_states").Scan(&count))
			if (record.Phase == "prepared" && count != 2) || (record.Phase == "committed" && count != 3) {
				t.Fatal("non-atomic adoption", count, record.Phase)
			}
			f.r.fault = nil
			f.now = f.now.Add(time.Hour)
			f.boot = "rebooted"
			bad := request
			bad.PreservationHash = protocol.Hash("different")
			if _, err = f.r.adoptLED(context.Background(), migrationSocket, bad, nil); err == nil {
				t.Fatal("conflicting retry accepted")
			}
			out, err := f.r.adoptLED(context.Background(), migrationSocket, request, nil)
			must(t, err)
			if out.State != "committed" || out.RecordHash != protocol.Hash(record.Record) || !out.PreparedAt.Equal(record.Record.PreparedAt) {
				t.Fatal(out)
			}
		})
	}
}
func TestRecoveryLEDAdoptionDetectsLedgerAndJournalCorruption(t *testing.T) {
	for _, damage := range []string{"file", "schema", "ack", "lost-row", "cleanup-flag", "forged-both", "preservation", "extra-row", "history"} {
		t.Run(damage, func(t *testing.T) {
			f, request := adoptionFixture(t)
			_, err := f.r.adoptLED(context.Background(), migrationSocket, request, f.api)
			must(t, err)
			switch damage {
			case "file":
				must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), recoveryLEDAdoptionFile)))
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='led-adoption-version'")
				must(t, err)
			case "ack":
				_, err = f.r.db.Exec("UPDATE reconciliation_led_adoption SET hash=?", protocol.Hash("wrong"))
				must(t, err)
			case "lost-row":
				_, err = f.r.db.Exec("DELETE FROM led_states WHERE id=?", f.new)
				must(t, err)
			case "cleanup-flag":
				_, err = f.r.db.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", f.new)
				must(t, err)
			case "forged-both":
				var record recoveryLEDAdoption
				must(t, f.r.stage.ReadJSON(recoveryLEDAdoptionFile, &record))
				record.AfterHash = protocol.Hash("forged")
				must(t, f.r.stage.WriteJSON(recoveryLEDAdoptionFile, record, true))
				raw, e := json.Marshal(record)
				must(t, e)
				_, err = f.r.db.Exec("UPDATE reconciliation_led_adoption SET hash=?,body=?", protocol.Hash(record), string(raw))
				must(t, err)
			case "preservation":
				must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), recoveryLEDPreservationFile)))
			case "extra-row":
				_, err = f.r.db.Exec("PRAGMA ignore_check_constraints=ON; INSERT INTO reconciliation_led_adoption SELECT 2,hash,body FROM reconciliation_led_adoption WHERE singleton=1")
				must(t, err)
			case "history":
				_, err = f.r.db.Exec("UPDATE metadata SET value='{}' WHERE key='led-layout'")
				must(t, err)
			}
			if _, err = f.r.reviewLEDInventory(context.Background(), migrationSocket, nil, false); err == nil {
				t.Fatal("damaged adoption hidden by original source view")
			}
			must(t, f.r.Close())
			reopened, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil)
			if err == nil {
				reopened.Close()
				t.Fatal("damaged adoption accepted on reopen")
			}
		})
	}
}

func TestRecoveryLEDAdoptionRechecksIntentAgeAndStorage(t *testing.T) {
	for _, failure := range []string{"age", "storage"} {
		t.Run(failure, func(t *testing.T) {
			f, request := adoptionFixture(t)
			commits := 0
			f.r.fault = func(point string) error {
				if point == "reconciliation_commit" {
					commits++
					if commits == 2 && failure == "age" {
						f.now = f.now.Add(31 * time.Second)
						f.uptime += 31
					}
				}
				return nil
			}
			if failure == "storage" {
				f.api.read = func() {
					_, err := f.r.db.Exec("CREATE TABLE test_adoption_capacity(data BLOB); INSERT INTO test_adoption_capacity VALUES(zeroblob(?))", f.config.MaxDatabaseBytes)
					must(t, err)
				}
			}
			if _, err := f.r.adoptLED(context.Background(), migrationSocket, request, f.api); err == nil {
				t.Fatal("invalid intent allowed", failure)
			}
			if _, err := os.Stat(filepath.Join(f.r.stage.File().Name(), recoveryLEDAdoptionFile)); !os.IsNotExist(err) {
				t.Fatal("failed preflight wrote intent", err)
			}
		})
	}
}
func TestRecoveryLEDAdoptionCannotDischargeFaults(t *testing.T) {
	f, request := adoptionFixture(t)
	out, err := f.r.adoptLED(context.Background(), migrationSocket, request, f.api)
	must(t, err)
	before := protocol.Hash(f.api.states)
	if _, err = f.r.clearLEDFault(context.Background(), migrationSocket, request.InventoryID, "selected", f.old, out.RecordHash, f.api, acceptLEDDomain); err == nil {
		t.Fatal("adoption became domain resolution")
	}
	if protocol.Hash(f.api.states) != before || len(f.api.cleared) != 0 {
		t.Fatal("fault changed without domain evidence")
	}
}
func TestRecoveryLEDAdoptionRetainsSeparatelyClearedFaults(t *testing.T) {
	for _, adoptFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(adoptFirst), func(t *testing.T) {
			f := newLEDEvidenceFixture(t, true, false)
			ctx := context.Background()
			evidence := cleanupProposal(t, f)
			p, err := f.r.preserveLED(ctx, migrationSocket, f.inventory.InventoryID, f.inventory.InventoryHash, f.api)
			must(t, err)
			request := RecoveryLEDAdoptionRequest{f.inventory.InventoryID, f.inventory.InventoryHash, p.RecordHash}
			if adoptFirst {
				_, err = f.r.adoptLED(ctx, migrationSocket, request, f.api)
				must(t, err)
			}
			cleared, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, f.api, acceptLEDDomain)
			must(t, err)
			if cleared.Outcome != "acknowledged" {
				t.Fatal(cleared)
			}
			fresh, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true)
			must(t, err)
			if !adoptFirst {
				request.InventoryID = fresh.InventoryID
				request.InventoryHash = fresh.InventoryHash
			}
			_, err = f.r.adoptLED(ctx, migrationSocket, request, f.api)
			must(t, err)
			var count, obsolete int
			must(t, f.r.db.QueryRow("SELECT count(*),sum(obsolete) FROM led_states").Scan(&count, &obsolete))
			if count != p.Faults || obsolete != 0 {
				t.Fatal("lost ownership evidence", count, obsolete)
			}
			if _, ok := f.api.states[f.old]; ok {
				t.Fatal("adoption recreated cleared state")
			}
			review, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
			must(t, err)
			if !review.SourcesMatch || review.Adoption == nil || !review.Adoption.SourcesMatch {
				t.Fatal("adoption invalidated evidence", review)
			}
		})
	}
}

func TestRecoveryLEDAdoptionDetectsPreservedOriginalDrift(t *testing.T) {
	f, request := adoptionFixture(t)
	ctx := context.Background()
	_, err := f.r.adoptLED(ctx, migrationSocket, request, f.api)
	must(t, err)
	original, err := snapshotDB(f.r.dir, "state.db", false)
	must(t, err)
	_, err = original.Exec("UPDATE led_states SET obsolete=0 WHERE id=?", f.shared)
	must(t, err)
	must(t, original.Close())
	review, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if review.SourcesMatch || review.Adoption == nil || review.Adoption.SourcesMatch {
		t.Fatal("original drift hidden", review)
	}
	if _, err = f.r.adoptLED(ctx, migrationSocket, request, nil); err == nil {
		t.Fatal("changed original source accepted on retry")
	}
}
func TestRecoveryLEDAdoptionMaximumUnion(t *testing.T) {
	f := newLEDRecoveryFixture(t)
	ctx := context.Background()
	original, err := snapshotDB(f.r.dir, "state.db", false)
	must(t, err)
	for _, db := range []*sql.DB{f.r.db, original} {
		tx, err := db.BeginTx(ctx, nil)
		must(t, err)
		_, err = tx.Exec("DELETE FROM led_states")
		must(t, err)
		for i := 0; i < maxRecoveryLEDFaults; i++ {
			nonce, err := safefs.NewID("n")
			must(t, err)
			identity := ledStateIdentity{f.r.journal.Before.Worker, f.r.journal.Before.Recovery, "selected", nonce}
			raw, err := json.Marshal(identity)
			must(t, err)
			_, err = tx.Exec("INSERT INTO led_states(id,binding,condition,identity,obsolete) VALUES(?,'selected','critical',?,1)", "deploy.f."+protocol.Hash(identity)[:48], string(raw))
			must(t, err)
		}
		must(t, tx.Commit())
	}
	must(t, original.Close())
	api := &fakeLED{states: map[string]led.State{}}
	v, err := f.r.reviewLEDInventory(ctx, migrationSocket, api, false)
	must(t, err)
	p, err := f.r.preserveLED(ctx, migrationSocket, v.InventoryID, v.InventoryHash, api)
	must(t, err)
	request := RecoveryLEDAdoptionRequest{v.InventoryID, v.InventoryHash, p.RecordHash}
	out, err := f.r.adoptLED(ctx, migrationSocket, request, api)
	must(t, err)
	if out.Faults != 2*maxRecoveryLEDFaults {
		t.Fatal("bounded reader truncated union", out.Faults)
	}
	var count, obsolete int
	must(t, f.r.db.QueryRow("SELECT count(*),sum(obsolete) FROM led_states").Scan(&count, &obsolete))
	if count != out.Faults || obsolete != 0 {
		t.Fatal(count, obsolete)
	}
	runtime := &Store{dir: f.r.dir, db: f.r.db, identity: f.r.journal.After, config: f.config, now: func() time.Time { return f.now }}
	must(t, runtime.transaction(ctx, func(tx *sql.Tx) error {
		faults, err := runtime.recoveredLEDFaults(ctx, tx)
		if err != nil {
			return err
		}
		if len(faults) != count {
			t.Fatal("runtime truncated inherited ownership", len(faults), count)
		}
		return nil
	}))
	must(t, f.r.Close())
	reopened, err := openRecoveryReconciler(ctx, f.path, f.config, nil)
	must(t, err)
	defer reopened.Close()
	replay, err := reopened.adoptLED(ctx, migrationSocket, request, nil)
	must(t, err)
	if replay != out {
		t.Fatal("large union lost on retry", replay)
	}
	review, err := reopened.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if !review.SourcesMatch || review.Adoption == nil || review.Adoption.Faults != count {
		t.Fatal("large adopted union cannot be reviewed", review.Adoption)
	}
	epoch, err := reopened.handoffLED(ctx, migrationSocket, out.RecordHash)
	must(t, err)
	if epoch.Faults != count || epoch.State != "committed" {
		t.Fatal("epoch handoff truncated maximum union", epoch)
	}
	epochReplay, err := reopened.handoffLED(ctx, migrationSocket, out.RecordHash)
	must(t, err)
	if epochReplay != epoch {
		t.Fatal("maximum union handoff not replayable")
	}
}
