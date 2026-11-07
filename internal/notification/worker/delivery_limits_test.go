package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestDeliveryLimitsRetainOwnerAndActiveQueuedWork(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner := workerDeliveryFixture(t)
			s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
			must(t, err)
			must(t, s.Activate(producer, "delivery-generation"))
			msg := report(time.Now().UTC(), "retained-during-limits", 1, 1, 1)
			msg.Generation = "delivery-generation"
			queued, err := s.Enqueue(ctx, msg)
			must(t, err)
			must(t, s.Close())
			ownerFile, err := os.ReadFile(filepath.Join(path, managedDeliveryOwnerFile))
			must(t, err)
			current := before
			for cycle := 0; cycle < 2; cycle++ {
				after := current
				after.MaxMessages += 10
				must(t, PreflightDeliveryLimits(ctx, path, id, current, after))
				op, err := safefs.NewID("n")
				must(t, err)
				lost := errors.New("lost limits acknowledgement")
				if _, err = migrateEnrolledPolicy(ctx, path, id, current, after, op, 21, func(at string) error {
					if at == boundary {
						return lost
					}
					return nil
				}); !errors.Is(err, lost) {
					t.Fatal("missed boundary", err)
				}
				wrong := after
				wrong.MaxMessages++
				if _, err = MigrateDeliveryLimits(ctx, path, id, current, wrong, op); err == nil {
					t.Fatal("changed retry accepted")
				}
				receipt, err := MigrateDeliveryLimits(ctx, path, id, current, after, op)
				must(t, err)
				again, err := MigrateDeliveryLimits(ctx, path, id, current, after, op)
				must(t, err)
				if protocol.Hash(again) != protocol.Hash(receipt) {
					t.Fatal("retry changed receipt")
				}
				if stale, e := OpenManagedDelivery(path, current, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation")); e == nil {
					stale.Close()
					t.Fatal("stale config opened after publication")
				}
				s, err = OpenManagedDelivery(path, after, id, revision, owner, protocol.Hash(owner), protocol.Hash("new activation"))
				must(t, err)
				got, err := s.Get(ctx, producer, msg.ID)
				must(t, err)
				status, err := s.OwnershipStatus(producer, "delivery-generation")
				must(t, err)
				must(t, s.Close())
				if !status.Active || protocol.Hash(got) != protocol.Hash(queued) {
					t.Fatal("limits changed queued work or generation")
				}
				actual, err := os.ReadFile(filepath.Join(path, managedDeliveryOwnerFile))
				must(t, err)
				if string(actual) != string(ownerFile) {
					t.Fatal("limits replaced original owner")
				}
				current = after
			}
			// Missing policy evidence cannot reinterpret the immutable owner's config.
			must(t, os.Remove(filepath.Join(path, enrolledLimitsFile)))
			if s, err = OpenManagedDelivery(path, current, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation")); err == nil {
				s.Close()
				t.Fatal("missing policy history accepted")
			}
		})
	}
}

func TestDeliveryLimitsRefuseUndersizedBudgetAndLostAuthorityBeforeIntent(t *testing.T) {
	for _, damage := range []string{"capacity", "owner-file", "owner-sql", "producer-policy"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner := workerDeliveryFixture(t)
			s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
			must(t, err)
			must(t, s.Activate(producer, "delivery-generation"))
			for n := 1; n <= 3; n++ {
				msg := report(time.Now().UTC(), fmt.Sprintf("pending-%d", n), uint64(n), 1, 1)
				msg.Generation = "delivery-generation"
				msg.ReportKey = fmt.Sprintf("daily-%d", n)
				_, err = s.Enqueue(ctx, msg)
				must(t, err)
			}
			if damage == "owner-sql" {
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", managedDeliveryOwnerKey)
				must(t, err)
			}
			must(t, s.Close())
			after := before
			after.MaxMessages++
			switch damage {
			case "capacity":
				after.MaxMessages = 2
			case "owner-file":
				must(t, os.Remove(filepath.Join(path, managedDeliveryOwnerFile)))
			case "producer-policy":
				after.Producers = append([]Enrollment(nil), before.Producers...)
				after.Producers[0].MaxSilenceSeconds++
			}
			if err = PreflightDeliveryLimits(ctx, path, id, before, after); err == nil {
				t.Fatal("review accepted damaged or undersized change")
			}
			op, err := safefs.NewID("n")
			must(t, err)
			if _, err = MigrateDeliveryLimits(ctx, path, id, before, after, op); err == nil {
				t.Fatal("migration accepted damaged authority")
			}
			if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
				t.Fatal("refused change retained policy intent", err)
			}
		})
	}
}

func TestDeliveryLimitsRetainLEDDispositionAndFaults(t *testing.T) {
	for _, kind := range []string{"limits", "producer", "routes", "destination"} {
		producerCase := kind == "producer"
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, disabled), func(t *testing.T) {
				ctx := context.Background()
				path, c, output, api := enrolledLimitsOutputFixture(t, disabled)
				revision, err := safefs.NewID("n")
				must(t, err)
				installation, err := safefs.NewID("n")
				must(t, err)
				owner := ManagedDeliveryOwner{Version: 1, Root: t.TempDir(), Installation: installation, Identity: output.Identity, ConfigHash: protocol.Hash(c)}
				s, err := OpenManagedRuntime(path, c, output.Identity, revision)
				must(t, err)
				must(t, s.EnrollManagedDelivery(owner))
				must(t, s.Close())
				original, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				states, clears := protocol.Hash(api.states), len(api.cleared)
				for cycle := range 2 {
					next := c
					next.MaxMessages++
					migrate := MigrateDeliveryLimits
					version := 26
					if producerCase {
						next = c
						next.Producers = append([]Enrollment(nil), c.Producers...)
						next.Producers[0].MaxSilenceSeconds++
						migrate = MigrateDeliveryProducerPolicy
						version = 29
					}
					if kind == "destination" {
						next = c
						migrate = MigrateDeliveryProviders
						version = 30
					}
					if kind == "routes" {
						next = c
						next.Routes = maps.Clone(c.Routes)
						for name, route := range next.Routes {
							route.Revision = fmt.Sprintf("changed-%d", cycle)
							next.Routes[name] = route
						}
						migrate = MigrateDeliveryRoutes
						version = 28
					}
					op, err := safefs.NewID("n")
					must(t, err)
					receipt, err := migrate(ctx, path, output.Identity, c, next, op)
					must(t, err)
					if receipt.Version != version || receipt.LEDOutputHash == "" || receipt.DeliveryOwner == nil || *receipt.DeliveryOwner != owner {
						t.Fatal("production policy lost output/owner pins")
					}
					s, err = OpenManagedDelivery(path, next, output.Identity, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
					must(t, err)
					must(t, s.RequireLEDOutput(ctx, output.Operation, disabled))
					var faults int
					must(t, s.db.QueryRow("SELECT count(*) FROM led_states WHERE condition='critical' AND obsolete=0").Scan(&faults))
					if faults == 0 {
						t.Fatal("budgets erased unresolved LED fault")
					}
					must(t, s.Close())
					c = next
				}
				actual, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				if string(actual) != string(original) || protocol.Hash(api.states) != states || len(api.cleared) != clears {
					t.Fatal("budgets rewrote output disposition or contacted daemon")
				}
			})
		}
	}

}
