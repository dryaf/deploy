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

type runtimeLEDRecoveryFixture struct {
	s        *Store
	api      *inventoryLEDObserver
	id       string
	now      time.Time
	path     string
	config   Config
	revision string
	binding  ManagedDeliveryRecovery
	observe  func(error)
}

func newRuntimeLEDRecoveryFixture(t *testing.T) *runtimeLEDRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	f := &runtimeLEDRecoveryFixture{api: &inventoryLEDObserver{fakeLED: &fakeLED{states: map[string]led.State{}}}, now: time.Now().UTC()}
	f.path, f.config, f.revision, f.binding = recoveredDeliveryFixtureConfigured(t, false, false, func(s *Store) {
		must(t, s.transaction(ctx, func(tx *sql.Tx) error {
			var e error
			f.id, e = s.currentLEDState(tx, workerLEDBinding, "critical")
			return e
		}))
		f.api.states[f.id] = led.State{ID: f.id, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true, Message: "Notification state: critical"}
	}, func(path string, c Config) {
		r, e := openRecoveryReconciler(ctx, path, c, nil)
		must(t, e)
		defer r.Close()
		v, e := r.reviewLEDInventory(ctx, migrationSocket, f.api, false)
		must(t, e)
		p, e := r.preserveLED(ctx, migrationSocket, v.InventoryID, v.InventoryHash, f.api)
		must(t, e)
		a, e := r.adoptLED(ctx, migrationSocket, RecoveryLEDAdoptionRequest{v.InventoryID, v.InventoryHash, p.RecordHash}, f.api)
		must(t, e)
		_, e = r.handoffLED(ctx, migrationSocket, a.RecordHash)
		must(t, e)
	}, true)
	s, e := OpenManagedRecoveryVerification(f.path, f.config, f.binding.After, f.revision, f.binding.CommitHash)
	must(t, e)
	must(t, s.EnrollManagedDeliveryRecovery(f.binding))
	must(t, s.Close())
	f.open(t)
	must(t, f.s.Stage(producer, "aggregate-restored"))
	must(t, f.s.Activate(producer, "aggregate-restored"))
	must(t, f.s.Replace(ctx, snapshot(f.now, 2, 1)))
	t.Cleanup(func() { f.s.Close() })
	return f
}
func (f *runtimeLEDRecoveryFixture) open(t *testing.T) {
	t.Helper()
	var e error
	f.s, e = OpenManagedRecoveredDelivery(f.path, f.config, f.revision, f.binding, protocol.Hash(f.binding.Owner), protocol.Hash("aggregate-activation"))
	must(t, e)
	f.s.now = func() time.Time { return f.now }
	must(t, f.s.BindManagedLEDRecovery(f.api, func(context.Context) error { return nil }))
	f.observe, e = f.s.MonitorRuntimeLoop("delivery-admission")
	must(t, e)
	f.observe(nil)
}
func (f *runtimeLEDRecoveryFixture) proposal(t *testing.T) RuntimeLEDClearProposal {
	t.Helper()
	v, e := f.s.ReviewRuntimeLEDRecovery(context.Background())
	must(t, e)
	if v.Condition != "ok" || len(v.Faults) != 1 || v.Faults[0].Fault.ID != f.id {
		t.Fatal(v)
	}
	return v.Faults[0]
}
func TestRuntimeLEDRecoveryClearAndRestart(t *testing.T) {
	f := newRuntimeLEDRecoveryFixture(t)
	ctx := context.Background()
	f.api.states["foreign"] = led.State{ID: "foreign", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}
	p := f.proposal(t)
	h, e := f.s.Health(ctx)
	must(t, e)
	if h.Condition != "critical" {
		t.Fatal("review removed fault", h)
	}
	v, e := f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash)
	must(t, e)
	if v.Outcome != "acknowledged" || len(f.api.cleared) != 1 || f.api.cleared[0] != f.id {
		t.Fatal(v, f.api.cleared)
	}
	if _, ok := f.api.states["foreign"]; !ok {
		t.Fatal("foreign fault cleared")
	}
	h, e = f.s.Health(ctx)
	must(t, e)
	if h.Condition != "ok" || len(h.InheritedLEDFaults) != 0 {
		t.Fatal(h)
	}
	must(t, f.s.Close())
	f.open(t)
	replay, e := f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash)
	must(t, e)
	if protocol.Hash(replay) != protocol.Hash(v) || len(f.api.cleared) != 1 {
		t.Fatal("historical replay sent another clear")
	}
	h, e = f.s.Health(ctx)
	must(t, e)
	if h.Condition == "ok" {
		t.Fatal("restart reused healthy source session")
	}
}
func TestRuntimeLEDRecoveryRejectsStaleOrChangedEvidence(t *testing.T) {
	for _, change := range []string{"producer", "loop", "expiry", "registry", "hash", "verification", "admission", "source-during-observation", "queue"} {
		t.Run(change, func(t *testing.T) {
			f := newRuntimeLEDRecoveryFixture(t)
			ctx := context.Background()
			p := f.proposal(t)
			switch change {
			case "producer":
				v := snapshot(f.now, 3, 2)
				v.Subjects[0].Condition = "critical"
				must(t, f.s.Replace(ctx, v))
			case "queue":
				message := report(f.now, "uncertain", 1, 1, 1)
				message.Generation = "aggregate-restored"
				_, e := f.s.Enqueue(ctx, message)
				must(t, e)
				job, ok, e := f.s.Grant(ctx, producer, message.ID)
				must(t, e)
				if !ok {
					t.Fatal("fixture grant missing")
				}
				must(t, f.s.Finish(ctx, job, Result{Ambiguous: true}))
			case "loop":
				f.observe(errors.New("loop failed"))
			case "expiry":
				f.now = f.now.Add(time.Hour)
			case "registry":
				v := f.api.states[f.id]
				v.Message = "different incarnation"
				f.api.states[f.id] = v
			case "hash":
				p.Hash = protocol.Hash("other")
			case "verification":
				f.s.verificationOnly = true
			case "admission":
				f.s.runtimeLEDCheck = func(context.Context) error { return errors.New("revoked") }
			case "source-during-observation":
				f.api.read = func() { f.observe(errors.New("loop changed")) }
			}
			if _, e := f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash); e == nil {
				t.Fatal("unsafe clear accepted")
			}
			if len(f.api.cleared) != 0 {
				t.Fatal("unsafe clear dispatched")
			}
		})
	}
}
func TestRuntimeLEDRecoveryInterruptedNeverResends(t *testing.T) {
	for _, point := range []string{"runtime-led-head", "runtime-led-intent", "runtime-led-returned", "runtime-led-result", "runtime-led-recorded"} {
		t.Run(point, func(t *testing.T) {
			f := newRuntimeLEDRecoveryFixture(t)
			ctx := context.Background()
			p := f.proposal(t)
			stop := errors.New("interrupted")
			f.s.ledOperationFault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if _, e := f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash); !errors.Is(e, stop) {
				t.Fatal(e)
			}
			calls := len(f.api.cleared)
			must(t, f.s.Close())
			f.open(t)
			v, e := f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash)
			must(t, e)
			if len(f.api.cleared) != calls {
				t.Fatal("interrupted clear replayed")
			}
			if point == "runtime-led-result" || point == "runtime-led-recorded" {
				if v.Outcome != "acknowledged" {
					t.Fatal(v)
				}
			} else if v.Outcome != "uncertain" {
				t.Fatal(v)
			}
			if v.Outcome == "uncertain" {
				must(t, f.s.Replace(ctx, snapshot(f.now, 3, 2)))
				p = f.proposal(t)
				next, e := f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash)
				must(t, e)
				if next.Outcome != "acknowledged" && next.Outcome != "observed_absent" {
					t.Fatal(next)
				}
			}
		})
	}
}
func TestRuntimeLEDRecoveryLostAnchors(t *testing.T) {
	for _, loss := range []string{"head", "intent", "result", "sql", "head-and-sql"} {
		t.Run(loss, func(t *testing.T) {
			f := newRuntimeLEDRecoveryFixture(t)
			ctx := context.Background()
			p := f.proposal(t)
			_, e := f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash)
			must(t, e)
			switch loss {
			case "head":
				must(t, os.Remove(filepath.Join(f.path, runtimeLEDClearHead)))
			case "intent":
				must(t, os.Remove(filepath.Join(f.path, runtimeLEDClearFile(1)+".json")))
			case "result":
				must(t, os.Remove(filepath.Join(f.path, runtimeLEDClearFile(1)+"-result.json")))
			case "head-and-sql":
				must(t, os.Remove(filepath.Join(f.path, runtimeLEDClearHead)))
				_, e = f.s.db.Exec("DELETE FROM metadata WHERE key=?", runtimeLEDClearKey(1))
				must(t, e)
			case "sql":
				_, e = f.s.db.Exec("DELETE FROM metadata WHERE key=?", runtimeLEDClearKey(1))
				must(t, e)
			}
			if _, e = f.s.Health(ctx); e == nil {
				t.Fatal("lost cleanup proof reported health")
			}
			if _, e = f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash); e == nil {
				t.Fatal("lost cleanup proof recreated")
			}
		})
	}
}
