//go:build notificationintegration

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestProducerRetirementPinnedDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	api := led.Client{Socket: ledfixture.Start(t), Budget: time.Second}
	s, now, path := bindingFixture(t, binding("selected", "backup"))
	config := s.config
	r := LEDReconciler{Store: s, API: api, Budget: time.Second}
	foreign := led.State{ID: "other-application", LED: "green", Pattern: "blink-slow", Priority: 1, Persistent: true, Message: "Unowned application state"}
	must(t, api.Set(ctx, foreign))
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	v.Sequence, v.BasisRevision, v.Subjects[0].Condition = 2, 2, "retired"
	must(t, s.Replace(ctx, v))
	// Only the runtime's fresh domain evidence clears the original faults.
	must(t, r.Step(ctx))
	must(t, s.Revoke(ctx, producer, "generation-one"))
	_, err := s.RetireProducer(ctx, producer, "operator", "Source retired after domain cleanup", true)
	must(t, err)
	must(t, s.Close())
	reopened, err := Open(path, config, false)
	must(t, err)
	defer reopened.Close()
	r = LEDReconciler{Store: reopened, API: api, Budget: time.Second}
	must(t, r.Step(ctx))
	entries, err := api.List(ctx)
	must(t, err)
	if len(entries) != 3 {
		t.Fatal("unexpected registry after retired startup", entries)
	}
	for _, e := range entries {
		if e.ID == foreign.ID {
			if e.State != foreign {
				t.Fatal("retired startup changed unowned state", e)
			}
			continue
		}
		if !strings.HasSuffix(e.ID, ".baseline") || e.TTL != nil || !e.Persistent || e.Pattern == "heartbeat" {
			t.Fatal("retired startup created a fault or healthy lease", e)
		}
	}
	h, err := reopened.Health(ctx)
	must(t, err)
	if h.Condition != "retired" || len(h.RetiredProducers) != 1 {
		t.Fatal("retirement lost across runtime startup", h)
	}
}
