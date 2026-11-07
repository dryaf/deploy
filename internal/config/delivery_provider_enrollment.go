package config

import (
	"fmt"
	"reflect"
)

// Add or remove unused direct Telegram while retaining the exact Signal owner.
func ValidateDeliveryProviderEnrollmentDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || before.Notification.Commands.Enabled || after.Notification.Commands.Enabled {
		return fmt.Errorf("provider enrollment requires a local command-free backup gateway")
	}
	b, a := before.Notification.Providers, after.Notification.Providers
	oldSignal, hadSignal := b["signal"]
	nextSignal, hasSignal := a["signal"]
	if !hadSignal || !hasSignal || !reflect.DeepEqual(oldSignal, nextSignal) {
		return fmt.Errorf("retain the exact owned Signal profile")
	}
	oldTelegram, hadTelegram := b["telegram"]
	nextTelegram, hasTelegram := a["telegram"]
	if hadTelegram == hasTelegram {
		return fmt.Errorf("change only Telegram provider presence")
	}
	for _, profiles := range []ProviderDeclarations{b, a} {
		for name := range profiles {
			if name != "signal" && name != "telegram" {
				return fmt.Errorf("unsupported provider enrollment")
			}
		}
	}
	if oldTelegram.Account != "" || oldTelegram.BotName != "" || oldTelegram.Conversation != "" || nextTelegram.Account != "" || nextTelegram.BotName != "" || nextTelegram.Conversation != "" {
		return fmt.Errorf("retain command and explicit account declarations")
	}
	if hasTelegram && (nextTelegram.TokenFile == "" || nextTelegram.ChatIDFile == "" || nextTelegram.TokenInput != "" || nextTelegram.ChatIDInput != "") {
		return fmt.Errorf("select private target-side Telegram files")
	}
	copy := after
	n := *after.Notification
	copy.Notification = &n
	n.Providers = b
	if !reflect.DeepEqual(copy, before) {
		return fmt.Errorf("change provider enrollment separately from routes and other settings")
	}
	return nil
}
