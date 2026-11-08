package config

import (
	"fmt"
	"reflect"
)

// ValidateDeliveryProviderDeclaration limits a production change to target-side
// Telegram or owned Signal private-file references. Provider identity is additionally
// checked from those files by the installer before it retains any operation.
func ValidateDeliveryProviderDeclaration(before, after DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("change delivery credentials separately from backup policy")
	}
	b, a := before.Notification, after.Notification
	if b.Commands.Enabled || a.Commands.Enabled || len(b.Providers) != len(a.Providers) {
		return fmt.Errorf("delivery credential rotation requires unchanged direct-alert providers without commands")
	}
	changed := false
	signalChanged := false
	telegramChanged := false
	for name, old := range b.Providers {
		next, ok := a.Providers[name]
		if !ok {
			return fmt.Errorf("delivery credential rotation cannot replace provider identities")
		}
		if name == "telegram" && !reflect.DeepEqual(old, next) {
			if next.TokenInput != "" || next.ChatIDInput != "" || next.TokenFile == "" || next.ChatIDFile == "" {
				return fmt.Errorf("select private target-side Telegram token_file and chat_id_file references")
			}
			telegramChanged = old.TokenFile != next.TokenFile || old.ChatIDFile != next.ChatIDFile || old.TokenInput != "" || old.ChatIDInput != ""
			changed = changed || telegramChanged
			next.TokenFile, next.ChatIDFile, next.TokenInput, next.ChatIDInput = old.TokenFile, old.ChatIDFile, old.TokenInput, old.ChatIDInput
		}
		if name == "signal" && !reflect.DeepEqual(old, next) {
			if err := ValidateSignalReferenceChange(old, next); err != nil {
				return err
			}
			signalChanged, changed = true, true
			next = old
		}
		if !reflect.DeepEqual(old, next) {
			return fmt.Errorf("credential rotation cannot change provider or audience policy")
		}
	}
	copy := *a
	copy.Providers = b.Providers
	if !changed || signalChanged && telegramChanged || !reflect.DeepEqual(copy, *b) {
		return fmt.Errorf("change one provider's private-file references separately")
	}
	return nil
}

// ValidateSignalReferenceChange admits private reference selection only. The
// native service proves that the selected account still owns its original link.
func ValidateSignalReferenceChange(old, next ProviderDeclaration) error {
	if reflect.DeepEqual(old, next) {
		return fmt.Errorf("Signal references unchanged")
	}
	if next.AccountFile == "" || next.RecipientFile == "" || next.AccountInput != "" || next.RecipientInput != "" {
		return fmt.Errorf("select private target-side Signal account_file and recipient_file references")
	}
	next.AccountFile, next.RecipientFile, next.AccountInput, next.RecipientInput = old.AccountFile, old.RecipientFile, old.AccountInput, old.RecipientInput
	if !reflect.DeepEqual(old, next) {
		return fmt.Errorf("retain Signal account, service, inbox and executable; use their separate lifecycle for ownership changes")
	}
	return nil
}
