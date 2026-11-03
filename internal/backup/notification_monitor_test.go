//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func pipelineMonitorConfig(t *testing.T, f *pipelineFixture) WorkerInstallation {
	c := WorkerInstallation{WorkerID: f.binding.Producer.ID, Policy: configfixture.WorkerPolicy(t)}
	c.Policy.Timezone = "UTC"
	c.Policy.Notifications.DailyAt = "00:00"
	c.Policy.Monitoring.DailyDeliveryPingURLFile = "/fixture/no-real-monitor"
	return c
}
func setPipelineOutage(t *testing.T, f *pipelineFixture, id string) {
	testutil.Must(t, f.state.put("delivery_outage", deliveryOutage{ID: id, CatchupID: "catchup/" + id, StartedAt: f.now}))
}

func TestSharedDeliveryMonitorWaitsForFinalCatchupAndSuccessfulHeartbeat(t *testing.T) {
	f := newPipelineFixture(t)
	seedPipelineDailyHistory(t, f, 128)
	setPipelineOutage(t, f, "long-outage")
	c := pipelineMonitorConfig(t, f)
	var failed []bool
	record := func(_ context.Context, path string, fail bool) error {
		if path != c.Policy.Monitoring.DailyDeliveryPingURLFile {
			t.Fatal("wrong monitor")
		}
		failed = append(failed, fail)
		return nil
	}
	publish := func() {
		t.Helper()
		testutil.Must(t, publishSharedDailyDeliveryHeartbeat(context.Background(), f.state, c, f.now, record))
	}
	publish()
	partial := f.freeze(t, 2, 1)[0]
	f.transfer(t, f.client)
	f.accept(t, partial)
	f.transfer(t, f.client)
	publish()
	assertPipelineOutage(t, f, false)
	final := f.freeze(t, 3, 1)[0]
	f.transfer(t, f.client)
	f.accept(t, final)
	f.transfer(t, f.client)
	failure := errors.New("lost heartbeat reply")
	err := publishSharedDailyDeliveryHeartbeat(context.Background(), f.state, c, f.now, func(context.Context, string, bool) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	var outage deliveryOutage
	testutil.Must(t, f.state.get("delivery_outage", &outage))
	if outage.ID != "long-outage" {
		t.Fatal("failed heartbeat cleared outage")
	}
	var previous string
	if err = f.state.get("daily_ping_period", &previous); err != sql.ErrNoRows {
		t.Fatal("failed heartbeat acknowledged period", previous, err)
	}
	publish()
	publish()
	if fmt.Sprint(failed) != "[true true false]" {
		t.Fatal(failed)
	}
	testutil.Must(t, f.state.get("delivery_outage", &outage))
	if outage.ID != "" {
		t.Fatal("successful heartbeat did not clear completed outage")
	}
}

func TestSharedDeliveryMonitorRejectsStaleCompletion(t *testing.T) {
	for _, change := range []string{"outage", "new-source", "authority", "clock"} {
		t.Run(change, func(t *testing.T) {
			f := newPipelineFixture(t)
			seedPipelineDailyHistory(t, f, 0)
			setPipelineOutage(t, f, "long-outage")
			final := f.freeze(t, 2, 1)[0]
			f.transfer(t, f.client)
			f.accept(t, final)
			f.transfer(t, f.client)
			c := pipelineMonitorConfig(t, f)
			err := publishSharedDailyDeliveryHeartbeat(context.Background(), f.state, c, f.now, func(_ context.Context, _ string, failed bool) error {
				if failed {
					t.Fatal("unexpected failure heartbeat")
				}
				switch change {
				case "clock":
					testutil.Must(t, f.state.put("clock_suspect", true))
				case "outage":
					setPipelineOutage(t, f, "new-outage")
				case "new-source":
					f.observe(t, "a", "new alert after monitor decision", f.now)
				case "authority":
					var control ArchiveControl
					testutil.Must(t, f.state.get("control", &control))
					control.Epoch++
					testutil.Must(t, f.state.put("control", control))
				}
				return nil
			})
			if err == nil {
				t.Fatal("stale completion acknowledged")
			}
			var previous string
			if err = f.state.get("daily_ping_period", &previous); err != sql.ErrNoRows {
				t.Fatal("stale completion changed daily acknowledgement", previous, err)
			}
			var outage deliveryOutage
			testutil.Must(t, f.state.get("delivery_outage", &outage))
			expected := "long-outage"
			if change == "outage" {
				expected = "new-outage"
			}
			if outage.ID != expected {
				t.Fatal("stale completion cleared outage", outage)
			}
		})
	}
}

func TestSharedDeliveryCoverageNeedsAcceptanceProof(t *testing.T) {
	f := newPipelineFixture(t)
	seedPipelineDailyHistory(t, f, 0)
	final := f.freeze(t, 2, 1)[0]
	f.transfer(t, f.client)
	f.accept(t, final)
	f.transfer(t, f.client)
	period := "daily/" + f.binding.Producer.ID + "/" + f.now.Format("2006-01-02")
	check := func(complete bool) {
		t.Helper()
		ledgerTx(t, f.state, func(tx *sql.Tx) error {
			c, err := readBackupDeliveryCoverage(tx, period, "long-outage")
			if err == nil && c.complete() != complete {
				t.Fatalf("coverage=%+v; expected complete=%v", c, complete)
			}
			return err
		})
	}
	check(true)
	testutil.Must(t, f.state.put("clock_suspect", true))
	check(false)
	testutil.Must(t, f.state.put("clock_suspect", false))
	check(true)
	// An outbox acceptance timestamp is insufficient if the exact shared acceptance
	// is ineligible. A waiver is not delivery proof either.
	_, err := f.state.db.Exec("UPDATE notification_exports SET eligible=0 WHERE id=?", final.ID)
	testutil.Must(t, err)
	check(false)
	_, err = f.state.db.Exec("UPDATE notification_exports SET eligible=1 WHERE id=?", final.ID)
	testutil.Must(t, err)
	_, err = f.state.db.Exec("UPDATE notification_obligations SET disposition='waived' WHERE id='long-outage'")
	testutil.Must(t, err)
	check(false)
	_, err = f.state.db.Exec("UPDATE notification_obligations SET disposition='satisfied' WHERE id='long-outage'")
	testutil.Must(t, err)
	check(true)
	// Coalesced historical rows that have never been registered still block success.
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		if err := enqueueNotification(tx, notification.NotificationEvent{ID: period + "/missed", Kind: "daily", Message: "unregistered period"}, f.now.Add(-24*time.Hour)); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE outbox SET covered_by=? WHERE id=?", period, period+"/missed")
		return err
	})
	check(false)
}

