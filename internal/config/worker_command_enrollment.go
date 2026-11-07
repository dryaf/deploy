package config

import (
	"fmt"
	"reflect"
)

// ValidateInitialCommandDeclaration permits command policy and the existing
// Telegram provider's previously unspecified account/bot metadata only.
func ValidateInitialCommandDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Notification.Commands.Enabled || !after.Notification.Commands.Enabled {
		return fmt.Errorf("initial command enrollment requires enabling a command-free declaration")
	}
	if !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("enroll commands separately from backup changes")
	}
	next := *after.Notification
	next.Commands = before.Notification.Commands
	providers := make(map[string]ProviderDeclaration, len(next.Providers))
	for key, p := range next.Providers {
		providers[key] = p
	}
	next.Providers = providers
	old, ok := before.Notification.Providers["telegram"]
	p, present := providers["telegram"]
	if !ok || !present || len(providers) != 1 || len(before.Notification.Providers) != 1 {
		return fmt.Errorf("initial commands require the existing Telegram provider")
	}
	if old.Account != "" && old.Account != p.Account || old.BotName != "" && old.BotName != p.BotName {
		return fmt.Errorf("command enrollment cannot replace the declared Telegram account")
	}
	p.Account, p.BotName = old.Account, old.BotName
	providers["telegram"] = p
	if !reflect.DeepEqual(next, *before.Notification) {
		return fmt.Errorf("enroll commands separately from providers, credentials and other notification settings")
	}
	return nil
}
