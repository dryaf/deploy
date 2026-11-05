//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

type reportAPI struct {
	fixture    *backupEvaluationFixture
	t          *testing.T
	offline    bool
	operations []string
	accepted   []notify.Message
}

func (a *reportAPI) Call(ctx context.Context, r notify.Request, out any) (notify.Identity, error) {
	a.operations = append(a.operations, r.Operation)
	if a.offline {
		return notify.Identity{}, errors.New("fixture gateway unavailable")
	}
	id, err := a.fixture.client.Call(ctx, r, out)
	if err == nil && r.Operation == "message.enqueue" {
		a.fixture.accept(a.t, *r.Message)
		a.accepted = append(a.accepted, *r.Message)
	}
	return id, err
}

func setupReportFixture(t *testing.T) (*backupEvaluationFixture, *reportAPI, backupReportDependencies, *[]string) {
	t.Helper()
	f := newBackupEvaluationFixture(t)
	f.config.Policy.Timezone = "UTC"
	f.config.Policy.Notifications.DailyAt = "00:00"
	paths := []*string{&f.config.Policy.Monitoring.BackupPingURLFile, &f.config.Policy.Monitoring.DailyDeliveryPingURLFile}
	for i, path := range paths {
		name := []string{"backup", "delivery"}[i]
		*path = filepath.Join(t.TempDir(), name)
		testutil.Must(t, os.WriteFile(*path, []byte("https://monitor.example/"+name), 0600))
	}
	owner := backupNotificationOwner{Schema: 1, Mode: "shared", Binding: f.binding, Socket: f.client.Socket}
	// This fixture synthesizes the handoff; production uses the cutover coordinator.
	testutil.Must(t, f.state.put("notification_owner", owner))
	api := &reportAPI{fixture: f, t: t}
	calls := []string{}
	deps := backupReportDependencies{API: api, Clock: func() backupEvaluationClock { return f.clock }, Heartbeat: func(_ context.Context, path string, failed bool) error {
		outcome := "success"
		if failed {
			outcome = "failure"
		}
		calls = append(calls, filepath.Base(path)+"/"+outcome)
		return nil
	}}
	return f, api, deps, &calls
}

func TestSharedReportPassCurrentEvaluationDeliveryAndMonitor(t *testing.T) {
	f, api, deps, calls := setupReportFixture(t)
	testutil.Must(t, runSharedBackupReportWith(context.Background(), f.config, f.state, deps))
	if len(api.accepted) != 1 || api.accepted[0].ReportType != "daily" || !strings.Contains(api.accepted[0].Text, "Current backup evaluation: ok") {
		t.Fatal(api.accepted)
	}
	if strings.Join(*calls, ",") != "backup/success,delivery/success" {
		t.Fatal(*calls)
	}
	testutil.Must(t, runSharedBackupReportWith(context.Background(), f.config, f.state, deps))
	if len(api.accepted) != 1 || len(*calls) != 2 {
		t.Fatal("duplicate report or monitoring acknowledgement", len(api.accepted), *calls)
	}
	var health backupReportHealth
	testutil.Must(t, f.state.get("shared_report_health", &health))
	if !health.Available || health.Binding != f.binding {
		t.Fatal(health)
	}
}

func TestSharedReportOfflineRecoveryKeepsOutageIdentity(t *testing.T) {
	f, api, deps, calls := setupReportFixture(t)
	api.offline = true
	if err := runSharedBackupReportWith(context.Background(), f.config, f.state, deps); err == nil {
		t.Fatal("offline gateway was hidden")
	}
	if strings.Join(*calls, ",") != "backup/success,delivery/failure" {
		t.Fatal("delivery loss suppressed independent backup monitor", *calls)
	}
	var outage deliveryOutage
	testutil.Must(t, f.state.get("delivery_outage", &outage))
	if outage.ID == "" || outage.CatchupID != "catchup/"+outage.ID {
		t.Fatal(outage)
	}
	var health backupReportHealth
	testutil.Must(t, f.state.get("shared_report_health", &health))
	if health.Available {
		t.Fatal(health)
	}
	api.offline = false
	testutil.Must(t, runSharedBackupReportWith(context.Background(), f.config, f.state, deps))
	var waiting deliveryOutage
	testutil.Must(t, f.state.get("delivery_outage", &waiting))
	if waiting.ID != outage.ID {
		t.Fatal("outage replaced before final coverage", waiting)
	}
	testutil.Must(t, runSharedBackupReportWith(context.Background(), f.config, f.state, deps))
	testutil.Must(t, f.state.get("delivery_outage", &waiting))
	if waiting.ID != "" {
		t.Fatal("eligible final catchup did not clear outage")
	}
	if len(api.accepted) != 2 || api.accepted[1].OutageID != outage.ID {
		t.Fatal(api.accepted)
	}
	if strings.Join(*calls, ",") != "backup/success,delivery/failure,delivery/failure,delivery/success" {
		t.Fatal(*calls)
	}
}

func TestSharedReportRefusesUncommittedOrRevokedOwnership(t *testing.T) {
	for _, mode := range []string{"absent", "fenced", "malformed", "binding", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f, api, deps, _ := setupReportFixture(t)
			var owner backupNotificationOwner
			testutil.Must(t, f.state.get("notification_owner", &owner))
			switch mode {
			case "absent":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key='notification_owner'")
				testutil.Must(t, err)
			case "fenced":
				owner.Mode = "fenced"
				testutil.Must(t, f.state.put("notification_owner", owner))
			case "malformed":
				_, err := f.state.db.Exec("UPDATE meta SET value='broken' WHERE key='notification_owner'")
				testutil.Must(t, err)
			case "binding":
				owner.Binding.Generation = "different"
				testutil.Must(t, f.state.put("notification_owner", owner))
			case "revoked":
				testutil.Must(t, f.gateway.Revoke(context.Background(), f.binding.Producer, f.binding.Generation))
			}
			if err := runSharedBackupReportWith(context.Background(), f.config, f.state, deps); err == nil {
				t.Fatal("invalid ownership allowed report pass")
			}
			if len(api.accepted) != 0 {
				t.Fatal("messages submitted without shared authority")
			}
			if mode != "revoked" && len(api.operations) != 0 {
				t.Fatal("API contacted without local authority", api.operations)
			}
		})
	}
}

