package config

import (
	"fmt"
	"reflect"
)

// ValidateDeliveryProviderDeclaration limits a production change to target-side
// Telegram private-file references. Bot and destination identity are additionally
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
	for name, old := range b.Providers {
		next, ok := a.Providers[name]
		if !ok {
			return fmt.Errorf("delivery credential rotation cannot replace provider identities")
		}
		if name == "telegram" {
			if next.TokenInput != "" || next.ChatIDInput != "" || next.TokenFile == "" || next.ChatIDFile == "" {
				return fmt.Errorf("select private target-side Telegram token_file and chat_id_file references")
			}
			changed = old.TokenFile != next.TokenFile || old.ChatIDFile != next.ChatIDFile || old.TokenInput != "" || old.ChatIDInput != ""
			next.TokenFile, next.ChatIDFile, next.TokenInput, next.ChatIDInput = old.TokenFile, old.ChatIDFile, old.TokenInput, old.ChatIDInput
		}
		if !reflect.DeepEqual(old, next) {
			return fmt.Errorf("credential rotation cannot change provider or audience policy")
		}
	}
	copy := *a
	copy.Providers = b.Providers
	if !changed || !reflect.DeepEqual(copy, *b) {
		return fmt.Errorf("change only Telegram private-file references")
	}
	return nil
}
