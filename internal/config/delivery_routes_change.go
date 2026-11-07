package config

import (
	"fmt"
	"reflect"
)

// Route changes retain provider and producer declarations. Runtime review
// requires a settled queue before changing delivery paths.
func ValidateDeliveryRoutesDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || before.Notification.Commands.Enabled || after.Notification.Commands.Enabled || reflect.DeepEqual(before.Notification.Routes, after.Notification.Routes) {
		return fmt.Errorf("change production routes with unchanged direct-alert ownership")
	}
	copy := after
	n := *after.Notification
	copy.Notification = &n
	n.Routes = before.Notification.Routes
	if !reflect.DeepEqual(copy, before) {
		return fmt.Errorf("change routes separately from other worker settings")
	}
	return nil
}
