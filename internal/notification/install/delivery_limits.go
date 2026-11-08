package install

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func isPausedRuntimeChange(operation string) bool {
	return operation == "rotate-command-delivery-provider" || operation == "change-command-delivery-destination" || operation == "change-command-delivery-routes" || operation == "change-command-delivery-producer" || operation == "change-command-delivery-freshness" || operation == "change-command-delivery-limits" || operation == "rotate-delivery-provider" || operation == "change-delivery-limits" || operation == "change-delivery-producer" || operation == "change-delivery-routes" || operation == "change-delivery-destination" || operation == "change-delivery-provider-enrollment"
}
func pausedRuntimeOperation(kind string) string {
	switch kind {
	case "":
		return "rotate-delivery-provider"
	case "command-provider":
		return "rotate-command-delivery-provider"
	case "command-destination":
		return "change-command-delivery-destination"
	case "command-routes":
		return "change-command-delivery-routes"
	case "command-producer":
		return "change-command-delivery-producer"
	case "command-freshness":
		return "change-command-delivery-freshness"
	case "command-limits":
		return "change-command-delivery-limits"
	case "limits":
		return "change-delivery-limits"
	case "provider-enrollment":
		return "change-delivery-provider-enrollment"
	case "destination":
		return "change-delivery-destination"
	case "routes":
		return "change-delivery-routes"
	case "producer":
		return "change-delivery-producer"
	}
	return ""
}
func validatePausedRuntimeChange(m ManagedManifest, before, after Runtime, kind string) error {
	if kind == "command-routes" {
		return validatePausedCommandDeliveryRoutes(m, before, after)
	}
	if commandProviderKind(kind) {
		return validatePausedCommandDeliveryProviders(m, before, after, kind == "command-destination")
	}
	if kind == "command-producer" {
		return validatePausedCommandDeliveryProducerPolicy(m, before, after)
	}
	if kind == "command-declaration" {
		return ValidateCommandDeliveryDeclaration(m, before, after)
	}
	if kind == "command-freshness" {
		return validatePausedCommandDeliveryFreshness(m, before, after)
	}
	if kind == "command-limits" {
		return validatePausedCommandDeliveryLimits(m, before, after)
	}
	if kind == "" {
		return ValidateDeliveryProviderRotation(m, before, after)
	}
	if (kind != "limits" && kind != "producer" && kind != "routes" && kind != "destination" && kind != "provider-enrollment" && kind != "declaration") || m.Mode != "verification" || m.DeliveryOwnerHash == "" || m.DeliveryActivation != nil || m.RecoveryCommitHash != "" || m.Commands != nil || m.CommandHistoryHash != "" || m.SignalVerification != nil || m.SignalProfiles != nil || before.Bootstrap || after.Bootstrap || before.Telegram != nil || after.Telegram != nil || before.Signal != nil || after.Signal != nil || before.Worker.Interaction != nil || after.Worker.Interaction != nil {
		return fmt.Errorf("production policy changes require an unrecovered paused direct-alert owner")
	}
	if kind == "declaration" {
		if protocol.Hash(before) != protocol.Hash(after) {
			return fmt.Errorf("declaration continuation changed effective runtime")
		}
		return nil
	}
	if kind == "provider-enrollment" {
		return validateDeliveryProviderPair(before, after, true)
	}
	if kind == "destination" {
		return validateDeliveryDestinationPair(before, after)
	}
	validate := worker.ValidateEnrolledLimitsChange
	if kind == "routes" {
		validate = worker.ValidateEnrolledRoutesChange
	}
	if kind == "producer" {
		validate = worker.ValidateEnrolledProducerPolicyChange
	}
	if err := validate(before.Worker, after.Worker); err != nil {
		return err
	}
	want := before
	want.Worker = after.Worker
	if protocol.Hash(want) != protocol.Hash(after) || protocol.Hash(before) == protocol.Hash(after) {
		return fmt.Errorf("change only the selected notification policy")
	}
	return nil
}
func ReviewDeliveryLimits(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "limits")
}
func ChangePausedDeliveryLimits(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, spec, expected, "limits")
}
func InspectPausedDeliveryLimits(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, spec, expected, "limits")
}
func (i managedInstaller) probePausedRuntimeChange(ctx context.Context, d *safefs.Directory, m ManagedManifest, kind string) error {
	if commandProviderKind(kind) {
		return i.probeCommandDeliveryProviders(ctx, d, m)
	}
	if kind == "command-routes" {
		return i.probeCommandDeliveryRoutes(ctx, d, m)
	}
	if kind == "command-producer" || kind == "command-declaration" {
		return i.probeCommandDeliveryProducerPolicy(ctx, d, m)
	}
	if kind == "command-freshness" {
		return i.probeCommandDeliveryFreshness(ctx, d, m)
	}
	if kind == "command-limits" {
		return i.probeCommandDeliveryLimits(ctx, d, m)
	}
	if kind == "" {
		return i.probeDeliveryProviders(ctx, d, m)
	}
	if kind != "limits" && kind != "producer" && kind != "routes" && kind != "destination" && kind != "provider-enrollment" {
		return fmt.Errorf("unknown paused runtime capability")
	}
	probe := i.deliveryLimitsProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-delivery-limits-probe")
		}
	}
	for _, b := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, b.name, b.hash); err != nil {
			return err
		}
	}
	return nil
}

