package worker

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type fakeLED struct {
	states   map[string]led.State
	cleared  []string
	onClear  func(string)
	onStatus func()
	onSet    func(led.State)
	onList   func()
}

func (f *fakeLED) Set(_ context.Context, s led.State) error {
	f.states[s.ID] = s
	if f.onSet != nil {
		f.onSet(s)
	}
	return nil
}
func (f *fakeLED) List(context.Context) ([]led.Entry, error) {
	if f.onList != nil {
		f.onList()
	}
	return f.entries(), nil
}
func (f *fakeLED) entries() []led.Entry {
	winners := map[string]string{}
	for id, state := range f.states {
		winner := winners[state.LED]
		if winner == "" || state.Priority > f.states[winner].Priority || (state.Priority == f.states[winner].Priority && id < winner) {
			winners[state.LED] = id
		}
	}
	var entries []led.Entry
	for id, state := range f.states {
		entries = append(entries, led.Entry{State: state, OwnerUID: os.Getuid(), Winner: id == winners[state.LED]})
	}
	return entries
}
func (f *fakeLED) Clear(_ context.Context, id string) error {
	delete(f.states, id)
	f.cleared = append(f.cleared, id)
	if f.onClear != nil {
		f.onClear(id)
	}
	return nil
}
func (f *fakeLED) Status(context.Context) (led.DaemonStatus, error) {
	if f.onStatus != nil {
		f.onStatus()
	}
	status := led.DaemonStatus{LEDs: map[string]json.RawMessage{}, Patterns: map[string]json.RawMessage{}}
	for _, style := range standardLEDStyles() {
		status.Patterns[style.Pattern] = json.RawMessage(`[[1,1000]]`)
	}
	for _, color := range []string{"green", "red"} {
		var selected *led.Entry
		for _, entry := range f.entries() {
			if entry.LED == color && entry.Winner {
				copy := entry
				selected = &copy
			}
		}
		raw, err := json.Marshal(map[string]any{"enabled": true, "available": true, "default_pattern": "off", "selected_state": selected})
		if err != nil {
			return status, err
		}
		status.LEDs[color] = raw
	}
	return status, nil
}

func TestLEDNewFaultCannotReceiveQueuedHealthyUpdate(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	_, healthy := r.ids()
	var firstFault string
	for id, state := range api.states {
		if state.Priority == 90 {
			firstFault = id
		}
	}
	if firstFault == "" {
		t.Fatal("missing fault indication")
	}
	v = snapshot(*now, 2, 2)
	must(t, s.Replace(ctx, v))
	api.onClear = func(id string) {
		if id == firstFault {
			v = snapshot(*now, 3, 3)
			v.Subjects[0].Condition = "critical"
			must(t, s.Replace(ctx, v))
		}
	}
	must(t, r.Step(ctx))
	if _, ok := api.states[healthy]; ok {
		t.Fatal("published healthy after newer failure")
	}
	api.onClear = nil
	must(t, r.Step(ctx))
	for id, state := range api.states {
		if state.Priority == 90 && id == firstFault {
			t.Fatal("reused cleared fault incarnation")
		}
	}
}

func TestLEDSubjectRecoveryDoesNotClearAnotherFault(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	v.Subjects = append(v.Subjects, protocol.Subject{Key: "other", Condition: "critical", Reason: "failure", ValidUntil: now.Add(time.Hour)})
	must(t, s.Replace(ctx, v))
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	before := len(api.cleared)
	v.Sequence = 2
	v.BasisRevision = 2
	v.Subjects[0].Condition = "ok"
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	for _, id := range api.cleared[before:] {
		for existing, state := range api.states {
			if state.Priority == 90 && id == existing {
				t.Fatal("cleared unresolved peer fault")
			}
		}
	}
	_, healthy := r.ids()
	if _, ok := api.states[healthy]; ok {
		t.Fatal("one recovery established aggregate health")
	}
}