func TestSharedDeliveryMonitorStartupAndCorruptState(t *testing.T) {
	f := newPipelineFixture(t)
	c := pipelineMonitorConfig(t, f)
	c.Policy.Notifications.DailyAt = "23:59"
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	testutil.Must(t, f.state.put("daily_started", now))
	calls := 0
	send := func(context.Context, string, bool) error { calls++; return nil }
	testutil.Must(t, publishSharedDailyDeliveryHeartbeat(context.Background(), f.state, c, now, send))
	if calls != 0 {
		t.Fatal("published before first reporting period")
	}
	_, err := f.state.db.Exec("UPDATE meta SET value='broken' WHERE key='daily_started'")
	testutil.Must(t, err)
	if err = publishSharedDailyDeliveryHeartbeat(context.Background(), f.state, c, now, send); err == nil {
		t.Fatal("corrupt state accepted")
	}
	if calls != 0 {
		t.Fatal("contacted monitor with corrupt state")
	}
}

func TestSharedDeliveryMonitorRecoversAfterFailureInAcknowledgedPeriod(t *testing.T) {
	f := newPipelineFixture(t)
	seedPipelineDailyHistory(t, f, 0)
	final := f.freeze(t, 2, 1)[0]
	f.transfer(t, f.client)
	f.accept(t, final)
	f.transfer(t, f.client)
	c := pipelineMonitorConfig(t, f)
	var calls []bool
	send := func(_ context.Context, _ string, failed bool) error { calls = append(calls, failed); return nil }
	publish := func() {
		t.Helper()
		testutil.Must(t, publishSharedDailyDeliveryHeartbeat(context.Background(), f.state, c, f.now, send))
	}
	publish()
	publish()
	f.observe(t, "a", "new pending delivery", f.now)
	// A failure that reached the monitor but lost its reply leaves the recovery
	// latch durable across reopen; no delivery-outage metadata is required.
	err := publishSharedDailyDeliveryHeartbeat(context.Background(), f.state, c, f.now, func(ctx context.Context, path string, failed bool) error {
		testutil.Must(t, send(ctx, path, failed))
		return errors.New("lost failure acknowledgement")
	})
	if err == nil {
		t.Fatal("failure response not lost")
	}
	testutil.Must(t, f.state.Close())
	reopened, err := openBackupState(f.state.directory, false)
	testutil.Must(t, err)
	f.state = reopened
	t.Cleanup(func() { reopened.Close() })
	messages := f.freeze(t, 3, 1)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	f.transfer(t, f.client)
	f.accept(t, messages[0])
	f.transfer(t, f.client)
	publish()
	publish()
	if fmt.Sprint(calls) != "[false true false]" {
		t.Fatal("monitor stayed failed or repeated acknowledgement", calls)
	}
}
