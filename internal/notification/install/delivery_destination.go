package install

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
)

func validateDeliveryDestinationPair(before, after Runtime) error {
	return validateDeliveryProviderPair(before, after, false)
}
func validateDeliveryProviderPair(before, after Runtime, enrollment bool) error {
	if err := worker.ValidateEnrolledRoutesChange(before.Worker, after.Worker); err != nil {
		return err
	}
	if !enrollment && len(before.Providers) != len(after.Providers) {
		return fmt.Errorf("change destination with provider enrollment unchanged")
	}
	changed := false
	for name, old := range before.Providers {
		next, ok := after.Providers[name]
		if !ok {
			if enrollment && old.Kind == "telegram" {
				changed = true
				continue
			}
			return fmt.Errorf("retain provider names")
		}
		if old.Kind == "signal" && protocol.Hash(old) != protocol.Hash(next) {
			if err := validateSignalDeliveryProfileChange(old, next, true); err != nil {
				return err
			}
			changed = changed || old.Destination != next.Destination
		}
		if old.Kind == "telegram" {
			for _, p := range []provider.Profile{old, next} {
				if err := provider.CheckTelegramProfile(p); err != nil {
					return err
				}
			}
			changed = changed || old.Destination != next.Destination
		}
	}
	if enrollment {
		old, had := before.Providers["telegram"]
		next, has := after.Providers["telegram"]
		if had == has {
			return fmt.Errorf("change only Telegram presence")
		}
		if had {
			if err := provider.CheckTelegramProfile(old); err != nil {
				return err
			}
		}
		if has {
			if err := provider.CheckTelegramProfile(next); err != nil {
				return err
			}
		}
		changed = true
	}
	if !changed {
		return fmt.Errorf("destination migration requires a changed Telegram account or destination")
	}
	b, a := before, after
	b.Bootstrap = true
	a.Bootstrap = true
	b.Worker.Producers = nil
	a.Worker.Producers = nil
	b.Worker.LEDBindings = nil
	a.Worker.LEDBindings = nil
	if err := ValidateBootstrapProvidersChange(b, a); err != nil {
		return err
	}
	// Bootstrap review strips only enrollment and LED bindings; all other runtime fields stay exact.
	if protocol.Hash(before.Worker.Producers) != protocol.Hash(after.Worker.Producers) || protocol.Hash(before.Worker.LEDBindings) != protocol.Hash(after.Worker.LEDBindings) {
		return fmt.Errorf("retain producer and LED policy")
	}
	return nil
}
func ValidatePausedDeliveryDestination(m ManagedManifest, before, after Runtime) error {
	return validatePausedRuntimeChange(m, before, after, "destination")
}
func ReviewDeliveryDestination(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "destination")
}
func ChangePausedDeliveryDestination(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, spec, expected, "destination")
}
func InspectPausedDeliveryDestination(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, spec, expected, "destination")
}

func ValidatePausedDeliveryProviderEnrollment(m ManagedManifest, before, after Runtime) error {
	return validatePausedRuntimeChange(m, before, after, "provider-enrollment")
}
func ReviewDeliveryProviderEnrollment(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "provider-enrollment")
}
func ChangePausedDeliveryProviderEnrollment(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, spec, expected, "provider-enrollment")
}
func InspectPausedDeliveryProviderEnrollment(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, spec, expected, "provider-enrollment")
}
