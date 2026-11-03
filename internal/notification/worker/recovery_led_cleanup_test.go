package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func cleanupProposal(t *testing.T, f *ledEvidenceFixture) string {
	t.Helper()
	v := submitLEDEvidence(t, f)
	if !v.Resolutions["selected"].DomainResolved {
		t.Fatal(v.Resolutions)
	}
	return v.Resolutions["selected"].EvidenceHash
}

func acceptLEDDomain(context.Context, RecoveryLEDEvidence) error { return nil }

func TestRecoveryLEDCleanupExactAndHistoricalReplay(t *testing.T) {
	f := newLEDEvidenceFixture(t, true, false)
	evidence := cleanupProposal(t, f)
	ctx := context.Background()
	// Exact nonce ownership is independent of which fault currently wins.
	f.api.states["unrelated"] = led.State{ID: "unrelated", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}
	calls := 0
	guard := func(ctx context.Context, proof RecoveryLEDEvidence) error {
		calls++
		if proof.Challenge != f.challenge || protocol.Hash(proof.Report) != protocol.Hash(f.report) {
			t.Fatal("wrong producer proof")
		}
		return f.r.transaction(ctx, func(*sql.Tx) error { return nil })
	}
	f.api.onClear = func(string) { must(t, f.r.transaction(ctx, func(*sql.Tx) error { return nil })) }
	v, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, f.api, guard)
	must(t, err)
	if v.Outcome != "acknowledged" || calls != 2 || len(f.api.cleared) != 1 || f.api.cleared[0] != f.old {
		t.Fatal(v, calls, f.api.cleared)
	}
	if _, ok := f.api.states[f.new]; !ok {
		t.Fatal("newer nonce removed")
	}
	if _, ok := f.api.states["unrelated"]; !ok {
		t.Fatal("unrelated state removed")
	}
	// Historical outcome remains readable after expiry/reboot, with no API calls.
	f.now = f.now.Add(time.Hour)
	f.boot = "boot-two"
	w, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, nil, nil)
	must(t, err)
	if protocol.Hash(v) != protocol.Hash(w) {
		t.Fatal("retry renewed outcome")
	}
	if _, err = f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, protocol.Hash("wrong"), nil, nil); err == nil {
		t.Fatal("conflicting retry accepted")
	}
	if _, err = f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.new, evidence, f.api, guard); err == nil {
		t.Fatal("expired evidence cleared another nonce")
	}
	if s, err := Open(f.path, f.config, false); err == nil {
		s.Close()
		t.Fatal("cleanup activated runtime")
	}
	var n int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM led_states WHERE id=?", f.old).Scan(&n))
	if n != 1 {
		t.Fatal("cleanup rewrote source ledger")
	}
}

func TestRecoveryLEDCleanupRejectsUnsafeInputs(t *testing.T) {
	for _, scenario := range []string{"guard_missing", "guard_failure", "registry_change", "appeared", "expired", "monotonic_expired", "worker", "unknown", "wrong_socket", "wrong_hash", "source_changed", "guard_drift", "registry_timeout"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLEDEvidenceFixture(t, false, false)
			evidence := cleanupProposal(t, f)
			ctx := context.Background()
			id, b, socket := f.old, "selected", migrationSocket
			guard := RecoveryLEDDomainGuard(acceptLEDDomain)
			switch scenario {
			case "guard_missing":
				guard = nil
			case "guard_failure":
				guard = func(context.Context, RecoveryLEDEvidence) error { return errors.New("source unavailable") }
			case "registry_change":
				state := f.api.states[id]
				state.Message = "changed"
				f.api.states[id] = state
			case "appeared":
				delete(f.api.states, id)
				v, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true)
				must(t, err)
				f.inventory = v
				f.challenge, err = f.r.ChallengeLED(ctx, v.InventoryID, producer)
				must(t, err)
				f.report.Snapshot.Sequence++
				evidence = cleanupProposal(t, f)
				f.api.states[id] = led.State{ID: id, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}
			case "expired":
				f.now = f.now.Add(28 * time.Second)
			case "monotonic_expired":
				f.uptime += 28
			case "worker":
				b = workerLEDBinding
			case "unknown":
				id = "foreign"
			case "wrong_socket":
				socket = "/tmp/other-led.sock"
			case "wrong_hash":
				evidence = protocol.Hash("other")
			case "source_changed":
				_, err := f.r.db.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", id)
				must(t, err)
			case "guard_drift":
				calls := 0
				guard = func(context.Context, RecoveryLEDEvidence) error {
					calls++
					if calls == 2 {
						return errors.New("domain changed")
					}
					return nil
				}
			case "registry_timeout":
				f.api.statusErr = context.DeadlineExceeded
			}
			v, err := f.r.clearLEDFault(ctx, socket, f.inventory.InventoryID, b, id, evidence, f.api, guard)
			if err == nil || len(f.api.cleared) != 0 {
				t.Fatal("unsafe clear", scenario, v, err)
			}
			if scenario == "guard_drift" {
				if v.Outcome != "not_sent" {
					t.Fatal(v)
				}
				retry, err := f.r.clearLEDFault(ctx, socket, f.inventory.InventoryID, b, id, evidence, f.api, acceptLEDDomain)
				must(t, err)
				if retry.Outcome != "not_sent" || len(f.api.cleared) != 0 {
					t.Fatal("replayed not-sent operation")
				}
			}
		})
	}
}

