package install

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
)

func ValidateManagedEnrolledCommands(m ManagedManifest, before, after Runtime) error {
	return validateManagedEnrolledCommands(m, before, after, false)
}
func ValidateManagedEnrolledCommandPolicy(m ManagedManifest, before, after Runtime) error {
	return validateManagedEnrolledCommands(m, before, after, true)
}
func validateManagedEnrolledCommands(m ManagedManifest, before, after Runtime, update bool) error {
	if m.Mode != "verification" || m.DeliveryOwnerHash != "" || m.RecoveryCommitHash != "" || m.Commands != nil || m.CommandHistoryHash != "" || m.FreshSignal != nil || m.SignalVerification != nil || m.SignalProfiles != nil || before.Bootstrap || after.Bootstrap || !update && before.Telegram != nil || update && before.Telegram == nil || before.Signal != nil || after.Signal != nil || after.Telegram == nil {
		return fmt.Errorf("initial commands require an unrecovered, unactivated Telegram gateway")
	}
	validate := worker.ValidateEnrolledCommandsChange
	if update {
		validate = worker.ValidateEnrolledCommandPolicyChange
		if protocol.Hash(before.Telegram) != protocol.Hash(after.Telegram) {
			return fmt.Errorf("command policy must retain Telegram account and credentials")
		}
	}
	if err := validate(before.Worker, after.Worker); err != nil {
		return err
	}
	if len(after.Providers) != 1 {
		return fmt.Errorf("initial command enrollment requires the existing direct Telegram provider")
	}
	p, ok := after.Providers["telegram"]
	if !ok || p.Kind != "telegram" || p.Conversation != nil || p.TokenFile != after.Telegram.TokenFile {
		return fmt.Errorf("command enrollment must retain the existing alert provider and credential")
	}
	if err := provider.CheckTelegramProfile(p); err != nil {
		return err
	}
	if err := provider.CheckTelegramAccount(*after.Telegram); err != nil {
		return err
	}
	next := before
	next.Worker = after.Worker
	next.Telegram = after.Telegram
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("initial commands cannot replace providers, credentials or LED ownership")
	}
	return nil
}
func MigrateEnrolledCommands(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "enrolled-commands")
}

func MigrateEnrolledCommandPolicy(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "enrolled-command-policy")
}
