package workerctl

import (
	"context"
	"fmt"
	"reflect"

	"github.com/dryaf/deploy/internal/config"
)

// Notification fingerprints intentionally exclude backup-only policy. Rebase
// only those fields, then separately prove they were applied to the backup.
func deliveryBackupBefore(before, after config.DesiredWorkers, kind string) (config.DesiredWorkers, error) {
	if before.Backup == nil || after.Backup == nil {
		if !reflect.DeepEqual(before.Backup, after.Backup) {
			return before, fmt.Errorf("retain the local backup dependency")
		}
		return before, nil
	}
	next := before
	b := *after.Backup
	if derivedFreshnessPolicy(kind) {
		b.Freshness = before.Backup.Freshness
	}
	if reportRoutePolicy(kind) {
		b.Notifications.Route = before.Backup.Notifications.Route
	}
	next.Backup = &b
	old, err := before.NotificationFingerprint()
	if err != nil {
		return before, err
	}
	hash, err := next.NotificationFingerprint()
	if err != nil || hash != old {
		return before, fmt.Errorf("apply backup dependency changes separately from notification policy")
	}
	return next, nil
}

func reviewDeliveryBackup(ctx context.Context, r ControlRequest, before config.DesiredWorkers, scope, kind string, l deliveryProviderLifecycle) error {
	want, err := deliveryBackupBefore(before, r.Desired, kind)
	if err != nil {
		return err
	}
	if l.backupReview == nil {
		if !reflect.DeepEqual(want.Backup, before.Backup) {
			return fmt.Errorf("completed backup policy inspection unavailable")
		}
		return nil
	}
	observed, actual, err := l.backupReview(ctx, r.Facts.Home)
	if err != nil {
		return err
	}
	if observed.Stage != "complete" || !observed.SharedReportingConnected || actual == nil || !reflect.DeepEqual(actual.Backup, want.Backup) {
		return fmt.Errorf("finish and apply backup-only changes before notification policy")
	}
	hash, err := actual.NotificationFingerprint()
	if err != nil || hash != scope {
		return fmt.Errorf("backup declaration lost the notification policy predecessor")
	}
	return nil
}