func ValidatePausedDeliveryLimits(m ManagedManifest, before, after Runtime) error {
	return validatePausedRuntimeChange(m, before, after, "limits")
}

func ReviewDeliveryProducerPolicy(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "producer")
}
func ChangePausedDeliveryProducerPolicy(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, spec, expected, "producer")
}
func InspectPausedDeliveryProducerPolicy(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, spec, expected, "producer")
}
func ValidatePausedDeliveryProducerPolicy(m ManagedManifest, before, after Runtime) error {
	return validatePausedRuntimeChange(m, before, after, "producer")
}
func preflightDeliveryPolicy(ctx context.Context, m ManagedManifest, before, after Runtime, kind string) error {
	if commandProviderKind(kind) {
		return worker.PreflightCommandDeliveryProviders(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker, m.CommandHistoryHash)
	}
	if kind == "command-routes" {
		return worker.PreflightCommandDeliveryRoutes(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker, m.CommandHistoryHash)
	}
	if kind == "command-producer" {
		return worker.PreflightCommandDeliveryProducerPolicy(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker, m.CommandHistoryHash)
	}
	if kind == "command-declaration" {
		return worker.PreflightCommandDeliveryDeclaration(ctx, m.Paths.State, m.Identity, before.Worker, m.CommandHistoryHash)
	}
	if kind == "command-freshness" {
		return worker.PreflightCommandDeliveryFreshness(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker, m.CommandHistoryHash)
	}
	if kind == "command-limits" {
		return worker.PreflightCommandDeliveryLimits(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker, m.CommandHistoryHash)
	}
	if kind == "declaration" {
		return worker.PreflightDeliveryDeclaration(ctx, m.Paths.State, m.Identity, before.Worker)
	}
	if kind == "destination" || kind == "provider-enrollment" {
		return worker.PreflightDeliveryProviders(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker)
	}
	if kind == "routes" {
		return worker.PreflightDeliveryRoutes(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker)
	}
	if kind == "producer" {
		return worker.PreflightDeliveryProducerPolicy(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker)
	}
	return worker.PreflightDeliveryLimits(ctx, m.Paths.State, m.Identity, before.Worker, after.Worker)
}

func ReviewDeliveryRoutes(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "routes")
}
func ChangePausedDeliveryRoutes(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, spec, expected, "routes")
}
func InspectPausedDeliveryRoutes(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, spec, expected, "routes")
}
func ValidatePausedDeliveryRoutes(m ManagedManifest, before, after Runtime) error {
	return validatePausedRuntimeChange(m, before, after, "routes")
}

func ValidatePausedDeliveryDeclaration(m ManagedManifest, before, after Runtime) error {
	return validatePausedRuntimeChange(m, before, after, "declaration")
}
func ReviewDeliveryDeclaration(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "declaration")
}
