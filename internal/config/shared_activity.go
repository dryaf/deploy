package config

import (
	"fmt"
	"reflect"
)

// SharedActivityDisabled uses all three explicit activity controls. A stopped
// dependency stops capture and reporting together; it never selects legacy senders.
func SharedActivityDisabled(d DesiredWorkers) bool {
	return d.Notification == nil || d.Backup == nil || !WorkerEnabled(d.Notification.Enabled) || !WorkerEnabled(d.Backup.Enabled) || !WorkerEnabled(d.Backup.Notifications.Enabled)
}

// ValidateSharedActivityDeclaration allows activity controls only. The dependency,
// runtime policy, credentials, commands and output permission remain identical.
func ValidateSharedActivityDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || before.Backup.Notifications.Worker == "" {
		return fmt.Errorf("shared activity requires retained gateway and backup declarations")
	}
	for name := range before.Notification.Producers {
		if name != "backup-worker" {
			return fmt.Errorf("shared activity requires separate fencing for additional producers")
		}
	}
	want := before
	n, b := *before.Notification, *before.Backup
	want.Notification, want.Backup = &n, &b
	n.Enabled = after.Notification.Enabled
	b.Enabled = after.Backup.Enabled
	b.Notifications.Enabled = after.Backup.Notifications.Enabled
	if !reflect.DeepEqual(want, after) {
		return fmt.Errorf("apply shared activity separately from providers, policy, commands, output and backup changes")
	}
	return nil
}
