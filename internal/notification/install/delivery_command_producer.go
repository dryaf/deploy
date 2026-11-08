package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func validatePausedCommandDeliveryProducerPolicy(m ManagedManifest, before, after Runtime) error {
	if m.Mode != "verification" || m.DeliveryOwnerHash == "" || m.DeliveryActivation != nil || m.RecoveryCommitHash != "" || m.Commands != nil || !safefs.DigestPattern.MatchString(m.CommandHistoryHash) || m.SignalVerification != nil || m.SignalProfiles != nil || before.Bootstrap || after.Bootstrap || before.Telegram == nil || after.Telegram == nil || before.Signal != nil || after.Signal != nil || before.Worker.Interaction == nil || after.Worker.Interaction == nil {
		return fmt.Errorf("command producer policy requires an unrecovered paused Telegram command owner")
	}
	if err := worker.ValidateEnrolledProducerPolicyChange(before.Worker, after.Worker); err != nil {
		return err
	}
	want := before
	want.Worker = after.Worker
	if protocol.Hash(want) != protocol.Hash(after) || protocol.Hash(before) == protocol.Hash(after) {
		return fmt.Errorf("change only producer policy while preserving command enrollment")
	}
	return nil
}
func (i managedInstaller) probeCommandDeliveryProducerPolicy(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.commandProducerProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-command-producer-probe")
		}
	}
	for _, b := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, b.name, b.hash); err != nil {
			return err
		}
	}
	return nil
}
func ReviewCommandDeliveryProducerPolicy(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "command-producer")
}
func ChangePausedCommandDeliveryProducerPolicy(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, spec, expected, "command-producer")
}
func InspectPausedCommandDeliveryProducerPolicy(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, spec, expected, "command-producer")
}
func ValidatePausedCommandDeliveryProducerPolicy(m ManagedManifest, before, after Runtime) error {
	return validatePausedCommandDeliveryProducerPolicy(m, before, after)
}

func ValidateCommandDeliveryDeclaration(m ManagedManifest, before, after Runtime) error {
	if m.Mode != "verification" || m.DeliveryOwnerHash == "" || m.DeliveryActivation != nil || m.RecoveryCommitHash != "" || m.Commands != nil || !safefs.DigestPattern.MatchString(m.CommandHistoryHash) || m.SignalVerification != nil || m.SignalProfiles != nil || before.Bootstrap || after.Bootstrap || before.Telegram == nil || after.Telegram == nil || before.Signal != nil || after.Signal != nil || before.Worker.Interaction == nil || after.Worker.Interaction == nil || protocol.Hash(before) != protocol.Hash(after) {
		return fmt.Errorf("command declaration must retain the exact unrecovered paused-capable Telegram runtime and receiver")
	}
	return nil
}
func ReviewCommandDeliveryDeclaration(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "command-declaration")
}
