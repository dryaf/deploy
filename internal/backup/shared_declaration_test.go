package backup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestDeclarativeSharedReportingFencesLegacyButRetainsPendingEvents(t *testing.T) {
	a, state, _, _, _ := archiveFixture(t)
	if err := a.updateControl(state, func(c *ArchiveControl) error { c.SharedRequired = true; return nil }); err != nil {
		t.Fatal(err)
	}
	p := configfixture.WorkerPolicy(t)
	legacy := WorkerInstallation{Policy: p}
	p.Notifications = config.WorkerNotifications{Mode: "shared"}
	p, err := config.NormalizeWorker(p)
	if err != nil {
		t.Fatal(err)
	}
	c := WorkerInstallation{Policy: p}
	if err = state.observeIncident("capture", "worker", "fixture pending incident", time.Now().UTC(), p.Notifications); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err = state.db.QueryRow("SELECT count(*) FROM outbox").Scan(&queued); err != nil || queued != 1 {
		t.Fatal("shared pending evidence lost", queued, err)
	}
	if requireLegacyNotificationOwner(state) == nil {
		t.Fatal("archive pin permitted legacy ownership")
	}
	for _, candidate := range []WorkerInstallation{c, legacy} {
		if requireBackupReporting(state, candidate) == nil {
			t.Fatal("old or shared configuration bypassed fence")
		}
		if err = drainNotificationOutbox(context.Background(), state, candidate, nil, time.Now()); err == nil {
			t.Fatal("legacy sender admitted")
		}
	}
	if err = requireNoNotificationHandoff(state); err != nil {
		t.Fatal("fresh shared state incorrectly claimed a cutover", err)
	}
	units, err := renderWorkerUnits(c, WorkerUnitPaths{Home: "/home/worker", Config: "/home/worker/config/runtime.json", State: "/home/worker/state", Archive: p.Storage.Path, Keys: "/home/worker/keys"}, "example.test/worker@sha256:"+strings.Repeat("a", 64), "")
	if err != nil {
		t.Fatal(err)
	}
	for name := range units {
		if strings.Contains(name, "notifier") || strings.Contains(name, "test-") || strings.Contains(name, "signal") {
			t.Fatal("fresh shared setup installed legacy sender", name)
		}
	}
	p.Notifications.Telegram = legacy.Policy.Notifications.Telegram
	p.Notifications.Primary = "telegram"
	if _, err = config.NormalizeWorker(p); err == nil {
		t.Fatal("shared runtime accepted legacy provider credentials")
	}
}
