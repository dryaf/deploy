//go:build notificationintegration

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestInheritedLEDOwnershipPinnedDaemon(t *testing.T) {
	f, s := inheritedLEDFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	api := led.Client{Socket: ledfixture.Start(t), Budget: time.Second}
	old := map[string]led.State{}
	for _, id := range []string{f.old, f.new, f.shared} {
		old[id] = f.api.states[id]
		must(t, api.Set(ctx, old[id]))
	}
	foreign := led.State{ID: "other-app", LED: "green", Pattern: "blink-slow", Priority: 1, Persistent: true, Message: "Not owned by deploy"}
	must(t, api.Set(ctx, foreign))
	old[foreign.ID] = foreign
	ttl := 30
	for _, b := range []string{workerLEDBinding, "selected"} {
		must(t, api.Set(ctx, led.State{ID: ledBindingNamespace(s.identity.Worker, b) + ".healthy", LED: "green", Pattern: "heartbeat", Priority: 10, TTL: &ttl}))
	}
	r := LEDReconciler{Store: s, API: api, Budget: time.Second}
	for range 2 {
		must(t, r.Step(ctx))
	}
	entries, err := api.List(ctx)
	must(t, err)
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.ID] = true
		if original, ok := old[entry.ID]; ok && protocol.Hash(entry.State) != protocol.Hash(original) {
			t.Fatal("runtime changed inherited/unowned state", entry)
		}
		if strings.HasSuffix(entry.ID, ".healthy") {
			t.Fatal("healthy lease survived incomplete recovery", entry)
		}
	}
	for id := range old {
		if !seen[id] {
			t.Fatal("runtime cleared inherited/unowned state", id)
		}
	}
	h, err := s.Health(ctx)
	must(t, err)
	if h.InheritedLEDFaults["selected"] != 2 || h.Condition != "critical" {
		t.Fatal("runtime lost inherited fault diagnostics", h)
	}
}
