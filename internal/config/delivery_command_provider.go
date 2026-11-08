package config

import "fmt"

// Alert credentials may change without granting another command bot or audience.
func ValidateCommandDeliveryProviderDeclaration(before, after DesiredWorkers) error {
	if err := ValidateDeliveryProviderDeclaration(before, after); err != nil {
		return err
	}
	for _, d := range []DesiredWorkers{before, after} {
		if len(d.Notification.Providers) != 1 {
			return fmt.Errorf("command provider changes require the retained Telegram-only gateway; native intake needs separate admission")
		}
		if d.Notification.Commands.Enabled {
			return fmt.Errorf("disable commands and apply before provider changes")
		}
		if _, ok := d.Notification.Providers["telegram"]; !ok {
			return fmt.Errorf("retain the admitted Telegram command profile; retirement requires receiver and reply disposition")
		}
	}
	return nil
}
