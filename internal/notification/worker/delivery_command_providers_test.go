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

func TestCommandProvidersRetainQueuedDeliveryOnlyForSameDestination(t *testing.T) {
	ctx := context.Background()
	path, c, id, revision, owner, head, pending := commandDeliveryLimitsFixture(t)
	s, err := OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("active"))
	must(t, err)
	_, err = s.db.Exec("INSERT INTO telegram_cursors VALUES('123',42)")
	must(t, err)
	m := report(time.Now().UTC(), "retained-provider-message", 1, 1, 1)
	m.Generation = "delivery-generation"
	ack, err := s.Enqueue(ctx, m)
	must(t, err)
	must(t, s.Close())
	changed := c
	changed.Routes = maps.Clone(c.Routes)
	changed.Routes["audit"] = c.Routes[c.Producers[0].Routes[0]]
	if err = PreflightCommandDeliveryProviders(ctx, path, id, c, changed, head); err == nil {
		t.Fatal("changed destination accepted unsettled messages")
	}
	op, err := safefs.NewID("n")
	must(t, err)
	if _, err = MigrateCommandDeliveryProviders(ctx, path, id, c, changed, op, head); err == nil {
		t.Fatal("changed destination migrated pending messages")
	}
	if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
		t.Fatal("failed review retained intent", err)
	}
	must(t, PreflightCommandDeliveryProviders(ctx, path, id, c, c, head))
	r, err := MigrateCommandDeliveryProviders(ctx, path, id, c, c, op, head)
	must(t, err)
	if r.Version != 35 || protocol.Hash(r.Before) != protocol.Hash(r.After) {
		t.Fatal("same destination lacked typed receipt")
	}
	s, err = OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("renewed"))
	must(t, err)
	defer s.Close()
	var offset int
	must(t, s.db.QueryRow("SELECT offset FROM telegram_cursors WHERE account='123'").Scan(&offset))
	if offset != 42 {
		t.Fatal("rotation reset intake cursor")
	}
	again, err := s.Enqueue(ctx, m)
	must(t, err)
	if protocol.Hash(ack) != protocol.Hash(again) || getCommand(t, s, pending.Command.ID).Command.ID != pending.Command.ID {
		t.Fatal("rotation changed retained delivery or commands")
	}
}
