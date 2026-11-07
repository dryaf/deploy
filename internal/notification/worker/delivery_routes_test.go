package worker

import (
	"context"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryRoutesRequireSettledQueueAndRetainHistory(t *testing.T) {
	for _, destination := range []bool{false, true} {
		for _, state := range []string{"waiting_status", "queued", "retry", "sending", "provider_accepted", "expired", "superseded", "cancelled"} {
			t.Run(fmt.Sprint(destination, state), func(t *testing.T) {
				ctx := context.Background()
				path, before, id, revision, owner := workerDeliveryFixture(t)
				s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
				must(t, err)
				must(t, s.Activate(producer, "delivery-generation"))
				_, err = s.db.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) VALUES(?,?,?,?,?,?,?,?)", producer.ID, "old-message", 1, protocol.Hash("original"), "original body", "delivery-generation", "original-route", state)
				must(t, err)
				must(t, s.Close())
				after := before
				after.Routes = maps.Clone(before.Routes)
				for name, route := range after.Routes {
					route.Primary = "new-profile"
					route.Revision = "changed"
					after.Routes[name] = route
				}
				pending := state == "waiting_status" || state == "queued" || state == "retry" || state == "sending"
				preflight, migrate := PreflightDeliveryRoutes, MigrateDeliveryRoutes
				if destination {
					preflight, migrate = PreflightDeliveryProviders, MigrateDeliveryProviders
					after = before
				}
				reviewErr := preflight(ctx, path, id, before, after)
				op, err := safefs.NewID("n")
				must(t, err)
				_, err = migrate(ctx, path, id, before, after, op)
				if pending {
					if reviewErr == nil || err == nil {
						t.Fatal("pending message retargeted")
					}
					if _, e := os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(e) {
						t.Fatal("refusal retained intent", e)
					}
				} else {
					must(t, reviewErr)
					must(t, err)
				}
				current := after
				if pending {
					current = before
				}
				s, err = OpenManagedDelivery(path, current, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
				must(t, err)
				var body, route, got string
				must(t, s.db.QueryRow("SELECT body,route_revision,state FROM messages WHERE id='old-message'").Scan(&body, &route, &got))
				status, err := s.OwnershipStatus(producer, "delivery-generation")
				must(t, err)
				must(t, s.Close())
				if body != "original body" || route != "original-route" || got != state || !status.Active {
					t.Fatal("route change altered history or ownership")
				}
			})
		}
	}

}
func TestDeliveryRoutesResumeAcrossProducerAndBudgetChanges(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner := workerDeliveryFixture(t)
			after := before
			after.Routes = maps.Clone(before.Routes)
			after.Routes["audit"] = Route{Revision: "new", Primary: "profile"}
			op, err := safefs.NewID("n")
			must(t, err)
			lost := errors.New("lost route reply")
			_, err = migrateEnrolledPolicy(ctx, path, id, before, after, op, 23, func(at string) error {
				if at == boundary {
					return lost
				}
				return nil
			})
			if !errors.Is(err, lost) {
				t.Fatal(err)
			}
			receipt, err := MigrateDeliveryRoutes(ctx, path, id, before, after, op)
			must(t, err)
			again, err := MigrateDeliveryRoutes(ctx, path, id, before, after, op)
			must(t, err)
			if protocol.Hash(receipt) != protocol.Hash(again) {
				t.Fatal("retry changed receipt")
			}
			quota := after
			quota.MaxMessages++
			op, err = safefs.NewID("n")
			must(t, err)
			_, err = MigrateDeliveryLimits(ctx, path, id, after, quota, op)
			must(t, err)
			timing := quota
			timing.Producers = append([]Enrollment(nil), quota.Producers...)
			timing.Producers[0].MaxSilenceSeconds++
			op, err = safefs.NewID("n")
			must(t, err)
			_, err = MigrateDeliveryProducerPolicy(ctx, path, id, quota, timing, op)
			must(t, err)
			final := timing
			final.Routes = before.Routes
			op, err = safefs.NewID("n")
			must(t, err)
			_, err = MigrateDeliveryRoutes(ctx, path, id, timing, final, op)
			must(t, err)
			s, err := OpenManagedDelivery(path, final, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
			must(t, err)
			must(t, s.Close())
		})
	}
}
