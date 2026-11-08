package config

import (
	"fmt"
	"maps"
	"reflect"
)

// Only an existing local producer's timing can change while command authority
// is retained. New producer declarations and access changes remain separate.
func ValidateCommandDeliveryFreshnessDeclaration(before, after DesiredWorkers) error {
	if err := ValidateDeliveryProducerDeclaration(before, after); err != nil {
		return err
	}
	old, had := before.Notification.Producers["backup-worker"]
	next, has := after.Notification.Producers["backup-worker"]
	if !had || !has {
		return fmt.Errorf("retain the existing explicit backup producer declaration before changing command delivery timing")
	}
	next.MaxSilenceSeconds, next.MaxEvidenceSeconds = old.MaxSilenceSeconds, old.MaxEvidenceSeconds
	projected := maps.Clone(after.Notification.Producers)
	projected["backup-worker"] = next
	if !reflect.DeepEqual(projected, before.Notification.Producers) {
		return fmt.Errorf("change only backup producer heartbeat and evidence timing while commands are disabled")
	}
	return nil
}
