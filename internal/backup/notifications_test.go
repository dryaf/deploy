//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestBackupNotificationBacklogCoversWithoutInventingDelivery(t *testing.T) {
	_, state, _, _, _ := archiveFixture(t)
	policy := configfixture.WorkerPolicy(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		if err := state.observeIncident("capture", string(rune('a'+i)), "offline", now, policy.Notifications); err != nil {
			t.Fatal(err)
		}
	}
	if err := coalesceNotificationBacklog(state, now); err != nil {
		t.Fatal(err)
	}
	var live, covered, accepted int
	state.db.QueryRow("SELECT count(*) FROM outbox WHERE covered_by IS NULL").Scan(&live)
	state.db.QueryRow("SELECT count(*) FROM outbox WHERE covered_by IS NOT NULL").Scan(&covered)
	state.db.QueryRow("SELECT count(*) FROM outbox WHERE accepted IS NOT NULL").Scan(&accepted)
	if live != 1 || covered != 12 || accepted != 0 {
		t.Fatal(live, covered, accepted)
	}
	c := WorkerInstallation{WorkerID: testutil.ID(t, "w"), Policy: policy}
	if err := state.enqueueDaily(c, now.AddDate(0, 0, -2), "old"); err != nil {
		t.Fatal(err)
	}
	if err := state.enqueueDaily(c, now, "current"); err != nil {
		t.Fatal(err)
	}
	var daily int
	state.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='daily' AND covered_by IS NULL").Scan(&daily)
	if daily != 1 {
		t.Fatal("daily backlog was not superseded", daily)
	}
}

func TestBackupDeliveryRecoveryRequiresCatchupAndResumesLostAck(t *testing.T) {
	_, state, _, _, _ := archiveFixture(t)
	policy := configfixture.WorkerPolicy(t)
	policy.Monitoring = config.WorkerMonitoring{}
	c := WorkerInstallation{WorkerID: testutil.ID(t, "w"), Policy: policy}
	now := time.Now().UTC()
	if err := state.observeIncident("capture", "source", "offline", now, policy.Notifications); err != nil {
		t.Fatal(err)
	}
	sender := func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		return notification.DeliveryAttempt{At: now, Ambiguous: true}, errors.New("offline")
	}
	if err := drainNotificationOutbox(context.Background(), state, c, sender, now); err != nil {
		t.Fatal(err)
	}
	var outage deliveryOutage
	if err := state.get("delivery_outage", &outage); err != nil || outage.ID == "" {
		t.Fatal(outage, err)
	}
	sender = func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		return notification.DeliveryAttempt{At: now.Add(2 * time.Minute), Accepted: true}, nil
	}
	if err := drainNotificationOutbox(context.Background(), state, c, sender, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := enqueueDeliveryCatchup(state, now.Add(3*time.Minute), "current observation"); err != nil {
		t.Fatal(err)
	}
	if err := state.get("delivery_outage", &outage); err != nil || outage.CatchupID == "" {
		t.Fatal(outage, err)
	}
	expected := outage.CatchupID
	outage.CatchupID = ""
	if err := state.put("delivery_outage", outage); err != nil {
		t.Fatal(err)
	}
	if err := enqueueDeliveryCatchup(state, now.Add(4*time.Minute), "new observation"); err != nil {
		t.Fatal(err)
	}
	if err := state.get("delivery_outage", &outage); err != nil || outage.CatchupID != expected {
		t.Fatal("lost-ack catchup did not resume", outage, err)
	}
}

func TestBackupReviewCatchupSurvivesNextNotifierPass(t *testing.T) {
	_, s, _, _, _ := archiveFixture(t)
	p := configfixture.WorkerPolicy(t)
	p.Notifications.DailyAt = "00:00"
	c := WorkerInstallation{WorkerID: testutil.ID(t, "w"), Policy: p}
	now := time.Now().UTC()
	if err := s.enqueueDaily(c, now, "current"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE outbox SET accepted=?", now.Format(eventTimeLayout)); err != nil {
		t.Fatal(err)
	}
	if err := markDeliveryOutage(s, now); err != nil {
		t.Fatal(err)
	}
	if err := enqueueDeliveryCatchup(s, now, "recovered"); err != nil {
		t.Fatal(err)
	}
	var outage deliveryOutage
	if err := s.get("delivery_outage", &outage); err != nil || outage.CatchupID == "" {
		t.Fatal("no catchup", err)
	}
	// The pending catch-up survived a failed provider request or process interruption.
	// The next normal notifier pass starts by calling enqueueDaily again.
	if err := s.enqueueDaily(c, now.Add(time.Minute), "current"); err != nil {
		t.Fatal(err)
	}
	var covered string
	if err := s.db.QueryRow("SELECT coalesce(covered_by,'') FROM outbox WHERE id=?", outage.CatchupID).Scan(&covered); err != nil {
		t.Fatal(err)
	}
	if covered != "" {
		t.Fatalf("required catch-up is now unsendable: covered_by=%s; delivery recovery still requires this catch-up's acceptance", covered)
	}
}

