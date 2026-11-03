//go:build notificationintegration

package worker

import (
	"context"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestRecoveryLEDPreservationAndAdoptionPinnedDaemon(t *testing.T) {
	ctx := context.Background()
	socket := ledfixture.Start(t)
	api := led.Client{Socket: socket, Budget: time.Second}
	f := newLEDRecoveryFixture(t)
	for _, id := range []string{f.shared, f.old, f.new, "other.application"} {
		must(t, api.Set(ctx, led.State{ID: id, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true, Message: "private mock-driver fault"}))
	}
	before, err := api.List(ctx)
	must(t, err)
	v, err := f.r.reviewLEDInventory(ctx, socket, api, false)
	must(t, err)
	out, err := f.r.preserveLED(ctx, socket, v.InventoryID, v.InventoryHash, api)
	must(t, err)
	if out.Faults != 3 || out.PostSnapshotFaults != 1 || out.SnapshotOnlyFaults != 1 || out.UnownedStates != 1 || out.ClearAllowed || out.ActivationAllowed {
		t.Fatal(out)
	}
	request := RecoveryLEDAdoptionRequest{v.InventoryID, v.InventoryHash, out.RecordHash}
	adopted, err := f.r.adoptLED(ctx, socket, request, api)
	must(t, err)
	if adopted.State != "committed" || adopted.Faults != 3 || adopted.ActivationAllowed || adopted.ClearAllowed {
		t.Fatal(adopted)
	}
	var count, obsolete int
	must(t, f.r.db.QueryRow("SELECT count(*),sum(obsolete) FROM led_states").Scan(&count, &obsolete))
	if count != 3 || obsolete != 0 {
		t.Fatal("incomplete adopted ledger", count, obsolete)
	}
	after, err := api.List(ctx)
	must(t, err)
	if protocol.Hash(before) != protocol.Hash(after) {
		t.Fatal("preservation changed real mock-daemon registry")
	}
	must(t, f.r.Close())
	replay, err := PreserveRecoveryLED(ctx, f.path, f.config, socket, v.InventoryID, v.InventoryHash, nil)
	must(t, err)
	adoptedReplay, err := AdoptRecoveryLED(ctx, f.path, f.config, socket, request, nil)
	must(t, err)
	if adoptedReplay != adopted {
		t.Fatal("adoption retry changed retained proof", adoptedReplay)
	}
	if replay != out {
		t.Fatal("public retry changed retained proof", replay, out)
	}
}
