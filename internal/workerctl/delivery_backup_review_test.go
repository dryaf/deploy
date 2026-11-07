package workerctl

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeliveryPolicyRequiresAppliedBackupContinuation(t *testing.T) {
	before := config.DesiredWorkers{Notification: &config.NotificationDeclaration{}, Backup: &config.BackupDeclaration{Schedule: "daily", Freshness: "26h", Notifications: config.DesiredBackupNotifications{Worker: "manager", Route: "alerts"}}}
	after := before
	b := *before.Backup
	b.Schedule, b.Freshness = "hourly", "48h"
	after.Backup = &b
	scope, err := before.NotificationFingerprint()
	testutil.Must(t, err)
	normalized, err := deliveryBackupBefore(before, after, "derived-freshness")
	testutil.Must(t, err)
	if normalized.Backup.Schedule != "hourly" || normalized.Backup.Freshness != "26h" {
		t.Fatal("lost independent backup update or source deadline")
	}
	testutil.Must(t, validateDeliveryPolicyDeclaration(before, after, "derived-freshness"))
	r := ControlRequest{Desired: after}
	actual := before
	l := deliveryProviderLifecycle{backupReview: func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: "complete", SharedReportingConnected: true}, &actual, nil
	}}
	if err := reviewDeliveryBackup(context.Background(), r, before, scope, "derived-freshness", l); err == nil {
		t.Fatal("unapplied schedule was accepted")
	}
	actual = normalized
	testutil.Must(t, reviewDeliveryBackup(context.Background(), r, before, scope, "derived-freshness", l))
	if err := reviewDeliveryBackup(context.Background(), r, before, scope, "derived-freshness", deliveryProviderLifecycle{}); err == nil {
		t.Fatal("backup history inferred without inspection")
	}
	changed := after
	b = *after.Backup
	b.Notifications.Route = "unreviewed"
	changed.Backup = &b
	if _, err := deliveryBackupBefore(before, changed, "derived-freshness"); err == nil {
		t.Fatal("different dependency normalized away")
	}
	l.backupReview = func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return backup.SetupObservation{Stage: "notification_policy_pending"}, &actual, nil
	}
	if err := reviewDeliveryBackup(context.Background(), r, before, scope, "derived-freshness", l); err == nil {
		t.Fatal("unfinished backup accepted")
	}
}
