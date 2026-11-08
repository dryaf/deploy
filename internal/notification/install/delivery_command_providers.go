package install

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func commandProviderKind(kind string) bool {
	return kind == "command-provider" || kind == "command-destination"
}
func commandPausedPolicy(kind string) bool {
	return commandProviderKind(kind) || kind == "command-limits" || kind == "command-freshness" || kind == "command-producer" || kind == "command-routes" || kind == "command-declaration"
}
func validatePausedCommandDeliveryProviders(m ManagedManifest, before, after Runtime, destination bool) error {
	if m.Mode != "verification" || m.DeliveryOwnerHash == "" || m.DeliveryActivation != nil || m.RecoveryCommitHash != "" || m.Commands != nil || !safefs.DigestPattern.MatchString(m.CommandHistoryHash) || m.SignalVerification != nil || m.SignalProfiles != nil || before.Bootstrap || after.Bootstrap || before.Telegram == nil || after.Telegram == nil || before.Signal != nil || after.Signal != nil || before.Worker.Interaction == nil || after.Worker.Interaction == nil {
		return fmt.Errorf("provider changes require the original disabled unrecovered Telegram command owner")
	}
	if err := worker.ValidateEnrolledRoutesChange(before.Worker, after.Worker); err != nil {
		return err
	}
	old, next := *before.Telegram, *after.Telegram
	next.TokenFile = old.TokenFile
	if protocol.Hash(old) != protocol.Hash(next) {
		return fmt.Errorf("retain the admitted Telegram account/name; a new bot needs receiver admission")
	}
	for _, a := range []provider.TelegramAccount{*before.Telegram, *after.Telegram} {
		if err := provider.CheckTelegramAccount(a); err != nil {
			return err
		}
	}
	if len(before.Providers) != len(after.Providers) {
		return fmt.Errorf("retain provider presence and command reply dependencies")
	}
	changed := false
	for name, old := range before.Providers {
		next, ok := after.Providers[name]
		if !ok {
			return fmt.Errorf("provider removal requires retained reply disposition")
		}
		if name == "telegram" {
			if old.Conversation != nil || next.Conversation != nil || old.TokenFile != before.Telegram.TokenFile || next.TokenFile != after.Telegram.TokenFile {
				return fmt.Errorf("retain command audiences and one token for intake and alerts")
			}
			for _, p := range []provider.Profile{old, next} {
				if err := provider.CheckTelegramProfile(p); err != nil {
					return err
				}
			}
			if (old.Destination != next.Destination) != destination {
				return fmt.Errorf("review same-destination rotation separately from an alert destination change")
			}
			changed = old.TokenFile != next.TokenFile || old.ChatFile != next.ChatFile || old.Destination != next.Destination
			next.TokenFile, next.ChatFile, next.Destination = old.TokenFile, old.ChatFile, old.Destination
		}
		if protocol.Hash(old) != protocol.Hash(next) {
			return fmt.Errorf("provider update changed retained provider authority")
		}
	}
	want := after
	want.Telegram = before.Telegram
	want.Providers = before.Providers
	want.Worker.Routes = before.Worker.Routes
	if !changed || protocol.Hash(want) != protocol.Hash(before) {
		return fmt.Errorf("change only alert references/destination with the original receiver")
	}
	return nil
}
func ValidatePausedCommandDeliveryProviders(m ManagedManifest, b, a Runtime) error {
	return validatePausedCommandDeliveryProviders(m, b, a, false)
}
func ValidatePausedCommandDeliveryDestination(m ManagedManifest, b, a Runtime) error {
	return validatePausedCommandDeliveryProviders(m, b, a, true)
}
func (i managedInstaller) probeCommandDeliveryProviders(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.commandProvidersProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, n, h string) error {
			return probeManagedCapability(ctx, d, n, h, "__managed-command-providers-probe")
		}
	}
	for _, b := range []struct{ n, h string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, b.n, b.h); err != nil {
			return err
		}
	}
	return nil
}
func ReviewCommandDeliveryProviders(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "command-provider")
}
func ReviewCommandDeliveryDestination(ctx context.Context, root, expected string, raw []byte) (DeliveryProviderReview, error) {
	return newManagedInstaller().reviewPausedRuntimeChange(ctx, root, expected, raw, "command-destination")
}
func ChangePausedCommandDeliveryProviders(ctx context.Context, s ManagedSpec, h string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, s, h, "command-provider")
}
func ChangePausedCommandDeliveryDestination(ctx context.Context, s ManagedSpec, h string) (ManagedManifest, error) {
	return newManagedInstaller().changePausedRuntime(ctx, s, h, "command-destination")
}
func InspectPausedCommandDeliveryProviders(ctx context.Context, s ManagedSpec, h string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, s, h, "command-provider")
}
func InspectPausedCommandDeliveryDestination(ctx context.Context, s ManagedSpec, h string) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectPausedRuntimeChange(ctx, s, h, "command-destination")
}
