package config

import (
	"fmt"
	"reflect"
)

// ValidateDerivedFreshnessDeclaration changes the backup deadline that also
// supplies the gateway's default evidence deadline. Explicit producer policy is
// a separate update and must not accidentally acquire this migration authority.
func ValidateDerivedFreshnessDeclaration(before, after DesiredWorkers) error {
	if before.Backup == nil || after.Backup == nil || before.Notification == nil || after.Notification == nil || before.Backup.Freshness == after.Backup.Freshness || !WorkerEnabled(before.Backup.Enabled) || !WorkerEnabled(before.Backup.Notifications.Enabled) || before.Backup.Notifications.Worker == "" {
		return fmt.Errorf("derived freshness requires an enabled shared backup and a changed deadline")
	}
	if _, explicit := before.Notification.Producers["backup-worker"]; explicit {
		return fmt.Errorf("explicit producer timing does not derive from backup freshness; apply the backup policy separately")
	}
	next := after
	b := *after.Backup
	next.Backup = &b
	b.Freshness = before.Backup.Freshness
	if !reflect.DeepEqual(before, next) {
		return fmt.Errorf("change derived backup freshness separately from other worker settings")
	}
	return nil
}
