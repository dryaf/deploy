package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func validatePausedCommandDeliveryLimits(m ManagedManifest, before, after Runtime) error {
	if m.Mode != "verification" || m.DeliveryOwnerHash == "" || m.DeliveryActivation != nil || m.RecoveryCommitHash != "" || m.Commands != nil || !safefs.DigestPattern.MatchString(m.CommandHistoryHash) || m.SignalVerification != nil || m.SignalProfiles != nil || before.Bootstrap || after.Bootstrap || before.Telegram == nil || after.Telegram == nil || before.Signal != nil || after.Signal != nil || before.Worker.Interaction == nil || after.Worker.Interaction == nil {
		return fmt.Errorf("command budgets require an unrecovered paused Telegram command owner")
	}
	if err := worker.ValidateEnrolledLimitsChange(before.Worker, after.Worker); err != nil {
		return err
	}
	want := before
	want.Worker = after.Worker
	if protocol.Hash(want) != protocol.Hash(after) || protocol.Hash(before) == protocol.Hash(after) {
		return fmt.Errorf("change only notification budgets while preserving command enrollment")
	}
	return nil
}
func (i managedInstaller) probeCommandDeliveryLimits(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.commandLimitsProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-command-limits-probe")
		}
	}
	for _, b := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, b.name, b.hash); err != nil {
			return err
		}
	}
	return nil
}
func ReviewCommandDeliveryLimits(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "command-limits")
}
func ChangePausedCommandDeliveryLimits(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, spec, expected, "command-limits")
}
func InspectPausedCommandDeliveryLimits(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, spec, expected, "command-limits")
}
func ValidatePausedCommandDeliveryLimits(m ManagedManifest, before, after Runtime) error {
	return validatePausedCommandDeliveryLimits(m, before, after)
}
