package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestCommandDeliveryTimingRetainsObservationsAcrossBudgetChanges(t *testing.T) {
	ctx := context.Background()
	path, before, id, revision, owner, head, pending := commandDeliveryLimitsFixture(t)
	s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
	must(t, err)
	observed := time.Now().UTC().Add(-24 * time.Hour)
	observation := snapshot(observed, 1, 1)
	raw, err := json.Marshal(observation)
	must(t, err)
	_, err = s.db.Exec("INSERT INTO status VALUES(?,?,?,?,?,?,?)", producer.ID, 1, 1, protocol.Hash(observation), string(raw), observed.UnixNano(), "original-session")
	must(t, err)
	msg := report(time.Now().UTC(), "pending-during-command-timing", 1, 1, 1)
	msg.Generation = "delivery-generation"
	queued, err := s.Enqueue(ctx, msg)
	must(t, err)
	must(t, s.Close())
	for cycle := range 4 {
		after := before
		after.Producers = append([]Enrollment(nil), before.Producers...)
		migrate := MigrateCommandDeliveryFreshness
		if cycle == 1 {
			after.Producers[0].MaxMessages += 10
			migrate = MigrateCommandDeliveryProducerPolicy
		} else if cycle == 2 {
			after.MaxMessages += 10
			migrate = MigrateCommandDeliveryLimits
		} else {
			after.Producers[0].MaxSilenceSeconds += 10
			after.Producers[0].MaxEvidenceSeconds += 100
		}
		op, err := safefs.NewID("n")
		must(t, err)
		_, err = migrate(ctx, path, id, before, after, op, head)
		must(t, err)
		s, err = OpenManagedDelivery(path, after, id, revision, owner, protocol.Hash(owner), protocol.Hash("renewal"))
		must(t, err)
		var body, session string
		var received int64
		must(t, s.db.QueryRow("SELECT body,received,session FROM status WHERE producer=?", producer.ID).Scan(&body, &received, &session))
		got, err := s.Get(ctx, producer, msg.ID)
		must(t, err)
		status, err := s.OwnershipStatus(producer, "delivery-generation")
		must(t, err)
		command := getCommand(t, s, pending.Command.ID)
		must(t, s.RequireManagedCommandHistory(head))
		must(t, s.Close())
		if body != string(raw) || received != observed.UnixNano() || session != "original-session" || !status.Active || !reflect.DeepEqual(got, queued) || !reflect.DeepEqual(command, pending) {
			t.Fatal("timing/budget updates changed observations, queued work or ownership")
		}
		before = after
	}
}

func TestCommandDeliveryTimingRefusesOtherPolicyBeforeRetainingIntent(t *testing.T) {
	for _, field := range []string{"quota", "routes", "required", "policy-health", "identity", "global-budget"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, _, _, head, _ := commandDeliveryLimitsFixture(t)
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			after.Producers[0].MaxSilenceSeconds += 10
			switch field {
			case "quota":
				after.Producers[0].MaxMessages++
			case "routes":
				after.Producers[0].Routes = nil
			case "required":
				after.Producers[0].Required = !after.Producers[0].Required
			case "policy-health":
				after.Producers[0].PolicyHealth = !after.Producers[0].PolicyHealth
			case "identity":
				after.Producers[0].Producer.Epoch = "changed"
			case "global-budget":
				after.MaxMessages++
			}
			if err := PreflightCommandDeliveryFreshness(ctx, path, id, before, after, head); err == nil {
				t.Fatal("unrelated policy accepted")
			}
			op, err := safefs.NewID("n")
			must(t, err)
			if _, err = MigrateCommandDeliveryFreshness(ctx, path, id, before, after, op, head); err == nil {
				t.Fatal("unrelated policy migrated")
			}
			if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
				t.Fatal("rejection retained intent", err)
			}
		})
	}
}
