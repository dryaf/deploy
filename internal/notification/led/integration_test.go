//go:build notificationintegration

package led

import (
	"context"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

// The release target requires an explicit offline checkout. Ordinary unit tests
// neither download dependencies nor discover a developer's sibling checkout.
func TestPinnedMockDaemon(t *testing.T) {
	socket := ledfixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := Client{Socket: socket, Budget: time.Second}
	var err error
	if err = c.Set(ctx, State{ID: "deploy.test.failure", LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true, Message: "Synthetic fault"}); err != nil {
		t.Fatal(err)
	}
	entries, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "deploy.test.failure" || !entries[0].Winner {
		t.Fatalf("unexpected registry: %+v", entries)
	}
	s, err := c.Status(ctx)
	if err != nil || s.Degraded {
		t.Fatalf("daemon health: %+v %v", s, err)
	}
	if err = c.Clear(ctx, "deploy.test.failure"); err != nil {
		t.Fatal(err)
	}
	ttl := 1
	if err = c.Set(ctx, State{ID: "deploy.test.healthy", LED: "green", Pattern: "heartbeat", Priority: 10, TTL: &ttl}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	entries, err = c.List(ctx)
	if err != nil || len(entries) != 0 {
		t.Fatalf("lease did not expire: %+v %v", entries, err)
	}
}
