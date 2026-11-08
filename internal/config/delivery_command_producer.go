package config

import "fmt"

// An explicit declaration configures the existing reserved backup producer. It
// cannot enroll another identity or change receiver policy while intake is off.
func ValidateCommandDeliveryProducerDeclaration(before, after DesiredWorkers) error {
	if err := ValidateDeliveryProducerDeclaration(before, after); err != nil {
		return err
	}
	if len(before.Notification.Providers) != 1 {
		return fmt.Errorf("command producer policy requires the retained Telegram-only profile")
	}
	if _, ok := before.Notification.Providers["telegram"]; !ok {
		return fmt.Errorf("command producer policy requires the retained Telegram profile")
	}
	for _, n := range []*NotificationDeclaration{before.Notification, after.Notification} {
		for name := range n.Producers {
			if name != "backup-worker" {
				return fmt.Errorf("retain the existing local backup producer; additional identities need enrollment")
			}
		}
	}
	return nil
}
