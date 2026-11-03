//go:build notificationintegration

package worker

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestVerificationModePinnedDaemon(t *testing.T) {
	ctx := context.Background()
	api := led.Client{Socket: ledfixture.Start(t), Budget: time.Second}
	s, now, path := bindingFixture(t, binding("selected", "backup"))
	var fault string
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		fault, err = s.currentLEDState(tx, "selected", "critical")
		return err
	}))
	must(t, api.Set(ctx, led.State{ID: fault, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}))
	for _, b := range []string{workerLEDBinding, "selected"} {
		ttl := 20
		must(t, api.Set(ctx, led.State{ID: ledBindingNamespace(s.Identity().Worker, b) + ".healthy", LED: "green", Pattern: "heartbeat", Priority: 10, TTL: &ttl}))
	}
	c := s.config
	must(t, s.Close())
	v, err := OpenVerification(path, c)
	must(t, err)
	defer v.Close()
	v.now = func() time.Time { return *now }
	must(t, v.Replace(ctx, snapshot(*now, 1, 1)))
	r := LEDReconciler{Store: v, API: api, Budget: time.Second}
	must(t, r.Step(ctx))
	entries, err := api.List(ctx)
	must(t, err)
	retained := false
	for _, e := range entries {
		retained = retained || e.ID == fault
		if strings.HasSuffix(e.ID, ".healthy") || e.Pattern == "heartbeat" {
			t.Fatal("verification kept healthy output", e)
		}
	}
	if !retained {
		t.Fatal("verification cleared persistent fault")
	}
	h, err := v.Health(ctx)
	must(t, err)
	if !h.VerificationOnly || h.Condition == "ok" || h.LEDOutputs["selected"].VisualVerified {
		t.Fatal(h)
	}
}
