package config

import "fmt"

// Compilation must independently prove that this declaration edit leaves the
// effective runtime unchanged. Only one existing policy group may be written.
func ValidateDeliveryDeclarationChange(before, after DesiredWorkers) error {
	for _, validate := range []func(DesiredWorkers, DesiredWorkers) error{ValidateDeliveryLimitsDeclaration, ValidateDeliveryProducerDeclaration, ValidateDeliveryRoutesDeclaration} {
		if validate(before, after) == nil {
			return nil
		}
	}
	return fmt.Errorf("record one unchanged effective notification policy at a time")
}
