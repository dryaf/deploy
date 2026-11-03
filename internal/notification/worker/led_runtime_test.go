package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
)

func TestLEDRuntimeRenewalDuringRPCDoesNotStarveRecovery(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	observe, err := s.MonitorRuntimeLoop("signal-intake")
	must(t, err)
	observe(nil)
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	var fault string
	for id, state := range api.states {
		if state.Priority == 90 {
			fault = id
		}
	}
	if fault == "" {
		t.Fatal("missing initial fault")
	}
	must(t, s.Replace(ctx, snapshot(*now, 2, 2)))
	deadline := now.Add(90 * time.Second)
	var publishedAt time.Time
	api.onSet = func(state led.State) {
		if state.Pattern == "heartbeat" {
			publishedAt = *now
		}
	}
	api.onStatus = func() { *now = now.Add(time.Second); observe(nil) }
	api.onClear = func(string) { *now = now.Add(time.Second); observe(nil) }
	must(t, r.Step(ctx))
	if _, exists := api.states[fault]; exists {
		t.Fatal("ordinary runtime renewal prevented fault retirement")
	}
	_, healthy := r.ids()
	state, exists := api.states[healthy]
	if !exists || state.TTL == nil || *state.TTL != led.HealthyTTL(publishedAt, deadline, r.Budget, 500*time.Millisecond) {
		t.Fatal("healthy lease missing or extended by concurrent renewal", state)
	}
}

func TestLEDRuntimeChangesInvalidatePreparedHealth(t *testing.T) {
	for _, change := range []string{"failure", "failure-recovery", "expired-renewal", "new-monitor", "shorter-deadline", "producer-fault"} {
		t.Run(change, func(t *testing.T) {
			s, now, _ := fixture(t)
			ctx := context.Background()
			must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
			observe, err := s.MonitorRuntimeLoop("signal-intake")
			must(t, err)
			observe(nil)
			api := &fakeLED{states: map[string]led.State{}}
			r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
			api.onStatus = func() {
				switch change {
				case "failure":
					observe(errors.New("offline"))
				case "failure-recovery":
					observe(errors.New("offline"))
					observe(nil)
				case "expired-renewal":
					*now = now.Add(91 * time.Second)
					observe(nil)
				case "new-monitor":
					other, e := s.MonitorRuntimeLoop("telegram-intake")
					must(t, e)
					other(nil)
				case "shorter-deadline":
					v := snapshot(*now, 2, 2)
					v.Subjects[0].ValidUntil = now.Add(30 * time.Second)
					must(t, s.Replace(ctx, v))
				case "producer-fault":
					v := snapshot(*now, 2, 2)
					v.Subjects[0].Condition = "critical"
					must(t, s.Replace(ctx, v))
				}
			}
			must(t, r.Step(ctx))
			_, healthy := r.ids()
			if _, exists := api.states[healthy]; exists {
				t.Fatal("published stale prepared health")
			}
		})
	}
}
