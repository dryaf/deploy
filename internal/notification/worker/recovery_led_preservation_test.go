package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func preservationFixture(t *testing.T) (*ledRecoveryFixture, RecoveryLEDReview) {
	t.Helper()
	f := newLEDRecoveryFixture(t)
	v, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, false)
	must(t, err)
	return f, v
}
func TestRecoveryLEDPreservationRetainsUnionWithoutAuthority(t *testing.T) {
	f, v := preservationFixture(t)
	ctx := context.Background()
	before := protocol.Hash(f.api.states)
	f.api.read = func() {
		if !f.r.mu.TryLock() {
			t.Fatal("registry I/O held database mutex")
		}
		f.r.mu.Unlock()
	}
	out, err := f.r.preserveLED(ctx, migrationSocket, v.InventoryID, v.InventoryHash, f.api)
	must(t, err)
	if out.State != "committed" || out.Faults != 3 || out.PostSnapshotFaults != 1 || out.SnapshotOnlyFaults != 1 || out.UnownedStates != 2 || !out.SourcesMatch || !out.Quarantined || out.ActivationAllowed || out.ClearAllowed {
		t.Fatal(out)
	}
	record, phase, err := f.r.readLEDPreservation(ctx, f.r.db)
	must(t, err)
	if phase != "committed" || protocol.Hash(*record) != out.RecordHash {
		t.Fatal(phase, record)
	}
	for _, fault := range record.Faults {
		if !strings.Contains(fault.Identity, f.r.journal.Before.Recovery) || strings.Contains(fault.Identity, f.r.journal.After.Recovery) {
			t.Fatal("old epoch relabeled", fault)
		}
		if fault.ID == f.shared && (!fault.Snapshot || !fault.Latest || fault.SnapshotCleanupPending || !fault.LatestCleanupPending) {
			t.Fatal("cleanup provenance lost", fault)
		}
	}
	raw, err := os.ReadFile(filepath.Join(f.r.stage.File().Name(), recoveryLEDPreservationFile))
	must(t, err)
	if strings.Contains(string(raw), "private application") || strings.Contains(string(raw), "foreign private") {
		t.Fatal("free-form daemon message persisted")
	}
	if before != protocol.Hash(f.api.states) || len(f.api.cleared) != 0 {
		t.Fatal("preservation mutated registry")
	}
	var count int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM led_states").Scan(&count))
	if count != 2 {
		t.Fatal("snapshot ledger changed", count)
	}
	calls := f.api.calls
	f.now = f.now.Add(time.Hour)
	f.uptime += 3600
	f.boot = "rebooted"
	retry, err := f.r.preserveLED(ctx, migrationSocket, v.InventoryID, v.InventoryHash, nil)
	must(t, err)
	if retry != out || f.api.calls != calls {
		t.Fatal("retry renewed ownership observation", retry, out)
	}
	review, err := f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if review.Preservation == nil || *review.Preservation != out || review.ObservationCurrent {
		t.Fatal(review.Preservation)
	}
	must(t, f.r.Close())
	if _, err = Open(f.path, f.config, false); err == nil {
		t.Fatal("preservation activated quarantined worker")
	}
}

