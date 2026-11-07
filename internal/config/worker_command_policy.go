package config

import (
	"fmt"
	"reflect"
)

// An unactivated gateway can revise the policy it will later submit for
// receiver review. This does not change credentials or admit provider intake.
func ValidateVerificationCommandDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || !before.Notification.Commands.Enabled || !after.Notification.Commands.Enabled {
		return fmt.Errorf("verification command changes require enabled declarations; admission remains separate")
	}
	if !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("change commands separately from backup settings")
	}
	next := *after.Notification
	next.Commands = before.Notification.Commands
	if !reflect.DeepEqual(next, *before.Notification) {
		return fmt.Errorf("change commands separately from providers and other notification settings")
	}
	if reflect.DeepEqual(before.Notification.Commands, after.Notification.Commands) {
		return fmt.Errorf("command declaration is unchanged")
	}
	return nil
}
