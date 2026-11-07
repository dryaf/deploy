package config

import (
	"fmt"
	"reflect"
	"slices"
)

// A staged backup may select another existing gateway route. Provider changes,
// route definitions and explicit producer permissions have separate reviews.
func ValidateSharedReportRouteDeclaration(before, after DesiredWorkers) error {
	if before.Backup == nil || after.Backup == nil || before.Notification == nil || after.Notification == nil || !WorkerEnabled(before.Backup.Enabled) || !WorkerEnabled(before.Backup.Notifications.Enabled) || before.Backup.Notifications.Worker == "" || before.Backup.Notifications.Route == after.Backup.Notifications.Route {
		return fmt.Errorf("shared report route requires an enabled shared backup and a changed route")
	}
	if producer, explicit := before.Notification.Producers["backup-worker"]; explicit && !slices.Contains(producer.Routes, after.Backup.Notifications.Route) {
		return fmt.Errorf("apply explicit producer route permission before selecting that backup reporting route")
	}
	route := after.Backup.Notifications.Route
	if _, exists := before.Notification.Routes[route]; !exists || !ValidInventoryName(route) {
		return fmt.Errorf("declare and apply the destination notification route before selecting it for backup reports")
	}
	next := after
	b := *after.Backup
	next.Backup = &b
	b.Notifications.Route = before.Backup.Notifications.Route
	if !reflect.DeepEqual(before, next) {
		return fmt.Errorf("change the shared backup reporting route separately from other worker settings")
	}
	return nil
}