func TestBackupReviewLegacyCoveredCatchupCanDeliver(t *testing.T) {
	_, s, c, _ := healthyWorkerFixture(t)
	now := time.Now().UTC()
	testutil.Must(t, markDeliveryOutage(s, now))
	testutil.Must(t, enqueueDeliveryCatchup(s, now, "summary"))
	var outage deliveryOutage
	testutil.Must(t, s.get("delivery_outage", &outage))
	var raw string
	testutil.Must(t, s.db.QueryRow("SELECT record FROM outbox WHERE id=?", outage.CatchupID).Scan(&raw))
	var event notification.NotificationEvent
	testutil.Must(t, json.Unmarshal([]byte(raw), &event))
	event.Kind = "daily"
	body, _ := json.Marshal(event)
	_, err := s.db.Exec("UPDATE outbox SET kind='daily',covered_by='old-daily',record=? WHERE id=?", string(body), outage.CatchupID)
	testutil.Must(t, err)
	testutil.Must(t, enqueueDeliveryCatchup(s, now.Add(time.Minute), "current"))
	sent := 0
	testutil.Must(t, drainNotificationOutbox(context.Background(), s, c, func(_ context.Context, _ string, _ config.WorkerNotifications, e notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		if e.ID == outage.CatchupID {
			sent++
			if e.Kind != "delivery-catchup" {
				return notification.DeliveryAttempt{}, fmt.Errorf("wrong kind %s", e.Kind)
			}
		}
		return notification.DeliveryAttempt{Accepted: true}, nil
	}, now.Add(time.Minute)))
	var accepted int
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE id=? AND accepted IS NOT NULL AND covered_by IS NULL", outage.CatchupID).Scan(&accepted))
	if sent != 1 || accepted != 1 {
		t.Fatalf("catchup not accepted: sent=%d accepted=%d", sent, accepted)
	}
}

func TestBackupNotificationOutboxDedupRecoveryAndFallback(t *testing.T) {
	_, state, _, _, _ := archiveFixture(t)
	policy := configfixture.WorkerPolicy(t)
	policy.Notifications.Fallback = "telegram"
	policy.Notifications.Telegram = &config.WorkerTelegram{TokenFile: "/secret/token", ChatIDFile: "/secret/chat"}
	now := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)
	for range 3 {
		if err := state.observeIncident("capture", "db", "capture failed", now, policy.Notifications); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := state.db.QueryRow("SELECT count(*) FROM outbox").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	later := now.Add(7 * time.Hour)
	for range 2 {
		if err := state.observeIncident("capture", "db", "capture failed", later, policy.Notifications); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.db.QueryRow("SELECT count(*) FROM outbox").Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if err := state.resolveIncident("capture", "db", later, policy.Notifications); err != nil {
		t.Fatal(err)
	}
	if err := state.resolveIncident("capture", "db", later, policy.Notifications); err != nil {
		t.Fatal(err)
	}
	if err := state.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='reminder'").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	calls := 0
	sender := func(_ context.Context, provider string, _ config.WorkerNotifications, _ notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		calls++
		if provider == "signal" {
			return notification.DeliveryAttempt{Provider: provider, At: later, Ambiguous: true}, context.DeadlineExceeded
		}
		return notification.DeliveryAttempt{Provider: provider, At: later, Accepted: true}, nil
	}
	c := WorkerInstallation{WorkerID: testutil.ID(t, "w"), Policy: policy}
	if err := drainNotificationOutbox(context.Background(), state, c, sender, later); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatal("expected primary plus fallback per event", calls)
	}
	if err := state.db.QueryRow("SELECT count(*) FROM outbox WHERE accepted IS NULL").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err := state.enqueueDaily(c, later, "current status"); err != nil {
		t.Fatal(err)
	}
	if err := state.enqueueDaily(c, later.Add(time.Hour), "updated status"); err != nil {
		t.Fatal(err)
	}
	if err := state.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='daily'").Scan(&count); err != nil || count != 1 {
		t.Fatal("civil-day report duplicated", count, err)
	}
}
