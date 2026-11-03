package worker

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type uncertainLED struct{ *fakeLED }

func (f uncertainLED) Set(ctx context.Context, s led.State) error {
	_ = f.fakeLED.Set(ctx, s)
	return errors.New("lost set reply")
}
func (f uncertainLED) Clear(ctx context.Context, id string) error {
	_ = f.fakeLED.Clear(ctx, id)
	return errors.New("lost clear reply")
}

func preparedHealthyLED(t *testing.T, s *Store) (*LEDReconciler, Health, led.State) {
	t.Helper()
	h, err := s.Health(context.Background())
	must(t, err)
	r := &LEDReconciler{Store: s, API: &fakeLED{states: map[string]led.State{}}, Budget: 2 * time.Second}
	_, id := r.ids()
	ttl := led.HealthyTTL(s.now(), h.ValidUntil, r.Budget, 500*time.Millisecond)
	style := r.style("ok")
	return r, h, led.State{ID: id, LED: "green", Pattern: style.Pattern, Priority: style.Priority, TTL: &ttl, Message: "Required producers have fresh evidence"}
}

func lastLEDOperation(t *testing.T, s *Store) ledOperationEntry {
	t.Helper()
	head, err := readLEDHead(context.Background(), s.db, workerLEDBinding)
	must(t, err)
	e, err := readLEDEntry(context.Background(), s.db, workerLEDBinding, head.Issued)
	must(t, err)
	return e
}

func TestLEDOperationLostAcknowledgements(t *testing.T) {
	for _, kind := range []string{"set", "clear"} {
		t.Run(kind, func(t *testing.T) {
			s, now, path := fixture(t)
			ctx := context.Background()
			must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
			r, h, state := preparedHealthyLED(t, s)
			api := &fakeLED{states: map[string]led.State{state.ID: state}}
			r.API = uncertainLED{api}
			var err error
			if kind == "set" {
				_, _, err = r.mutateLED(ctx, kind, state.ID, &state, &h)
			} else {
				err = r.clearLED(ctx, state.ID, nil)
			}
			if err == nil {
				t.Fatal("lost reply acknowledged")
			}
			e := lastLEDOperation(t, s)
			if e.Outcome != "uncertain" || e.DispatchedAt.IsZero() {
				t.Fatal(e)
			}
			must(t, s.Close())
			reopened, err := Open(path, configuration(), false)
			must(t, err)
			defer reopened.Close()
			reopened.now = func() time.Time { return *now }
			status, err := reopened.LEDOperations(ctx)
			must(t, err)
			if status[workerLEDBinding].Uncertain != 1 || status[workerLEDBinding].LastAcknowledgedRevision != 0 {
				t.Fatal(status)
			}
			api.onSet = func(st led.State) {
				if st.ID == state.ID {
					t.Error("restart replayed old healthy state")
				}
			}
			must(t, (&LEDReconciler{Store: reopened, API: api, Budget: 2 * time.Second}).Step(ctx))
			if _, exists := api.states[state.ID]; exists {
				t.Fatal("startup did not revoke old lease")
			}
		})
	}
}

func TestLEDOperationRechecksEvidenceAfterIntent(t *testing.T) {
	for _, change := range []string{"source", "expiry", "shorter", "cancel"} {
		t.Run(change, func(t *testing.T) {
			s, now, _ := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			v := snapshot(*now, 1, 1)
			v.Subjects[0].ValidUntil = now.Add(40 * time.Second)
			must(t, s.Replace(ctx, v))
			r, h, state := preparedHealthyLED(t, s)
			sent := 0
			r.API.(*fakeLED).onSet = func(st led.State) { sent++ }
			s.ledOperationFault = func(stage string) error {
				if stage != "intent_committed" {
					return nil
				}
				switch change {
				case "source":
					v.Sequence, v.BasisRevision = 2, 2
					v.Subjects[0].Condition = "critical"
					return s.Replace(ctx, v)
				case "expiry":
					*now = now.Add(40 * time.Second)
				case "shorter":
					*now = now.Add(10 * time.Second)
				case "cancel":
					cancel()
				}
				return nil
			}
			actual, until, err := r.mutateLED(ctx, "set", state.ID, &state, &h)
			e := lastLEDOperation(t, s)
			if change == "shorter" {
				must(t, err)
				if sent != 1 || *actual.TTL != *state.TTL-10 || e.Sent == nil || *e.Sent.TTL != *actual.TTL || until.After(h.ValidUntil) {
					t.Fatal("intent delay extended lease", actual, e)
				}
			} else if err == nil || sent != 0 || e.Outcome != "not_sent" {
				t.Fatal("stale operation sent", sent, e, err)
			}
			_, err = s.LEDOperations(context.Background())
			must(t, err)
		})
	}
}

