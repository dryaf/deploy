package worker

import (
	"context"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeliveryDeclarationReviewRetainsLiveLeaseAndQueuedWork(t *testing.T) {
	ctx := context.Background()
	path, c, id, revision, owner := workerDeliveryFixture(t)
	s, err := OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
	must(t, err)
	defer s.Close()
	must(t, s.Activate(producer, "delivery-generation"))
	msg := report(time.Now().UTC(), "pending-noop", 1, 1, 1)
	msg.Generation = "delivery-generation"
	queued, err := s.Enqueue(ctx, msg)
	must(t, err)
	for range 2 {
		must(t, PreflightDeliveryDeclaration(ctx, path, id, c))
	}
	got, err := s.Get(ctx, producer, msg.ID)
	must(t, err)
	status, err := s.OwnershipStatus(producer, "delivery-generation")
	must(t, err)
	if !status.Active || protocol.Hash(got) != protocol.Hash(queued) {
		t.Fatal("declaration changed live work")
	}
	if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
		t.Fatal("declaration wrote a policy receipt", err)
	}
	_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", managedDeliveryOwnerKey)
	must(t, err)
	if err = PreflightDeliveryDeclaration(ctx, path, id, c); err == nil {
		t.Fatal("missing SQL owner was accepted")
	}
}
