package worker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeliveryProducerPolicyKeepsObservationsAndQueuedWork(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner := workerDeliveryFixture(t)
			s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
			must(t, err)
			must(t, s.Activate(producer, "delivery-generation"))
			observed := time.Now().UTC().Add(-24 * time.Hour)
			observation := snapshot(observed, 1, 1)
			raw, err := json.Marshal(observation)
			must(t, err)
			_, err = s.db.Exec("INSERT INTO status VALUES(?,?,?,?,?,?,?)", producer.ID, 1, 1, protocol.Hash(observation), string(raw), observed.UnixNano(), "original-session")
			must(t, err)
			msg := report(time.Now().UTC(), "retained-policy-report", 1, 1, 1)
			msg.Generation = "delivery-generation"
			queued, err := s.Enqueue(ctx, msg)
			must(t, err)
			must(t, s.Close())
			next := before
			next.Producers = append([]Enrollment(nil), before.Producers...)
			next.Producers[0].MaxSilenceSeconds = 300
			next.Producers[0].MaxEvidenceSeconds = 172800
			next.Producers[0].MaxMessages++
			must(t, PreflightDeliveryProducerPolicy(ctx, path, id, before, next))
			op, err := safefs.NewID("n")
			must(t, err)
			lost := errors.New("lost production policy reply")
			receipt, err := migrateEnrolledPolicy(ctx, path, id, before, next, op, 24, func(at string) error {
				if at == boundary {
					return lost
				}
				return nil
			})
			if !errors.Is(err, lost) {
				t.Fatal("missed boundary", err)
			}
			for range 2 {
				again, err := MigrateDeliveryProducerPolicy(ctx, path, id, before, next, op)
				must(t, err)
				if protocol.Hash(again) != protocol.Hash(receipt) {
					t.Fatal("retry changed policy")
				}
			}
			budgets := next
			budgets.MaxMessages++
			op, err = safefs.NewID("n")
			must(t, err)
			_, err = MigrateDeliveryLimits(ctx, path, id, next, budgets, op)
			must(t, err)
			s, err = OpenManagedDelivery(path, budgets, id, revision, owner, protocol.Hash(owner), protocol.Hash("new activation"))
			must(t, err)
			var body, session string
			var received int64
			must(t, s.db.QueryRow("SELECT body,received,session FROM status WHERE producer=?", producer.ID).Scan(&body, &received, &session))
			got, err := s.Get(ctx, producer, msg.ID)
			must(t, err)
			status, err := s.OwnershipStatus(producer, "delivery-generation")
			must(t, err)
			must(t, s.Close())
			if body != string(raw) || received != observed.UnixNano() || session != "original-session" || !status.Active || protocol.Hash(got) != protocol.Hash(queued) {
				t.Fatal("policy fabricated observations or replaced work/ownership")
			}
		})
	}
}

func TestDeliveryProducerAccessChangesRequireSettledQueue(t *testing.T) {
	for _, damage := range []string{"required", "health", "routes", "context", "identity"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner := workerDeliveryFixture(t)
			s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
			must(t, err)
			must(t, s.Activate(producer, "delivery-generation"))
			msg := report(time.Now().UTC(), "pending-before-access", 1, 1, 1)
			msg.Generation = "delivery-generation"
			_, err = s.Enqueue(ctx, msg)
			must(t, err)
			must(t, s.Close())
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			switch damage {
			case "required":
				after.Producers[0].Required = !after.Producers[0].Required
			case "health":
				after.Producers[0].PolicyHealth = !after.Producers[0].PolicyHealth
			case "routes":
				after.Producers[0].Routes = nil
			case "context":
				after.Producers[0].InteractiveContext = true
				after.Producers[0].ContextKinds = []string{"source"}
			case "identity":
				after.Producers[0].Producer.ID = "replacement"
			}
			if err = PreflightDeliveryProducerPolicy(ctx, path, id, before, after); err == nil {
				t.Fatal("review ignored pending audience/authority")
			}
			op, err := safefs.NewID("n")
			must(t, err)
			if _, err = MigrateDeliveryProducerPolicy(ctx, path, id, before, after, op); err == nil {
				t.Fatal("pending access change admitted")
			}
			if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
				t.Fatal("refused policy retained intent", err)
			}
		})
	}
}

func TestDeliveryProducerAccessChangesKeepOwnerWithSettledQueue(t *testing.T) {
	for _, field := range []string{"required", "health", "context"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner := workerDeliveryFixture(t)
			s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
			must(t, err)
			must(t, s.Activate(producer, "delivery-generation"))
			must(t, s.Close())
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			switch field {
			case "required":
				after.Producers[0].Required = !after.Producers[0].Required
			case "health":
				after.Producers[0].PolicyHealth = !after.Producers[0].PolicyHealth
			case "context":
				after.Producers[0].InteractiveContext = true
				after.Producers[0].ContextKinds = []string{"source"}
			}
			must(t, PreflightDeliveryProducerPolicy(ctx, path, id, before, after))
			op, err := safefs.NewID("n")
			must(t, err)
			_, err = MigrateDeliveryProducerPolicy(ctx, path, id, before, after, op)
			must(t, err)
			s, err = OpenManagedDelivery(path, after, id, revision, owner, protocol.Hash(owner), protocol.Hash("new activation"))
			must(t, err)
			status, err := s.OwnershipStatus(producer, "delivery-generation")
			must(t, err)
			must(t, s.Close())
			if !status.Active {
				t.Fatal("producer authority replaced")
			}
		})
	}
}
