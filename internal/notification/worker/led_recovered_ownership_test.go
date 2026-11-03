package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Borrow the fenced candidate to exercise runtime primitives. No fixture removes
// quarantine or installs it as the live database. The recovery session owns it.
func inheritedLEDFixture(t *testing.T, extraBindings ...LEDBinding) (*ledRecoveryFixture, *Store) {
	t.Helper()
	f, request := adoptionFixture(t, extraBindings...)
	ctx := context.Background()
	_, err := f.r.adoptLED(ctx, migrationSocket, request, f.api)
	must(t, err)
	_, err = f.r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	s := &Store{dir: f.r.dir, db: f.r.db, identity: f.r.journal.After, config: f.config, now: func() time.Time { return f.now }, session: "inherited-runtime-test"}
	return f, s
}

func TestInheritedLEDOwnershipRetainsUnionAndBlocksMutation(t *testing.T) {
	f, s := inheritedLEDFixture(t)
	ctx := context.Background()
	before := protocol.Hash(f.api.states)
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		faults, err := s.recoveredLEDFaults(ctx, tx)
		if err != nil {
			return err
		}
		if len(faults) != 3 {
			t.Fatal("incomplete union", faults)
		}
		for _, id := range []string{f.old, f.new, f.shared} {
			v := faults[id]
			if !errors.Is(s.checkLEDState(tx, id, v.Binding, v.Identity), errLEDRecoveryPending) {
				t.Fatal("inherited ownership became mutation authority", v)
			}
		}
		return nil
	}))
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "critical" || h.Bindings["selected"].Condition != "critical" || !h.ValidUntil.IsZero() || h.InheritedLEDFaults[workerLEDBinding] != 1 || h.InheritedLEDFaults["selected"] != 2 {
		t.Fatal("inherited faults disappeared from health", h)
	}
	// Fresh ordinary source data cannot discharge either old fault incarnation.
	v := snapshot(f.now, 100, 100)
	must(t, s.Replace(ctx, v))
	h, err = s.Health(ctx)
	must(t, err)
	if h.Bindings["selected"].Condition != "critical" {
		t.Fatal("ordinary ok snapshot discharged inherited faults", h)
	}
	r := LEDReconciler{Store: s, API: f.api, Budget: time.Second, binding: &s.config.LEDBindings[0]}
	for _, kind := range []string{"set", "clear"} {
		_, err = s.db.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", f.old)
		must(t, err)
		state := f.api.states[f.old]
		var arg *led.State
		if kind == "set" {
			arg = &state
		}
		if _, _, err = r.mutateLED(ctx, kind, f.old, arg, &h); !errors.Is(err, errLEDRecoveryPending) {
			t.Fatal("inherited mutation not fenced", kind, err)
		}
	}
	if protocol.Hash(f.api.states) != before || len(f.api.cleared) != 0 {
		t.Fatal("ownership inspection mutated registry")
	}
	if live, err := Open(f.path, f.config, false); err == nil {
		live.Close()
		t.Fatal("runtime primitive bypassed quarantine")
	}
}

func TestInheritedLEDUsesNewIncarnationsAndSkipsOldCleanup(t *testing.T) {
	f, s := inheritedLEDFixture(t)
	ctx := context.Background()
	var fresh string
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		fresh, err = s.currentLEDState(tx, "selected", "critical")
		if err != nil {
			return err
		}
		if fresh == f.old || fresh == f.new || fresh == f.shared {
			t.Fatal("reused inherited incarnation")
		}
		again, err := s.currentLEDState(tx, "selected", "critical")
		if err != nil || again != fresh {
			t.Fatal("did not reuse current-epoch incarnation", again, err)
		}
		ids, err := s.obsoleteLEDStates(tx, "selected", "", true)
		if err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != fresh {
			t.Fatal("old faults entered ordinary cleanup", ids)
		}
		var marked int
		if err = tx.QueryRow("SELECT sum(obsolete) FROM led_states WHERE id IN (?,?)", f.old, f.new).Scan(&marked); err != nil {
			return err
		}
		if marked != 0 {
			t.Fatal("ordinary cleanup marked inherited faults")
		}
		// Ownership evidence is independent of the mutable operation projection.
		proof, err := f.r.readLEDAdoptionAuthority(ctx, tx)
		if err != nil || proof == nil || proof.Phase != "committed" {
			t.Fatal(proof, err)
		}
		// The offline quarantine checker retains its stronger whole-union check.
		if _, err = f.r.readLEDAdoption(ctx, tx); err == nil {
			t.Fatal("runtime validation weakened quarantine projection validation")
		}
		return nil
	}))
}

