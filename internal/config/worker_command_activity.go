package config

import (
	"fmt"
	"reflect"
)

func ValidateVerificationCommandActivityDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Notification.Commands.Enabled == after.Notification.Commands.Enabled {
		return fmt.Errorf("change the verification command enabled setting")
	}
	if !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("change command activity separately from backup settings")
	}
	next := *after.Notification
	next.Commands.Enabled = before.Notification.Commands.Enabled
	if !reflect.DeepEqual(next, *before.Notification) {
		return fmt.Errorf("toggle commands separately from permissions, conversations and other settings")
	}
	return nil
}
