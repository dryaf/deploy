package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
)

type clearRegistryLED struct {
	*fakeLED
	list func(context.Context) ([]led.Entry, error)
}

func (a clearRegistryLED) List(ctx context.Context) ([]led.Entry, error) {
	return a.list(ctx)
}

func pendingFaultClear(t *testing.T) (*Store, *time.Time, *LEDReconciler, *fakeLED, string, Health) {
	t.Helper()
	s, now, _ := fixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	api := &fakeLED{states: map[string]led.State{}}
	r := &LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	var id string
	must(t, s.db.QueryRow("SELECT id FROM led_states WHERE condition='critical'").Scan(&id))
	must(t, s.Replace(ctx, snapshot(*now, 2, 2)))
	h, err := r.sourceHealth(ctx)
	must(t, err)
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := s.obsoleteLEDStates(tx, workerLEDBinding, "", true)
		return err
	}))
	api.cleared = nil
	return s, now, r, api, id, h
}

func TestLEDClearRequiresMatchingOwnedRegistryState(t *testing.T) {
	for _, changed := range []string{"led", "pattern", "priority", "message", "persistent", "ttl", "duplicate", "unavailable", "oversized"} {
		t.Run(changed, func(t *testing.T) {
			s, _, r, api, id, h := pendingFaultClear(t)
			original := api.states[id]
			value := original
			switch changed {
			case "led":
				value.LED = "red"
			case "pattern":
				value.Pattern = "off"
			case "priority":
				value.Priority--
			case "message":
				value.Message = "another application state"
			case "persistent":
				value.Persistent = false
			case "ttl":
				ttl := 10
				value.TTL = &ttl
			}
			api.states[id] = value
			r.API = clearRegistryLED{api, func(context.Context) ([]led.Entry, error) {
				if !s.mu.TryLock() {
					t.Fatal("registry I/O holds store mutex")
				}
				s.mu.Unlock()
				if changed == "unavailable" {
					return nil, errors.New("registry unavailable")
				}
				entries := api.entries()
				if changed == "duplicate" {
					entries = append(entries, led.Entry{State: value, OwnerUID: os.Getuid()})
				}
				if changed == "oversized" {
					entries = append(entries, make([]led.Entry, 1025)...)
				}
				return entries, nil
			}}
			if err := r.clearLED(context.Background(), id, &h); err == nil {
				t.Fatal("changed registry authorized clear")
			}
			if len(api.cleared) != 0 {
				t.Fatal("issued clear despite registry conflict", api.cleared)
			}
			operation := lastLEDOperation(t, s)
			if operation.Outcome != "not_sent" || !operation.DispatchedAt.IsZero() || operation.Intent.ID != id {
				t.Fatal("registry refusal recorded as a send", operation)
			}
			var obsolete int
			must(t, s.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", id).Scan(&obsolete))
			if obsolete != 1 {
				t.Fatal("refusal lost cleanup obligation")
			}
			api.states[id] = original
			r.API = api
			must(t, r.clearLED(context.Background(), id, &h))
			if len(api.cleared) != 1 || api.cleared[0] != id {
				t.Fatal("fresh matching registry did not permit clear")
			}
		})
	}
}

func TestLEDClearRechecksSourceAfterRegistryIO(t *testing.T) {
	s, now, r, api, id, h := pendingFaultClear(t)
	api.onList = func() {
		v := snapshot(*now, 3, 3)
		v.Subjects[0].Condition = "critical"
		must(t, s.Replace(context.Background(), v))
	}
	if err := r.clearLED(context.Background(), id, &h); !errors.Is(err, errLEDObservationChanged) {
		t.Fatal(err)
	}
	if len(api.cleared) != 0 {
		t.Fatal("cleared after newer fault during registry I/O")
	}
	if operation := lastLEDOperation(t, s); operation.Outcome != "not_sent" {
		t.Fatal(operation)
	}
}

func TestLEDClearAbsentFaultAndHealthyRevocation(t *testing.T) {
	s, _, r, api, id, h := pendingFaultClear(t)
	delete(api.states, id)
	must(t, r.clearLED(context.Background(), id, &h))
	if operation := lastLEDOperation(t, s); operation.Outcome != "acknowledged" {
		t.Fatal(operation)
	}
	r.API = clearRegistryLED{api, func(context.Context) ([]led.Entry, error) {
		t.Fatal("healthy revocation depends on registry")
		return nil, nil
	}}
	_, healthy := r.ids()
	must(t, r.clearLED(context.Background(), healthy, nil))
}

func TestLEDClearIgnoresOtherUIDsOwnership(t *testing.T) {
	_, _, r, api, id, h := pendingFaultClear(t)
	r.API = clearRegistryLED{api, func(context.Context) ([]led.Entry, error) {
		// The same public ID under a different UID is not ours to validate or
		// clear. The real daemon scopes Clear to the authenticated caller UID.
		foreign := led.Entry{State: led.State{ID: id, LED: "red", Pattern: "off"}, OwnerUID: os.Getuid() + 1}
		return append(api.entries(), foreign), nil
	}}
	must(t, r.clearLED(context.Background(), id, &h))
	if len(api.cleared) != 1 || api.cleared[0] != id {
		t.Fatal("other UID incorrectly blocked owned cleanup")
	}
}

func TestLEDClearCancelledDuringRegistryReadIsNotSent(t *testing.T) {
	s, _, r, api, id, h := pendingFaultClear(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.onList = cancel
	if err := r.clearLED(ctx, id, &h); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(api.cleared) != 0 || lastLEDOperation(t, s).Outcome != "not_sent" {
		t.Fatal("cancelled registry read dispatched clear")
	}
}

func TestLEDClearPauseRequiresBoundedNonpersistentLease(t *testing.T) {
	s, _, _ := fixture(t)
	r := &LEDReconciler{Store: s}
	style := r.style("paused")
	ttl := 45
	state := led.State{ID: "paused", LED: r.targetLED(), Pattern: style.Pattern, Priority: style.Priority, Message: "Notification state: paused", TTL: &ttl}
	api := &fakeLED{states: map[string]led.State{state.ID: state}}
	r.API = api
	op := ledOperation{Kind: "clear", ID: state.ID, Condition: "paused"}
	must(t, r.checkLEDClearRegistry(context.Background(), op))
	for _, changed := range []string{"persistent", "missing-ttl", "expired", "too-long"} {
		v := state
		switch changed {
		case "persistent":
			v.Persistent = true
		case "missing-ttl":
			v.TTL = nil
		case "expired":
			n := 0
			v.TTL = &n
		case "too-long":
			n := 91
			v.TTL = &n
		}
		api.states[state.ID] = v
		if err := r.checkLEDClearRegistry(context.Background(), op); err == nil {
			t.Fatal("invalid pause clear accepted", changed)
		}
	}
}