func TestLEDOperationClearRechecksFreshResolution(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	api := &fakeLED{states: map[string]led.State{}}
	r := &LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	id := bindingFault(t, s, workerLEDBinding, "critical")
	v.Sequence, v.BasisRevision = 2, 2
	v.Subjects[0].Condition = "ok"
	must(t, s.Replace(ctx, v))
	h, err := s.Health(ctx)
	must(t, err)
	must(t, s.transaction(ctx, func(tx *sql.Tx) error { _, e := s.obsoleteLEDStates(tx, workerLEDBinding, "", true); return e }))
	s.ledOperationFault = func(stage string) error {
		if stage == "intent_committed" {
			v.Sequence, v.BasisRevision = 3, 3
			v.Subjects[0].Condition = "critical"
			return s.Replace(ctx, v)
		}
		return nil
	}
	if r.clearLED(ctx, id, &h) == nil {
		t.Fatal("changed evidence admitted clear")
	}
	if _, exists := api.states[id]; !exists {
		t.Fatal("fault cleared after new critical evidence")
	}
	if e := lastLEDOperation(t, s); e.Outcome != "not_sent" {
		t.Fatal(e)
	}
}

func TestLEDOperationBoundedHistoryKeepsAcknowledgement(t *testing.T) {
	s, _, path := fixture(t)
	ctx := context.Background()
	api := &fakeLED{states: map[string]led.State{}}
	r := &LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	_, id := r.ids()
	must(t, r.clearLED(ctx, id, nil))
	r.API = uncertainLED{api}
	for i := 0; i < ledOperationRetention+5; i++ {
		if r.clearLED(ctx, id, nil) == nil {
			t.Fatal("expected lost reply")
		}
	}
	status, err := s.LEDOperations(ctx)
	must(t, err)
	got := status[workerLEDBinding]
	if got.Issued != 70 || got.PrunedThrough != 6 || got.PrunedUnconfirmed != 5 || got.Uncertain != 64 || got.LastAcknowledgedRevision != 1 {
		t.Fatal(got)
	}
	must(t, s.Close())
	reopened, err := Open(path, configuration(), false)
	must(t, err)
	defer reopened.Close()
	again, err := reopened.LEDOperations(ctx)
	must(t, err)
	if protocol.Hash(status) != protocol.Hash(again) {
		t.Fatal("history changed across restart")
	}
}

func TestLEDOperationJournalRejectsCorruption(t *testing.T) {
	for _, damage := range []string{"digest", "epoch", "mapping", "ownership", "ttl", "ack", "gap", "orphan", "head"} {
		t.Run(damage, func(t *testing.T) {
			s, now, path := fixture(t)
			ctx := context.Background()
			must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
			r, h, state := preparedHealthyLED(t, s)
			_, _, err := r.mutateLED(ctx, "set", state.ID, &state, &h)
			must(t, err)
			e := lastLEDOperation(t, s)
			switch damage {
			case "digest":
				_, err = s.db.Exec("UPDATE led_operations SET hash='broken'")
			case "gap":
				_, err = s.db.Exec("DELETE FROM led_operations")
			case "orphan":
				_, err = s.db.Exec("DELETE FROM led_operation_heads")
			case "head":
				_, err = s.db.Exec("UPDATE led_operation_heads SET hash='broken'")
			default:
				switch damage {
				case "epoch":
					e.Intent.Identity.Recovery = "other-epoch"
				case "mapping":
					e.Intent.LayoutHash = protocol.Hash("other-layout")
				case "ownership":
					e.Intent.Ownership = "legacy"
				case "ttl":
					ttl := 91
					e.Sent.TTL = &ttl
				case "ack":
					e.Outcome = "pending"
				}
				body, _ := json.Marshal(e)
				_, err = s.db.Exec("UPDATE led_operations SET body=?,hash=?", string(body), protocol.Hash(e))
			}
			must(t, err)
			if _, err = s.LEDOperations(ctx); err == nil {
				t.Fatal("corrupt history accepted")
			}
			if _, err = s.Snapshot(ctx, filepath.Join(t.TempDir(), "snapshot")); err == nil {
				t.Fatal("corrupt history snapshotted")
			}
			must(t, s.Close())
			if reopened, err := Open(path, configuration(), false); err == nil {
				reopened.Close()
				t.Fatal("corrupt history admitted startup")
			}
		})
	}
}

