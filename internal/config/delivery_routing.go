package config

import "fmt"

func validateCommandRoutingProfile(before, after DesiredWorkers) error {
	for _, d := range []DesiredWorkers{before, after} {
		if d.Notification == nil || d.Notification.Commands.Enabled || len(d.Notification.Providers) != 1 {
			return fmt.Errorf("disable commands and retain the Telegram-only profile before changing routing or derived freshness")
		}
		if _, ok := d.Notification.Providers["telegram"]; !ok {
			return fmt.Errorf("retain the Telegram command profile")
		}
	}
	return nil
}
func ValidateCommandDeliveryRoutesDeclaration(before, after DesiredWorkers) error {
	if err := validateCommandRoutingProfile(before, after); err != nil {
		return err
	}
	return ValidateDeliveryRoutesDeclaration(before, after)
}
func ValidateDeliveryReportRouteDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Notification.Commands.Enabled || after.Notification.Commands.Enabled {
		return fmt.Errorf("disable command intake before selecting a production report route")
	}
	return ValidateSharedReportRouteDeclaration(before, after)
}
func ValidateCommandDeliveryReportRouteDeclaration(before, after DesiredWorkers) error {
	if err := validateCommandRoutingProfile(before, after); err != nil {
		return err
	}
	return ValidateDeliveryReportRouteDeclaration(before, after)
}
func ValidateCommandDerivedFreshnessDeclaration(before, after DesiredWorkers) error {
	if err := validateCommandRoutingProfile(before, after); err != nil {
		return err
	}
	return ValidateDerivedFreshnessDeclaration(before, after)
}
