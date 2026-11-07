package config

import "testing"

func TestVerificationCommandDeclarationPreservesOtherSettings(t *testing.T) {
	before := DesiredWorkers{Notification: &NotificationDeclaration{Commands: CommandDeclaration{Enabled: true, Mode: "read"}, Providers: ProviderDeclarations{"telegram": {TokenFile: "token", Account: "123"}}}}
	for _, kind := range []string{"mode", "unchanged", "disable", "provider", "limits", "backup"} {
		t.Run(kind, func(t *testing.T) {
			n := *before.Notification
			after := DesiredWorkers{Notification: &n}
			after.Notification.Commands.Mode = "mutation"
			switch kind {
			case "unchanged":
				after.Notification.Commands.Mode = "read"
			case "disable":
				after.Notification.Commands.Enabled = false
			case "provider":
				after.Notification.Providers = ProviderDeclarations{"telegram": {TokenFile: "other", Account: "123"}}
			case "limits":
				after.Notification.Limits.MaxMessages++
			case "backup":
				after.Backup = &BackupDeclaration{}
			}
			err := ValidateVerificationCommandDeclaration(before, after)
			if (err == nil) != (kind == "mode") {
				t.Fatal(kind, err)
			}
		})
	}
}