func TestLEDOperationSchemaSevenUpgradeAndRollback(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			s, _, path := fixture(t)
			_, err := s.db.Exec("DROP TABLE led_migration_operations; DROP TABLE led_migration_heads; DROP TABLE led_migrations; DROP TABLE led_operations; DROP TABLE led_operation_heads; PRAGMA user_version=7")
			must(t, err)
			if conflict {
				_, err = s.db.Exec("CREATE TABLE led_operations(occupied INTEGER)")
				must(t, err)
			}
			must(t, s.Close())
			reopened, err := Open(path, configuration(), false)
			if !conflict {
				must(t, err)
				defer reopened.Close()
				status, err := reopened.LEDOperations(context.Background())
				must(t, err)
				if len(status) != 0 {
					t.Fatal("migration invented historical acknowledgements")
				}
				return
			}
			if err == nil {
				reopened.Close()
				t.Fatal("migration overwrote conflicting table")
			}
			db, err := sql.Open("sqlite", filepath.Join(path, "state.db"))
			must(t, err)
			defer db.Close()
			var version, heads int
			must(t, db.QueryRow("PRAGMA user_version").Scan(&version))
			must(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='led_operation_heads'").Scan(&heads))
			if version != 7 || heads != 0 {
				t.Fatal("partial migration committed", version, heads)
			}
		})
	}
}

func TestLEDOperationSnapshotHistoryKeepsOriginalEpoch(t *testing.T) {
	s, now, path := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	r, h, state := preparedHealthyLED(t, s)
	_, _, err := r.mutateLED(ctx, "set", state.ID, &state, &h)
	must(t, err)
	before := s.Identity()
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	must(t, s.Close())
	j, err := prepareSnapshotRecovery(ctx, path, artifact, configuration(), nil)
	must(t, err)
	db := recoveryCandidate(t, path, j)
	must(t, checkLEDOperations(ctx, db, before))
	if err = checkLEDOperations(ctx, db, j.After); err == nil {
		t.Fatal("old acknowledgement acquired recovery epoch")
	}
	if reopened, err := Open(path, configuration(), false); err == nil {
		reopened.Close()
		t.Fatal("restore admitted LED reconciliation")
	}
}

func TestLEDOperationCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_OPERATION_STATE")
	if path == "" {
		return
	}
	s, err := Open(path, configuration(), false)
	must(t, err)
	defer s.Close()
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(time.Now(), 2, 2)))
	r, h, state := preparedHealthyLED(t, s)
	kind := os.Getenv("DEPLOY_LED_OPERATION_KIND")
	id := os.Getenv("DEPLOY_LED_OPERATION_ID")
	if kind == "clear" {
		must(t, s.transaction(ctx, func(tx *sql.Tx) error { _, err := s.obsoleteLEDStates(tx, workerLEDBinding, "", true); return err }))
	}
	s.ledOperationFault = func(stage string) error {
		if stage == os.Getenv("DEPLOY_LED_OPERATION_BOUNDARY") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	record := func(value any) {
		raw, err := json.Marshal(value)
		must(t, err)
		f, err := os.OpenFile(filepath.Join(path, "test-led-request.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		must(t, err)
		_, err = f.Write(raw)
		must(t, err)
		must(t, f.Sync())
		must(t, f.Close())
	}
	api := r.API.(*fakeLED)
	api.onSet = func(st led.State) { record(st) }
	api.onClear = func(id string) { record(led.State{ID: id}) }
	if kind == "clear" {
		err = r.clearLED(ctx, id, &h)
	} else {
		_, _, err = r.mutateLED(ctx, "set", state.ID, &state, &h)
	}
	must(t, err)
	t.Fatal("crash boundary not reached")
}

func TestLEDOperationSIGKILL(t *testing.T) {
	for _, kind := range []string{"set", "clear"} {
		for _, boundary := range []string{"intent_committed", "rpc_returned", "ack_committed"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				s, _, path := fixture(t)
				var faultID string
				if kind == "clear" {
					must(t, s.transaction(context.Background(), func(tx *sql.Tx) error {
						var err error
						faultID, err = s.currentLEDState(tx, workerLEDBinding, "critical")
						return err
					}))
				}
				must(t, s.Close())
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLEDOperationCrashChild$")
				cmd.Env = append(os.Environ(), "DEPLOY_LED_OPERATION_STATE="+path, "DEPLOY_LED_OPERATION_BOUNDARY="+boundary, "DEPLOY_LED_OPERATION_KIND="+kind, "DEPLOY_LED_OPERATION_ID="+faultID)
				pipe, err := cmd.StdoutPipe()
				must(t, err)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				must(t, cmd.Start())
				defer func() {
					if cmd.ProcessState == nil {
						cmd.Process.Kill()
						cmd.Wait()
					}
				}()
				scanner := bufio.NewScanner(pipe)
				if !scanner.Scan() || scanner.Text() != "READY" {
					t.Fatalf("boundary not reached: %s %s", scanner.Text(), stderr.String())
				}
				must(t, cmd.Process.Kill())
				if cmd.Wait() == nil {
					t.Fatal("expected SIGKILL")
				}
				reopened, err := Open(path, configuration(), false)
				must(t, err)
				defer reopened.Close()
				e := lastLEDOperation(t, reopened)
				want := "pending"
				if boundary == "ack_committed" {
					want = "acknowledged"
				}
				if e.Outcome != want {
					t.Fatal(e)
				}
				api := &fakeLED{states: map[string]led.State{}}
				if kind == "clear" {
					api.states[faultID] = led.State{ID: faultID, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true, Message: "Notification state: critical"}
				}
				raw, err := os.ReadFile(filepath.Join(path, "test-led-request.json"))
				if boundary == "intent_committed" {
					if !os.IsNotExist(err) {
						t.Fatal("RPC preceded intent boundary", err)
					}
				} else {
					must(t, err)
					var sent led.State
					must(t, json.Unmarshal(raw, &sent))
					if kind == "set" {
						api.states[sent.ID] = sent
					} else {
						delete(api.states, sent.ID)
					}
				}
				api.onSet = func(st led.State) {
					if strings.HasSuffix(st.ID, ".healthy") {
						t.Error("old healthy intent replayed")
					}
				}
				api.onClear = func(id string) {
					if id == faultID {
						t.Error("fault clear replayed without fresh evidence")
					}
				}
				must(t, (&LEDReconciler{Store: reopened, API: api, Budget: 2 * time.Second}).Step(context.Background()))
				for id := range api.states {
					if strings.HasSuffix(id, ".healthy") {
						t.Fatal("old lease survived startup")
					}
				}
				if kind == "clear" {
					var obsolete int
					must(t, reopened.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", faultID).Scan(&obsolete))
					if obsolete != 1 {
						t.Fatal("cleanup tombstone lost")
					}
					api.onClear = nil
					api.onSet = nil
					must(t, reopened.Replace(context.Background(), snapshot(time.Now(), 3, 3)))
					must(t, (&LEDReconciler{Store: reopened, API: api, Budget: 2 * time.Second}).Step(context.Background()))
					if err = reopened.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", faultID).Scan(&obsolete); err != sql.ErrNoRows {
						t.Fatal("fresh acknowledged clear did not retire tombstone", err)
					}
				}
				_, err = reopened.LEDOperations(context.Background())
				must(t, err)
			})
		}
	}
}

func TestLEDOperationAcknowledgementSurvivesCancellation(t *testing.T) {
	s, now, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	r, h, state := preparedHealthyLED(t, s)
	r.API.(*fakeLED).onSet = func(st led.State) {
		// The intent is durable and the DB mutex is free while external I/O runs.
		status, err := s.LEDOperations(context.Background())
		must(t, err)
		if status[workerLEDBinding].Pending != 1 {
			t.Error("RPC preceded intent commit")
		}
		cancel()
	}
	_, _, err := r.mutateLED(ctx, "set", state.ID, &state, &h)
	must(t, err)
	e := lastLEDOperation(t, s)
	if e.Outcome != "acknowledged" {
		t.Fatal("parent cancellation erased acknowledgement", e)
	}
}

