package backup

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestDisabledBackupReportingRetainsIncidentsWithoutQueueOrWake(t *testing.T) {
	a, state, _, _, _ := archiveFixture(t)
	if err := a.updateControl(state, func(c *ArchiveControl) error { c.ReportingDisabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	p := config.WorkerNotifications{Mode: "disabled"}
	now := time.Now().UTC()
	for _, at := range []time.Time{now, now.Add(8 * time.Hour)} {
		if err := state.observeIncident("capture", "worker", "fixture failure", at, p); err != nil {
			t.Fatal(err)
		}
	}
	var incidents, queue int
	if err := state.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL").Scan(&incidents); err != nil || incidents != 1 {
		t.Fatal(incidents, err)
	}
	if err := state.resolveIncident("capture", "worker", now.Add(9*time.Hour), p); err != nil {
		t.Fatal(err)
	}
	c := WorkerInstallation{Policy: config.BackupWorkerConfig{Timezone: "UTC", Notifications: p}}
	if err := state.enqueueDaily(c, now, "fixture daily report"); err != nil {
		t.Fatal(err)
	}
	if err := state.db.QueryRow("SELECT count(*) FROM outbox").Scan(&queue); err != nil || queue != 0 {
		t.Fatal("disabled reporting queued messages", queue, err)
	}
	if err := state.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NOT NULL").Scan(&incidents); err != nil || incidents != 1 {
		t.Fatal("incident history lost", incidents, err)
	}
	if _, err := os.Stat(filepath.Join(state.directory, "urgent")); !os.IsNotExist(err) {
		t.Fatal("disabled reporting woke notifier", err)
	}
	// Even a caller using an old provider-enabled config cannot bypass the archive pin.
	c.Policy = configfixture.WorkerPolicy(t)
	if err := drainNotificationOutbox(context.Background(), state, c, nil, now); err == nil {
		t.Fatal("sender admitted despite disabled archive pin")
	}
	if err := runNotifier(context.Background(), c, state); err == nil {
		t.Fatal("notifier admitted despite disabled archive pin")
	}
}

func TestDisabledBackupReportingPolicyUnitsAndEvidence(t *testing.T) {
	p := configfixture.WorkerPolicy(t)
	p.Notifications = config.WorkerNotifications{Mode: "disabled"}
	p.Monitoring = config.WorkerMonitoring{}
	var err error
	p, err = config.NormalizeWorker(p)
	if err != nil {
		t.Fatal(err)
	}
	c := WorkerInstallation{Policy: p}
	units, err := renderWorkerUnits(c, WorkerUnitPaths{Home: "/home/backup", Config: "/home/backup/config/runtime.json", State: "/home/backup/state", Archive: p.Storage.Path, Keys: "/home/backup/keys"}, "example.test/worker@sha256:"+strings.Repeat("a", 64), "")
	if err != nil {
		t.Fatal(err)
	}
	for name := range units {
		if strings.Contains(name, "notifier") || strings.Contains(name, "test-") || strings.Contains(name, "signal") {
			t.Fatal("unexpected delivery unit", name)
		}
	}
	for _, unit := range workerPolicyActivationUnits(c, false) {
		if _, ok := units[unit]; !ok {
			t.Fatal("activation enables an absent unit", unit)
		}
	}
	gates := workerQualificationGates(c, true)
	if slices.Contains(gates, "notification_delivery") || slices.Contains(gates, "external_monitor") || !slices.Contains(gates, "storage_durability") || !slices.Contains(gates, "application_restore") {
		t.Fatal("wrong selected evidence", gates)
	}
	if !slices.Equal(workerQualificationChecks(c), []string{"storage_crash_recovery", "isolated_application_restore"}) {
		t.Fatal("disabled feature still requires evidence")
	}
	p.Notifications.Mode = ""
	if _, err = config.NormalizeWorker(p); err == nil {
		t.Fatal("legacy provider requirement weakened")
	}
	p.Notifications.Mode = "disabled"
	p.Monitoring.BackupPingURLFile = "/home/backup/ping"
	if _, err = config.NormalizeWorker(p); err == nil {
		t.Fatal("silently ignored monitor")
	}
}

func TestDisabledBackupReportingHealthDoesNotInventDelivery(t *testing.T) {
	a, state, c, _ := healthyWorkerFixture(t)
	if err := a.updateControl(state, func(control *ArchiveControl) error { control.ReportingDisabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	c.Policy.Notifications = config.WorkerNotifications{Mode: "disabled"}
	c.Policy.Monitoring = config.WorkerMonitoring{}
	c.Policy.Storage.Mode, c.Policy.Storage.RequireMount, c.Policy.Storage.MinFreeBytes = "home", "", 1
	bin := t.TempDir()
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	script := "#!/bin/sh\nprintf 'LoadState=loaded\nActiveState=active\nResult=success\nUnitFileState=enabled\nNextElapseUSecRealtime=tomorrow\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := collectWorkerHealth(c, a.dir.File().Name(), state)
	if !p.Healthy || !p.BackupHealthy || p.DeliveryHealthy || !p.ReportingDisabled {
		t.Fatal("disabled delivery changed backup health or claimed provider health", p)
	}
	for unit := range p.Units {
		if strings.Contains(unit, "notifier") || strings.Contains(unit, "report") {
			t.Fatal("disabled delivery service was required", unit)
		}
	}
}