func TestInheritedLEDRefusesMissingOrChangedAuthority(t *testing.T) {
	for _, change := range []string{"owner-epoch", "canonical-identity", "database-identity", "config", "mapping", "journal", "adoption-file", "preservation-file", "inventory-file", "ack", "schema", "row-missing", "row-condition", "row-binding", "row-identity", "unproven-old"} {
		t.Run(change, func(t *testing.T) {
			f, s := inheritedLEDFixture(t)
			ctx := context.Background()
			switch change {
			case "owner-epoch":
				s.identity.Recovery = "n-11111111111111111111111111111111"
			case "canonical-identity":
				must(t, s.dir.WriteJSON("identity.json", f.r.journal.Before, true))
			case "database-identity":
				raw, err := json.Marshal(f.r.journal.Before)
				must(t, err)
				_, err = s.db.Exec("UPDATE metadata SET value=? WHERE key='identity'", string(raw))
				must(t, err)
			case "config":
				s.config.Producers[0].MaxSilenceSeconds++
			case "mapping":
				_, err := s.db.Exec("UPDATE metadata SET value='{}' WHERE key='led-layout'")
				must(t, err)
			case "journal":
				must(t, os.Remove(filepath.Join(f.path, "recovery.json")))
			case "adoption-file", "preservation-file", "inventory-file":
				name := recoveryLEDAdoptionFile
				if change == "preservation-file" {
					name = recoveryLEDPreservationFile
				}
				if change == "inventory-file" {
					var record recoveryLEDAdoption
					must(t, f.r.stage.ReadJSON(recoveryLEDAdoptionFile, &record))
					name = recoveryLEDInventoryFile(record.InventoryID)
				}
				must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), name)))
			case "ack":
				_, err := s.db.Exec("DELETE FROM reconciliation_led_adoption")
				must(t, err)
			case "schema":
				_, err := s.db.Exec("DROP TABLE reconciliation_led_adoption; DELETE FROM metadata WHERE key='led-adoption-version'")
				must(t, err)
			case "row-missing":
				_, err := s.db.Exec("DELETE FROM led_states WHERE id=?", f.new)
				must(t, err)
			case "row-condition", "row-binding", "row-identity":
				query := map[string]string{"row-condition": "UPDATE led_states SET condition='unknown' WHERE id=?", "row-binding": "UPDATE led_states SET binding='$worker' WHERE id=?", "row-identity": "UPDATE led_states SET identity='legacy' WHERE id=?"}[change]
				_, err := s.db.Exec(query, f.new)
				must(t, err)
			case "unproven-old":
				_, err := s.db.Exec("INSERT INTO led_states SELECT 'deploy.f.unproven',condition,obsolete,binding,identity FROM led_states WHERE id=?", f.old)
				must(t, err)
			}
			if err := s.transaction(ctx, func(tx *sql.Tx) error { _, e := s.recoveredLEDFaults(ctx, tx); return e }); err == nil {
				t.Fatal("accepted damaged inherited ownership", change)
			}
			before := protocol.Hash(f.api.states)
			if err := (&LEDReconciler{Store: s, API: f.api, Budget: time.Second}).Step(ctx); err == nil {
				t.Fatal("damaged inherited ownership reached runtime", change)
			}
			if protocol.Hash(f.api.states) != before {
				t.Fatal("damaged ownership changed daemon", change)
			}
			if len(f.api.cleared) != 0 {
				t.Fatal("refusal changed daemon")
			}
		})
	}
}

