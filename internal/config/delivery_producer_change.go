package config

import (
	"fmt"
	"reflect"
)

// Existing producer declarations may revise timing, quotas and access. Runtime
// review retains identities and requires a settled queue for access changes.
func ValidateDeliveryProducerDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || before.Notification.Commands.Enabled || after.Notification.Commands.Enabled || reflect.DeepEqual(before.Notification.Producers, after.Notification.Producers) {
		return fmt.Errorf("change production producer policy with unchanged direct-alert ownership")
	}
	copy := after
	n := *after.Notification
	copy.Notification = &n
	n.Producers = before.Notification.Producers
	if !reflect.DeepEqual(copy, before) {
		return fmt.Errorf("change producer policy separately from other worker settings")
	}
	return nil
}
