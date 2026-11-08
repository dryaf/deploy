package worker

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestCommandRoutesRefusePendingDeliveryBeforeIntent(t *testing.T) {
	ctx := context.Background()
	path, before, id, revision, owner, head, pending := commandDeliveryLimitsFixture(t)
	store, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("active"))
	must(t, err)
	m := report(time.Now().UTC(), "pending-route", 1, 1, 1)
	m.Generation = "delivery-generation"
	_, err = store.Enqueue(ctx, m)
	must(t, err)
	must(t, store.Close())
	after := before
	after.Routes = maps.Clone(before.Routes)
	after.Routes["audit"] = before.Routes[before.Producers[0].Routes[0]]
	if err = PreflightCommandDeliveryRoutes(ctx, path, id, before, after, head); err == nil {
		t.Fatal("pending message review accepted")
	}
	op, err := safefs.NewID("n")
	must(t, err)
	if _, err = MigrateCommandDeliveryRoutes(ctx, path, id, before, after, op, head); err == nil {
		t.Fatal("pending message migrated")
	}
	if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
		t.Fatal("failed route retained an intent", err)
	}
	store, err = OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("active"))
	must(t, err)
	defer store.Close()
	if getCommand(t, store, pending.Command.ID).Command.ID != pending.Command.ID {
		t.Fatal("queued command lost")
	}
}
