package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func workerDeliveryFixture(t *testing.T) (string, Config, protocol.Identity, string, ManagedDeliveryOwner) {
	t.Helper()
	s, _, path := fixture(t)
	c, id := s.config, s.Identity()
	must(t, s.Revoke(context.Background(), producer, "generation-one"))
	must(t, s.Stage(producer, "delivery-generation"))
	must(t, s.Close())
	revision, err := safefs.NewID("n")
	must(t, err)
	installation, err := safefs.NewID("n")
	must(t, err)
	owner := ManagedDeliveryOwner{Version: 1, Root: t.TempDir(), Installation: installation, Identity: id, ConfigHash: protocol.Hash(c)}
	s, err = OpenManagedRuntime(path, c, id, revision)
	must(t, err)
	must(t, s.EnrollManagedDelivery(owner))
	must(t, s.Close())
	return path, c, id, revision, owner
}

func TestManagedDeliveryWorkerOwnership(t *testing.T) {
	path, c, id, revision, owner := workerDeliveryFixture(t)
	if rogue, err := Open(path, c, false); err == nil {
		rogue.Close()
		t.Fatal("unmanaged startup bypassed enrollment")
	}
	s, err := OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
	must(t, err)
	defer s.Close()
	if s.VerificationOnly() {
		t.Fatal("delivery remained verification-only")
	}
	status, err := s.OwnershipStatus(producer, "delivery-generation")
	must(t, err)
	if status.Active || status.State != "staged" {
		t.Fatal("service activation activated a producer", status)
	}
	h, err := s.Health(context.Background())
	must(t, err)
	if h.DeliveryActivationHash != protocol.Hash("activation") {
		t.Fatal(h)
	}
	must(t, s.Close())
	verification, err := OpenManagedRuntime(path, c, id, revision)
	must(t, err)
	defer verification.Close()
	if !verification.VerificationOnly() {
		t.Fatal("deactivation could not reopen verification")
	}
	must(t, verification.RequireManagedDeliveryOwner(owner, protocol.Hash(owner)))
}

func TestManagedDeliveryWorkerRetainsIndependentFences(t *testing.T) {
	for _, loss := range []string{"file", "sql", "foreign-owner"} {
		t.Run(loss, func(t *testing.T) {
			path, c, id, revision, owner := workerDeliveryFixture(t)
			if loss == "file" {
				must(t, os.Remove(filepath.Join(path, managedDeliveryOwnerFile)))
			} else if loss == "sql" {
				d, err := privateDirectory(path)
				must(t, err)
				db, err := snapshotDB(d, "state.db", false)
				must(t, err)
				_, err = db.Exec("DELETE FROM metadata WHERE key=?", managedDeliveryOwnerKey)
				must(t, err)
				must(t, db.Close())
				must(t, d.Close())
			} else {
				owner.Root = t.TempDir()
			}
			if rogue, err := Open(path, c, false); err == nil {
				rogue.Close()
				t.Fatal("ordinary startup ignored remaining ownership fence")
			}
			if delivery, err := OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation")); err == nil {
				delivery.Close()
				t.Fatal("delivery ignored lost owner authority")
			}
		})
	}
}

func TestManagedDeliveryWorkerRequiresExplicitGeneration(t *testing.T) {
	path, c, id, revision, owner := workerDeliveryFixture(t)
	s, err := OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
	must(t, err)
	defer s.Close()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(now, 1, 1)))
	m := report(now, "explicit-generation-report", 1, 1, 1)
	m.Generation = "delivery-generation"
	_, err = s.Enqueue(ctx, m)
	must(t, err)
	calls := 0
	sender := senderFunc(func(context.Context, string, protocol.Message) Result { calls++; return Result{Accepted: true} })
	_, err = s.DispatchOne(ctx, producer, sender)
	must(t, err)
	if calls != 0 {
		t.Fatal("service activation sent a staged generation")
	}
	must(t, s.Activate(producer, "delivery-generation"))
	_, err = s.DispatchOne(ctx, producer, sender)
	must(t, err)
	if calls != 1 {
		t.Fatal("explicit generation admission did not dispatch eligible work", calls)
	}
}