func TestInheritedLEDDoesNotRebaseOldOperationHistory(t *testing.T) {
	f, s := inheritedLEDFixture(t)
	ctx := context.Background()
	var raw string
	must(t, s.db.QueryRow("SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw))
	var layout ledLayout
	must(t, protocol.Decode([]byte(raw), &layout, protocol.MaxRequest))
	state := ledBaseline(s.identity.Worker, ledLayoutOutputs(layout)[0])
	entry := ledOperationEntry{Intent: ledOperation{Version: 1, Identity: f.r.journal.Before, Binding: workerLEDBinding, Revision: 1, LayoutHash: protocol.Hash(layout), Kind: "set", ID: state.ID, Condition: "baseline", State: &state, IssuedAt: f.now}, Outcome: "pending"}
	must(t, validateLEDOperation(entry, layout, f.r.journal.Before))
	body, err := json.Marshal(entry)
	must(t, err)
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO led_operations VALUES(?,?,?,?)", workerLEDBinding, 1, string(body), protocol.Hash(entry)); err != nil {
			return err
		}
		return writeLEDHead(ctx, tx, workerLEDBinding, ledOperationHead{Issued: 1})
	}))
	before := protocol.Hash(f.api.states)
	r := LEDReconciler{Store: s, API: f.api, Budget: time.Second}
	if err = r.Step(ctx); err == nil {
		t.Fatal("runtime mixed recovery epochs in operation history")
	}
	if _, err = r.beginLEDOperation(ctx, "set", state.ID, &state, nil); err == nil {
		t.Fatal("direct operation bypassed history fence")
	}
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM led_operations").Scan(&count))
	if count != 1 || protocol.Hash(f.api.states) != before {
		t.Fatal("failed handoff changed operation history or daemon")
	}
	must(t, checkLEDOperations(ctx, s.db, f.r.journal.Before))
}

func TestInheritedLEDRuntimePassPreservesOldIDsAndRevokesHealthy(t *testing.T) {
	f, s := inheritedLEDFixture(t)
	ctx := context.Background()
	old := map[string]led.State{}
	for _, id := range []string{f.old, f.new, f.shared, "deploy.f.unrecognized"} {
		old[id] = f.api.states[id]
	}
	// Ownership evidence is historical. Expiring the registry observation must
	// not reinterpret it as fresh clearance or discard ownership on a reboot.
	f.now = f.now.Add(time.Hour)
	f.boot, f.uptime = "rebooted", 1
	r := LEDReconciler{Store: s, API: f.api, Budget: time.Second}
	for range 2 {
		must(t, r.Step(ctx))
	}
	for id, want := range old {
		if protocol.Hash(f.api.states[id]) != protocol.Hash(want) {
			t.Fatal("runtime changed inherited or unowned state", id)
		}
	}
	for _, binding := range []string{workerLEDBinding, "selected"} {
		if _, exists := f.api.states[ledBindingNamespace(s.identity.Worker, binding)+".healthy"]; exists {
			t.Fatal("inherited ownership permitted a healthy lease")
		}
	}
	for _, id := range f.api.cleared {
		if _, inherited := old[id]; inherited {
			t.Fatal("runtime cleared old state", id)
		}
	}
	var pending int
	must(t, s.db.QueryRow("SELECT sum(obsolete) FROM led_states WHERE id IN (?,?,?)", f.old, f.new, f.shared).Scan(&pending))
	if pending != 0 {
		t.Fatal("runtime manufactured old cleanup authority")
	}
}

func TestInheritedLEDLegacyInventoryIsNotClearAuthority(t *testing.T) {
	f, s := inheritedLEDFixture(t)
	ctx := context.Background()
	var raw string
	must(t, s.db.QueryRow("SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw))
	var layout ledLayout
	must(t, protocol.Decode([]byte(raw), &layout, protocol.MaxRequest))
	layout.Legacy = []string{f.shared}
	body, err := json.Marshal(layout)
	must(t, err)
	_, err = s.db.Exec("UPDATE metadata SET value=? WHERE key='led-layout'", string(body))
	must(t, err)
	_, err = s.db.Exec("UPDATE led_states SET identity='legacy' WHERE id=?", f.shared)
	must(t, err)
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.checkLEDState(tx, f.shared, workerLEDBinding, "legacy"); err == nil {
			t.Fatal("legacy layout bypassed inherited provenance")
		}
		return nil
	}))
	if _, err = s.Health(ctx); err == nil {
		t.Fatal("legacy relabeling enabled healthy state")
	}
}

func TestInheritedLEDBindingIsolation(t *testing.T) {
	_, s := inheritedLEDFixture(t, binding("unaffected", "other-subject"))
	h, err := s.Health(context.Background())
	must(t, err)
	if h.Condition != "critical" || h.Bindings["selected"].Condition != "critical" {
		t.Fatal("inherited adverse indication lost", h)
	}
	// This producer is still fenced, so its other binding remains unknown for
	// liveness reasons. It must not inherit another binding's critical fault.
	if h.Bindings["unaffected"].Condition != "unknown" || h.InheritedLEDFaults["unaffected"] != 0 {
		t.Fatal("inherited fault crossed binding selection", h)
	}
}