type failingRecoveryClear struct {
	LEDRecoveryCleaner
	cancel context.CancelFunc
}

func (a failingRecoveryClear) Clear(ctx context.Context, id string) error {
	if err := a.LEDRecoveryCleaner.Clear(ctx, id); err != nil {
		return err
	}
	if a.cancel != nil {
		a.cancel()
	}
	return context.DeadlineExceeded
}

func TestRecoveryLEDCleanupUncertainAndAbsent(t *testing.T) {
	for _, scenario := range []string{"absent", "lost_reply", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLEDEvidenceFixture(t, true, false)
			evidence := cleanupProposal(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var api LEDRecoveryCleaner = f.api
			if scenario == "absent" {
				delete(f.api.states, f.old)
			} else {
				a := failingRecoveryClear{LEDRecoveryCleaner: f.api}
				if scenario == "cancelled" {
					a.cancel = cancel
				}
				api = a
			}
			v, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, api, acceptLEDDomain)
			want := "uncertain"
			if scenario == "absent" {
				want = "observed_absent"
				must(t, err)
			} else if err == nil {
				t.Fatal("lost reply hidden")
			}
			if v.Outcome != want || v.FinishedAt.IsZero() {
				t.Fatal(v)
			}
			n := len(f.api.cleared)
			retry, err := f.r.clearLEDFault(context.Background(), migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, f.api, acceptLEDDomain)
			must(t, err)
			if protocol.Hash(retry) != protocol.Hash(v) || len(f.api.cleared) != n {
				t.Fatal("uncertain clear replayed")
			}
		})
	}
}

func TestRecoveryLEDCleanupJournalDamageFailsClosed(t *testing.T) {
	for _, scenario := range []string{"head", "intent", "result", "schema", "row", "digest"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLEDEvidenceFixture(t, false, false)
			evidence := cleanupProposal(t, f)
			_, err := f.r.clearLEDFault(context.Background(), migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, f.api, acceptLEDDomain)
			must(t, err)
			records, err := f.r.readLEDClears(context.Background(), f.r.db)
			must(t, err)
			v := records[0].Intent
			switch scenario {
			case "head", "intent", "result":
				name := recoveryLEDClearHead
				if scenario != "head" {
					name = ledClearFile(v, scenario)
				}
				must(t, os.Remove(f.r.stage.File().Name()+"/"+name))
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='led-clear-version'")
				must(t, err)
			case "row":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_led_clears")
				must(t, err)
			case "digest":
				_, err = f.r.db.Exec("UPDATE reconciliation_led_clears SET hash='broken'")
				must(t, err)
			}
			if _, err = f.r.readLEDClears(context.Background(), f.r.db); err == nil {
				t.Fatal("damaged history accepted")
			}
			must(t, f.r.Close())
			if r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil); err == nil {
				r.Close()
				t.Fatal("damaged recovery opened")
			}
		})
	}
}

func TestRecoveryLEDCleanupRechecksAfterIntent(t *testing.T) {
	for _, change := range []string{"registry", "source", "expiry", "cancellation"} {
		t.Run(change, func(t *testing.T) {
			f := newLEDEvidenceFixture(t, false, false)
			evidence := cleanupProposal(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.r.fault = func(stage string) error {
				if stage != "led_clear_committed" {
					return nil
				}
				switch change {
				case "registry":
					v := f.api.states[f.old]
					v.Message = "different owner state"
					f.api.states[f.old] = v
				case "source":
					_, err := f.r.db.Exec("UPDATE led_states SET obsolete=1 WHERE id=?", f.old)
					return err
				case "expiry":
					f.uptime += 28
				case "cancellation":
					cancel()
				}
				return nil
			}
			v, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, f.api, acceptLEDDomain)
			if err == nil || v.Outcome != "not_sent" || len(f.api.cleared) != 0 {
				t.Fatal(v, err)
			}
			records, err := f.r.readLEDClears(context.Background(), f.r.db)
			must(t, err)
			if len(records) != 1 || records[0].Result == nil || records[0].Result.Outcome != "not_sent" {
				t.Fatal(records)
			}
		})
	}
}

