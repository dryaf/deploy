package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
)

type registryLED struct {
	*fakeLED
	list   func([]led.Entry) ([]led.Entry, error)
	status func(led.DaemonStatus) (led.DaemonStatus, error)
}

func (f registryLED) List(ctx context.Context) ([]led.Entry, error) {
	entries, err := f.fakeLED.List(ctx)
	if err != nil || f.list == nil {
		return entries, err
	}
	return f.list(entries)
}

func (f registryLED) Status(ctx context.Context) (led.DaemonStatus, error) {
	s, err := f.fakeLED.Status(ctx)
	if err != nil || f.status == nil {
		return s, err
	}
	return f.status(s)
}

func TestLEDRegistryObservationSuppressionAndExpiry(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	api := &fakeLED{states: map[string]led.State{"another-app": {ID: "another-app", LED: "green", Pattern: "solid", Priority: 99, Persistent: true}}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	h, err := s.Health(ctx)
	must(t, err)
	if h.LED == nil || h.LED.Condition != "suppressed" || !h.LED.RegistryAccepted || !h.LED.Suppressed || h.LED.VisualVerified {
		t.Fatal("suppression was mistaken for visibility", h.LED)
	}
	for _, id := range api.cleared {
		if id == "another-app" {
			t.Fatal("evicted another application's winner")
		}
	}
	delete(api.states, "another-app") // Its owner clears it, not the worker.
	must(t, r.Step(ctx))
	h, err = s.Health(ctx)
	must(t, err)
	if h.LED.Condition != "ok" || h.LED.Suppressed || h.LED.VisualVerified {
		t.Fatal("registry winner not observed", h.LED)
	}
	*now = now.Add(31 * time.Second)
	h, err = s.Health(ctx)
	must(t, err)
	if h.LED.Condition != "unknown" || h.LED.RegistryAccepted {
		t.Fatal("stopped reconciler retained current output health", h.LED)
	}
}

func TestLEDRegistryUncertainResultsRevokeHealthyLease(t *testing.T) {
	for _, change := range []string{"missing-state", "missing-baseline", "changed-state", "foreign-owner", "degraded-after-set", "list-error", "new-source-fault", "daemon-restart"} {
		t.Run(change, func(t *testing.T) {
			s, now, _ := fixture(t)
			ctx := context.Background()
			must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
			base := &fakeLED{states: map[string]led.State{}}
			api := registryLED{fakeLED: base}
			r := LEDReconciler{Store: s, Budget: 2 * time.Second}
			baseline, healthy := r.ids()
			published := false
			base.onSet = func(state led.State) {
				if state.ID != healthy {
					return
				}
				published = true
				switch change {
				case "missing-state":
					delete(base.states, healthy)
				case "missing-baseline":
					delete(base.states, baseline)
				case "changed-state":
					state.Pattern = "solid"
					base.states[healthy] = state
				case "new-source-fault":
					v := snapshot(*now, 2, 2)
					v.Subjects[0].Condition = "critical"
					must(t, s.Replace(ctx, v))
				}
			}
			api.list = func(entries []led.Entry) ([]led.Entry, error) {
				if published && change == "list-error" {
					return nil, errors.New("private-provider-secret")
				}
				if published && change == "foreign-owner" {
					for i := range entries {
						if entries[i].ID == healthy {
							entries[i].OwnerUID = os.Getuid() + 1
						}
					}
				}
				return entries, nil
			}
			api.status = func(status led.DaemonStatus) (led.DaemonStatus, error) {
				status.Degraded = published && change == "degraded-after-set"
				if published && change == "daemon-restart" {
					status.LEDs["green"] = json.RawMessage(`{"enabled":true,"available":true,"default_pattern":"off","selected_state":null}`)
				}
				return status, nil
			}
			r.API = api
			err := r.Step(ctx)
			if change != "new-source-fault" && err == nil {
				t.Fatal("uncertain registry observation accepted")
			}
			if _, exists := base.states[healthy]; exists {
				t.Fatal("uncertain update retained healthy lease")
			}
			h, err := s.Health(ctx)
			must(t, err)
			if h.LED == nil || h.LED.Condition != "unknown" || h.LED.RegistryAccepted {
				t.Fatal("uncertain output presented as healthy", h.LED)
			}
			raw, err := json.Marshal(h)
			must(t, err)
			if strings.Contains(string(raw), "private-provider-secret") {
				t.Fatal("LED diagnostics exposed raw errors")
			}
		})
	}
}

func TestLEDRegistryCapacityPreservesOtherApplications(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global-%t", global), func(t *testing.T) {
			s, now, _ := fixture(t)
			ctx := context.Background()
			must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
			base := &fakeLED{states: map[string]led.State{}}
			api := registryLED{fakeLED: base, list: func(entries []led.Entry) ([]led.Entry, error) {
				limit, owner := 127, os.Getuid()
				if global {
					limit, owner = 1023, owner+1
				}
				for i := 0; i < limit; i++ {
					entries = append(entries, led.Entry{OwnerUID: owner, State: led.State{ID: fmt.Sprintf("foreign-%d", i)}})
				}
				return entries, nil
			}}
			r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
			if err := r.Step(ctx); err == nil {
				t.Fatal("full registry admitted another state")
			}
			_, healthy := r.ids()
			for _, id := range base.cleared {
				if id != healthy {
					t.Fatal("quota pressure cleared an unrelated state", id)
				}
			}
			if _, exists := base.states[healthy]; exists {
				t.Fatal("capacity failure retained healthy lease")
			}
		})
	}
}