func TestRecoveryLEDPreservationRejectsStaleOrChangedEvidence(t *testing.T) {
	for _, change := range []string{"expiry", "monotonic-expiry", "reboot", "source", "registry", "disappeared", "wrong-hash", "socket", "cancelled", "source-during-rpc", "elapsed-during-rpc", "no-observer"} {
		t.Run(change, func(t *testing.T) {
			f, v := preservationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			socket, hash := migrationSocket, v.InventoryHash
			var api LEDRecoveryObserver = f.api
			switch change {
			case "expiry":
				f.now = f.now.Add(31 * time.Second)
			case "monotonic-expiry":
				f.uptime += 31
			case "reboot":
				f.boot = "other"
			case "source":
				_, err := f.r.db.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", f.old)
				must(t, err)
			case "registry":
				st := f.api.states[f.new]
				st.Message = "changed incarnation"
				f.api.states[f.new] = st
			case "disappeared":
				delete(f.api.states, f.new)
			case "wrong-hash":
				hash = protocol.Hash("wrong")
			case "socket":
				socket = "/different/led.sock"
			case "cancelled":
				cancel()
			case "source-during-rpc":
				f.api.read = func() { _, err := f.r.db.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", f.old); must(t, err) }
			case "elapsed-during-rpc":
				f.api.read = func() { f.now = f.now.Add(31 * time.Second); f.uptime += 31 }
			case "no-observer":
				api = nil
			}
			if _, err := f.r.preserveLED(ctx, socket, v.InventoryID, hash, api); err == nil {
				t.Fatal("changed evidence accepted")
			}
			if _, err := os.Stat(filepath.Join(f.r.stage.File().Name(), recoveryLEDPreservationFile)); !os.IsNotExist(err) {
				t.Fatal("failed preflight published intent", err)
			}
			if len(f.api.cleared) != 0 {
				t.Fatal("failed preservation cleared states")
			}
		})
	}
}

func TestRecoveryLEDPreservationKeepsUnknownStatesUnowned(t *testing.T) {
	f, v := preservationFixture(t)
	f.api.extra = append(f.api.extra, led.Entry{State: led.State{ID: "foreign-added-later", LED: "green", Pattern: "on", Priority: 95, Persistent: true}, OwnerUID: os.Getuid() + 10})
	out, err := f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, f.api)
	must(t, err)
	if out.Faults != 3 || out.UnownedStates != 2 {
		t.Fatal("new unowned registry entry became ownership", out)
	}
	// Refresh can observe it, but cannot replace the already frozen preservation.
	refreshed, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, true)
	must(t, err)
	if refreshed.Preservation == nil || refreshed.Preservation.RecordHash != out.RecordHash {
		t.Fatal(refreshed.Preservation)
	}
	if _, err = f.r.preserveLED(context.Background(), migrationSocket, refreshed.InventoryID, refreshed.InventoryHash, f.api); err == nil {
		t.Fatal("preservation was silently replaced")
	}
}

func TestRecoveryLEDPreservationInterruptedCommit(t *testing.T) {
	for _, point := range []string{"led_preservation_intent", "led_preservation_transaction", "led_preservation_commit"} {
		t.Run(point, func(t *testing.T) {
			f, v := preservationFixture(t)
			f.r.fault = func(p string) error {
				if p == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, f.api); err == nil {
				t.Fatal("missing fault")
			}
			record, _, err := f.r.readLEDPreservation(context.Background(), f.r.db)
			must(t, err)
			if record == nil {
				t.Fatal("intent missing")
			}
			hash := protocol.Hash(*record)
			f.r.fault = nil
			f.now = f.now.Add(time.Hour)
			f.uptime += 3600
			out, err := f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, nil)
			must(t, err)
			if out.State != "committed" || out.RecordHash != hash || !out.PreparedAt.Equal(record.PreparedAt) {
				t.Fatal(out)
			}
		})
	}
}

func TestRecoveryLEDPreservationCorruptionBlocksReopen(t *testing.T) {
	for _, damage := range []string{"intent-missing", "unexpected-row", "sql-hash", "sql-body", "schema", "inventory", "invented-fault", "forged-both", "source-changed"} {
		t.Run(damage, func(t *testing.T) {
			f, v := preservationFixture(t)
			_, err := f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, f.api)
			must(t, err)
			switch damage {
			case "intent-missing":
				must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), recoveryLEDPreservationFile)))
			case "unexpected-row":
				_, err = f.r.db.Exec("PRAGMA ignore_check_constraints=ON; INSERT INTO reconciliation_led_preservation SELECT 2,hash,body FROM reconciliation_led_preservation WHERE singleton=1")
				must(t, err)
			case "sql-hash":
				_, err = f.r.db.Exec("UPDATE reconciliation_led_preservation SET hash=?", protocol.Hash("wrong"))
				must(t, err)
			case "sql-body":
				_, err = f.r.db.Exec("UPDATE reconciliation_led_preservation SET body='{}'")
				must(t, err)
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='led-preservation-version'")
				must(t, err)
			case "inventory":
				must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), recoveryLEDInventoryFile(v.InventoryID))))
			case "invented-fault", "forged-both":
				var record recoveryLEDPreservation
				must(t, f.r.stage.ReadJSON(recoveryLEDPreservationFile, &record))
				record.Faults[0].ID = "invented"
				must(t, f.r.stage.WriteJSON(recoveryLEDPreservationFile, record, true))
				if damage == "forged-both" {
					raw, e := json.Marshal(record)
					must(t, e)
					_, e = f.r.db.Exec("UPDATE reconciliation_led_preservation SET hash=?,body=?", protocol.Hash(record), string(raw))
					must(t, e)
				}
			case "source-changed":
				_, err = f.r.db.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", f.old)
				must(t, err)
			}
			if damage == "source-changed" {
				if _, err = f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, nil); err == nil {
					t.Fatal("changed source accepted")
				}
				review, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, nil, false)
				must(t, err)
				if review.Preservation == nil || review.Preservation.SourcesMatch {
					t.Fatal("source drift hidden")
				}
				return
			}
			must(t, f.r.Close())
			r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil)
			if err == nil {
				r.Close()
				t.Fatal("damaged preservation accepted on reopening")
			}
		})
	}
}