func TestRecoveryLEDCleanupFreshInventoryRequiredAfterUncertainty(t *testing.T) {
	f := newLEDEvidenceFixture(t, true, false)
	evidence := cleanupProposal(t, f)
	ctx := context.Background()
	f.r.fault = func(stage string) error {
		if stage == "led_clear_committed" {
			return errors.New("interrupted before RPC")
		}
		return nil
	}
	_, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, f.api, acceptLEDDomain)
	if err == nil {
		t.Fatal("missing injected interruption")
	}
	f.r.fault = nil
	old, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, f.api, acceptLEDDomain)
	must(t, err)
	if old.Outcome != "uncertain" || len(f.api.cleared) != 0 {
		t.Fatal(old)
	}
	// Explicit refresh and a new challenged domain evaluation can authorize a
	// distinct attempt. Historical uncertainty itself supplies no authority.
	f.now = f.now.Add(time.Second)
	f.uptime++
	v, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true)
	must(t, err)
	f.inventory = v
	f.challenge, err = f.r.ChallengeLED(ctx, v.InventoryID, producer)
	must(t, err)
	f.report.Snapshot.Sequence++
	f.report.Snapshot.EvaluatedAt = f.now
	for i := range f.report.Transitions {
		f.report.Transitions[i].StatusSequence++
		f.report.Transitions[i].ObservedAt = f.now
	}
	evidence = cleanupProposal(t, f)
	fresh, err := f.r.clearLEDFault(ctx, migrationSocket, f.inventory.InventoryID, "selected", f.old, evidence, f.api, acceptLEDDomain)
	must(t, err)
	if fresh.Outcome != "acknowledged" || fresh.InventoryID == old.InventoryID || len(f.api.cleared) != 1 {
		t.Fatal(fresh)
	}
	v, err = f.r.reviewLEDInventory(ctx, migrationSocket, nil, false)
	must(t, err)
	if v.CleanupAttempts != 2 || v.CleanupUnconfirmed != 1 || len(v.RecentCleanup) != 2 {
		t.Fatal(v)
	}
}

type heldRecoveryClear struct {
	LEDRecoveryCleaner
	entered chan struct{}
	release chan struct{}
}

func (a heldRecoveryClear) Clear(ctx context.Context, id string) error {
	close(a.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.release:
		return a.LEDRecoveryCleaner.Clear(ctx, id)
	}
}

func TestRecoveryLEDCleanupCloseWaitsForAttempt(t *testing.T) {
	f := newLEDEvidenceFixture(t, false, false)
	evidence := cleanupProposal(t, f)
	api := heldRecoveryClear{f.api, make(chan struct{}), make(chan struct{})}
	s := &recoveryLEDCleanupSession{recoveryReconciler: f.r, socket: migrationSocket, api: api}
	req := protocol.RecoveryLEDClearRequest{InventoryID: f.inventory.InventoryID, Binding: "selected", FaultID: f.old, EvidenceHash: evidence}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.ClearLED(ctx, req, acceptLEDDomain); done <- err }()
	select {
	case <-api.entered:
	case <-ctx.Done():
		t.Fatal("clear not dispatched")
	}
	if _, err := s.ClearLED(ctx, req, acceptLEDDomain); err == nil {
		t.Fatal("concurrent cleanup accepted")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatal("close released in-flight ownership", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(api.release)
	must(t, <-done)
	must(t, <-closed)
	if _, err := s.ClearLED(ctx, req, acceptLEDDomain); err == nil {
		t.Fatal("closed session reused")
	}
	must(t, s.Close())
	r, err := openRecoveryReconciler(ctx, f.path, f.config, nil)
	must(t, err)
	defer r.Close()
	records, err := r.readLEDClears(ctx, r.db)
	must(t, err)
	if len(records) != 1 || records[0].Result == nil || records[0].Result.Outcome != "acknowledged" {
		t.Fatal(records)
	}
}
