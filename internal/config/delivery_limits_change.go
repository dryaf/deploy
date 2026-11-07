package config

import (
	"fmt"
	"reflect"
)

// Production budgets are changed separately from producers, provider inputs,
// command policy and backup settings; the installer reviews retained capacity.
func ValidateDeliveryLimitsDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || before.Notification.Commands.Enabled || after.Notification.Commands.Enabled || before.Notification.Limits == after.Notification.Limits {
		return fmt.Errorf("change production notification limits with unchanged direct-alert policy")
	}
	copy := after
	n := *after.Notification
	copy.Notification = &n
	n.Limits = before.Notification.Limits
	if !reflect.DeepEqual(copy, before) {
		return fmt.Errorf("change notification limits separately from all other settings")
	}
	return nil
}
