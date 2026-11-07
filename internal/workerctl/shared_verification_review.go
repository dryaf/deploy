package workerctl

import (
	"context"
	"fmt"
	"reflect"
)

// Check the backup half before retaining a gateway operation. A simultaneous
// unapplied backup-only edit must not strand a policy/mapping/output intent.
func reviewSharedVerificationBackup(ctx context.Context, r ControlRequest, scope string, l notificationRemappingLifecycle) error {
	if l.scopeReview == nil {
		return nil
	}
	observed, actual, err := l.scopeReview(ctx, r.Facts.Home)
	if err != nil {
		return err
	}
	if observed.Stage != "complete" || observed.SharedReportingConnected || actual == nil || !reflect.DeepEqual(actual.Backup, r.Desired.Backup) {
		return fmt.Errorf("finish backup setup and apply backup-only changes before shared policy migration")
	}
	old, err := actual.NotificationFingerprint()
	if err != nil || old != scope {
		return fmt.Errorf("backup declaration has a different gateway policy predecessor")
	}
	return nil
}