func TestRecoveryLEDPreservationValidatesBoundedUnion(t *testing.T) {
	f, _ := preservationFixture(t)
	set, err := f.r.readLEDInventorySet(context.Background(), f.r.db)
	must(t, err)
	v := *set.Head
	for _, change := range []string{"mapping", "uid", "legacy", "condition", "future-time"} {
		t.Run(change, func(t *testing.T) {
			candidate := v
			candidate.Sources.Snapshot.Faults = append([]recoveryLEDFault{}, v.Sources.Snapshot.Faults...)
			at := f.now
			switch change {
			case "mapping":
				candidate.Sources.Latest.Layout.Revision = "invalid"
			case "uid":
				candidate.Sources.Latest.Layout.UID++
			case "legacy":
				candidate.Sources.Snapshot.Faults[0].Identity = "legacy"
			case "condition":
				for i := range candidate.Sources.Snapshot.Faults {
					if candidate.Sources.Snapshot.Faults[i].ID == f.shared {
						candidate.Sources.Snapshot.Faults[i].Condition = "warning"
					}
				}
			case "future-time":
				at = v.ValidUntil
			}
			if _, err := f.r.makeLEDPreservation(candidate, at); err == nil {
				t.Fatal("invalid union accepted")
			}
		})
	}
	// Pending database publication retains the worker's process-wide lease.
	if _, err := PreserveRecoveryLED(context.Background(), f.path, f.config, migrationSocket, v.ID, protocol.Hash(v), nil); err == nil {
		t.Fatal("exclusive worker lease bypassed")
	}
}

func TestRecoveryLEDPreservationRechecksBeforeIndependentIntent(t *testing.T) {
	f, v := preservationFixture(t)
	commits := 0
	f.r.fault = func(point string) error {
		if point == "reconciliation_commit" {
			commits++
			if commits == 2 {
				f.now = f.now.Add(31 * time.Second)
				f.uptime += 31
			}
		}
		return nil
	}
	if _, err := f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, f.api); err == nil {
		t.Fatal("schema publication extended review lifetime")
	}
	if _, err := os.Stat(filepath.Join(f.r.stage.File().Name(), recoveryLEDPreservationFile)); !os.IsNotExist(err) {
		t.Fatal("expired review wrote intent", err)
	}
}

func TestRecoveryLEDPreservationCannotDischargeFaults(t *testing.T) {
	f, v := preservationFixture(t)
	out, err := f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, f.api)
	must(t, err)
	before := protocol.Hash(f.api.states)
	request := protocol.RecoveryLEDClearRequest{InventoryID: v.InventoryID, Binding: "selected", FaultID: f.old, EvidenceHash: out.RecordHash}
	if _, err = f.r.clearLEDFault(context.Background(), migrationSocket, request.InventoryID, request.Binding, request.FaultID, request.EvidenceHash, f.api, acceptLEDDomain); err == nil {
		t.Fatal("preservation became domain resolution authority")
	}
	if protocol.Hash(f.api.states) != before || len(f.api.cleared) != 0 {
		t.Fatal("fault changed without domain evidence")
	}
}

func TestRecoveryLEDPreservationHonorsStorageBudget(t *testing.T) {
	f, v := preservationFixture(t)
	f.api.read = func() {
		_, err := f.r.db.Exec("CREATE TABLE test_preservation_capacity(data BLOB); INSERT INTO test_preservation_capacity VALUES(zeroblob(?))", f.config.MaxDatabaseBytes)
		must(t, err)
	}
	if _, err := f.r.preserveLED(context.Background(), migrationSocket, v.InventoryID, v.InventoryHash, f.api); err == nil {
		t.Fatal("exhausted database budget accepted")
	}
	if _, err := os.Stat(filepath.Join(f.r.stage.File().Name(), recoveryLEDPreservationFile)); !os.IsNotExist(err) {
		t.Fatal("storage failure wrote ownership intent", err)
	}
}