func TestLEDOperationFailedJournalWriteNeverAcknowledges(t *testing.T) {
	for _, stage := range []string{"intent", "receipt"} {
		t.Run(stage, func(t *testing.T) {
			s, now, _ := fixture(t)
			ctx := context.Background()
			must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
			r, h, state := preparedHealthyLED(t, s)
			sqlText := "CREATE TRIGGER fail_led BEFORE INSERT ON led_operations BEGIN SELECT RAISE(FAIL,'injected disk failure'); END"
			if stage == "receipt" {
				sqlText = "CREATE TRIGGER fail_led BEFORE UPDATE ON led_operations BEGIN SELECT RAISE(FAIL,'injected disk failure'); END"
			}
			_, err := s.db.Exec(sqlText)
			must(t, err)
			calls := 0
			r.API.(*fakeLED).onSet = func(st led.State) { calls++ }
			if _, _, err = r.mutateLED(ctx, "set", state.ID, &state, &h); err == nil {
				t.Fatal("journal failure hidden")
			}
			status, err := s.LEDOperations(ctx)
			must(t, err)
			if stage == "intent" {
				if calls != 0 || len(status) != 0 {
					t.Fatal("unrecorded RPC sent", calls, status)
				}
			} else if calls != 1 || status[workerLEDBinding].Pending != 1 || status[workerLEDBinding].LastAcknowledgedRevision != 0 {
				t.Fatal("failed acknowledgement commit accepted", calls, status)
			}
		})
	}
}

func TestLEDOperationSerializesReconcilersWithoutHoldingStoreMutex(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	api := &fakeLED{states: map[string]led.State{}}
	r := &LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	peer := &LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	api.onSet = func(st led.State) {
		if err := peer.Step(ctx); err == nil {
			t.Error("parallel reconciler admitted")
		}
		if _, err := s.Health(ctx); err != nil {
			t.Error(err)
		}
	}
	must(t, r.Step(ctx))
}

func TestLEDOperationStatusSeparatesHistoricalAcceptanceFromHealth(t *testing.T) {
	s, now, path := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	r, h, state := preparedHealthyLED(t, s)
	_, _, err := r.mutateLED(ctx, "set", state.ID, &state, &h)
	must(t, err)
	must(t, s.Close())
	reopened, err := Open(path, configuration(), false)
	must(t, err)
	defer reopened.Close()
	reply := reopened.Handle(ctx, protocol.Request{Version: protocol.Version, Operation: "system.status", Expected: reopened.Identity()})
	if !reply.OK {
		t.Fatal(reply.Error)
	}
	var status Health
	must(t, json.Unmarshal(reply.Result, &status))
	if status.Condition != "unknown" || status.LED != nil {
		t.Fatal("historical RPC created current health", status)
	}
	history := status.LEDOperations[workerLEDBinding]
	if history.LastAcknowledgedID != state.ID || history.LastAcknowledgedKind != "set" || history.LastAcknowledgedRevision != 1 {
		t.Fatal("acknowledgement absent from status", history)
	}
}

type deferredClearLED struct {
	*fakeLED
	fail bool
}

func (f *deferredClearLED) Clear(ctx context.Context, id string) error {
	if f.fail {
		return errors.New("healthy clear acknowledgement unavailable")
	}
	return f.fakeLED.Clear(ctx, id)
}

func TestLEDOperationDeferralPreservesCleanupFailure(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	must(t, s.Replace(ctx, v))
	api := &deferredClearLED{fakeLED: &fakeLED{states: map[string]led.State{}}}
	r := &LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	_, healthy := r.ids()
	s.ledOperationFault = func(stage string) error {
		if stage != "intent_committed" {
			return nil
		}
		entry := lastLEDOperation(t, s)
		if entry.Intent.ID == healthy && entry.Intent.Kind == "set" {
			v.Sequence, v.BasisRevision = 2, 2
			v.Subjects[0].Condition = "critical"
			api.fail = true
			return s.Replace(ctx, v)
		}
		return nil
	}
	if err := r.Step(ctx); err == nil {
		t.Fatal("source deferral swallowed failed healthy clear")
	}
	h, err := s.Health(ctx)
	must(t, err)
	if h.LED == nil || h.LED.RegistryAccepted || h.LED.Condition != "unknown" {
		t.Fatal("failed cleanup claimed registry health", h.LED)
	}
	status, err := s.LEDOperations(ctx)
	must(t, err)
	if status[workerLEDBinding].Uncertain < 1 {
		t.Fatal("lost clear not recorded", status)
	}
}