func TestSharedReportLeaseAndLegacyFence(t *testing.T) {
	f, _, deps, _ := setupReportFixture(t)
	lease, err := backupNotifierLease(f.state)
	testutil.Must(t, err)
	if err = runSharedBackupReportWith(context.Background(), f.config, f.state, deps); err == nil {
		t.Fatal("parallel report acquired notifier lease")
	}
	lease.Close()
	called := false
	send := func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		called = true
		return notification.DeliveryAttempt{}, nil
	}
	if err = drainNotificationOutbox(context.Background(), f.state, f.config, send, f.clock.Now); err == nil || called {
		t.Fatal("legacy sender bypassed fence", err, called)
	}
	// A malformed ownership record must also fence a compatible legacy sender.
	_, err = f.state.db.Exec("UPDATE meta SET value='broken' WHERE key='notification_owner'")
	testutil.Must(t, err)
	if err = requireLegacyNotificationOwner(f.state); err == nil {
		t.Fatal("malformed fence allowed legacy send")
	}
	if err = activateWorker(f.config, f.state); err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatal("legacy activation ignored ownership fence", err)
	}
}

func TestSharedReportEvaluationFailureCannotPublishMonitorSuccess(t *testing.T) {
	f, api, deps, calls := setupReportFixture(t)
	_, err := f.state.db.Exec("UPDATE meta SET value='broken' WHERE key='health_probe'")
	testutil.Must(t, err)
	if err = runSharedBackupReportWith(context.Background(), f.config, f.state, deps); err == nil {
		t.Fatal("broken evaluation succeeded")
	}
	if len(api.operations) != 0 || strings.Join(*calls, ",") != "backup/failure,delivery/failure" {
		t.Fatal(api.operations, *calls)
	}
	var count int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM backup_notification_snapshots").Scan(&count))
	if count != 0 {
		t.Fatal("broken evaluation partially committed")
	}
	var started string
	if err = f.state.db.QueryRow("SELECT value FROM meta WHERE key='daily_started'").Scan(&started); err != sql.ErrNoRows {
		t.Fatal("daily source creation escaped evaluation rollback", err)
	}
}

func TestSharedReportRecoversMonitorAfterEvaluationError(t *testing.T) {
	f, _, deps, calls := setupReportFixture(t)
	testutil.Must(t, runSharedBackupReportWith(context.Background(), f.config, f.state, deps))
	_, err := f.state.db.Exec("UPDATE meta SET value='broken' WHERE key='health_probe'")
	testutil.Must(t, err)
	if err = runSharedBackupReportWith(context.Background(), f.config, f.state, deps); err == nil {
		t.Fatal("bad evaluation succeeded")
	}
	f.refreshProbe(t)
	testutil.Must(t, runSharedBackupReportWith(context.Background(), f.config, f.state, deps))
	if (*calls)[len(*calls)-1] != "delivery/success" {
		t.Fatal("same-day delivery monitor stayed failed", *calls)
	}
}

func TestSharedReportHealthKeepsDeliveryFailureSeparate(t *testing.T) {
	f, _, _, _ := setupReportFixture(t)
	var probe WorkerHealthProbe
	testutil.Must(t, f.state.get("health_probe", &probe))
	probe.Healthy = false
	probe.DeliveryHealthy = false
	probe.BackupHealthy = true
	shared, err := recordSharedBackupHealthProbe(f.state, f.config, probe)
	testutil.Must(t, err)
	if !shared {
		t.Fatal("shared probe not recorded")
	}
	var incidents int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL").Scan(&incidents))
	if incidents != 0 {
		t.Fatal("delivery failure became backup failure")
	}
	var outage deliveryOutage
	testutil.Must(t, f.state.get("delivery_outage", &outage))
	if outage.ID == "" {
		t.Fatal("delivery failure did not retain outage")
	}
	evaluation, _ := f.evaluate(t)
	if evaluation.Snapshot.Subjects[0].Condition != "ok" {
		t.Fatal("delivery failure lowered backup-only health", evaluation)
	}
	probe.BackupHealthy = false
	_, err = recordSharedBackupHealthProbe(f.state, f.config, probe)
	testutil.Must(t, err)
	evaluation, _ = f.evaluate(t)
	if evaluation.Snapshot.Subjects[0].Condition == "ok" {
		t.Fatal("backup failure hidden")
	}
}

func TestSharedReportBatchingExcludesRepresentedSources(t *testing.T) {
	f, _, _, _ := setupReportFixture(t)
	for i := 0; i < 9; i++ {
		f.observe(t, fmt.Sprintf("existing-%d", i), "first group", f.clock.Now)
	}
	f.evaluate(t)
	for i := 0; i < 9; i++ {
		f.observe(t, fmt.Sprintf("new-%d", i), "second group", f.clock.Now)
	}
	ledgerTx(t, f.state, func(tx *sql.Tx) error { return coalesceBackupBacklog(tx, f.clock.Now, true) })
	var covered int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='failure' AND covered_by LIKE 'batch/%'").Scan(&covered))
	if covered != 9 {
		t.Fatal("represented failures entered automatic batch", covered)
	}
}
