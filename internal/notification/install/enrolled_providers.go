package install

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
)

func ValidateManagedEnrolledProviders(m ManagedManifest, before, after Runtime) error {
	// Review the unchanged installation/producer authority independently of the
	// explicitly selected direct Telegram profile replacement.
	if (before.Telegram == nil) != (after.Telegram == nil) {
		return fmt.Errorf("provider update cannot add or remove command intake policy")
	}
	if before.Telegram != nil {
		prior, next := *before.Telegram, *after.Telegram
		next.TokenFile = prior.TokenFile
		if protocol.Hash(prior) != protocol.Hash(next) || before.Providers["telegram"].TokenFile != before.Telegram.TokenFile || after.Providers["telegram"].TokenFile != after.Telegram.TokenFile {
			return fmt.Errorf("retain the Telegram command account and select one token for alerts and intake")
		}
		for _, account := range []provider.TelegramAccount{*before.Telegram, *after.Telegram} {
			if err := provider.CheckTelegramAccount(account); err != nil {
				return err
			}
		}
	}
	same := after
	same.Providers = before.Providers
	same.Telegram = before.Telegram
	if err := validateManagedEnrolledPolicy(m, before, same, "routes"); err != nil {
		return err
	}
	if err := worker.ValidateEnrolledRoutesChange(before.Worker, after.Worker); err != nil {
		return err
	}
	for _, runtime := range []Runtime{before, after} {
		for _, p := range runtime.Providers {
			if p.Kind == "telegram" {
				if err := provider.CheckTelegramProfile(p); err != nil {
					return err
				}
			}
		}
	}
	b, a := before, after
	b.Bootstrap = true
	a.Bootstrap = true
	b.Worker.LEDBindings, a.Worker.LEDBindings = nil, nil
	b.Worker.Producers = nil
	a.Worker.Producers = nil
	// Command policy was compared unchanged above. Review the independent
	// direct-alert profile under its existing bootstrap provider contract.
	b.Telegram, a.Telegram = nil, nil
	b.Worker.Interaction, a.Worker.Interaction = nil, nil
	return ValidateBootstrapProvidersChange(b, a)
}
func MigrateEnrolledProviders(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "enrolled-providers")
}
