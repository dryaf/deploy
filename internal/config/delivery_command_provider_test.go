package config

import (
	"maps"
	"testing"
)

func TestCommandProviderDeclarationRetainsAuthority(t *testing.T) {
	for _, change := range []string{"token", "chat", "active", "metadata", "permissions", "remove", "signal", "backup", "limits"} {
		t.Run(change, func(t *testing.T) {
			b := DesiredWorkers{Notification: &NotificationDeclaration{Providers: ProviderDeclarations{"telegram": {TokenFile: "/private/token", ChatIDFile: "/private/chat", Account: "123", BotName: "Bot"}}, Commands: CommandDeclaration{Mode: "read"}}, Backup: &BackupDeclaration{Freshness: "26h"}}
			a := b
			n, backup := *b.Notification, *b.Backup
			a.Notification, a.Backup = &n, &backup
			n.Providers = maps.Clone(n.Providers)
			p := n.Providers["telegram"]
			p.TokenFile = "/private/new-token"
			switch change {
			case "chat":
				p.ChatIDFile = "/private/new-chat"
			case "active":
				n.Commands.Enabled = true
			case "metadata":
				p.Account = "999"
			case "permissions":
				n.Commands.Permissions = []PermissionDeclaration{{Actor: "new"}}
			case "remove":
				delete(n.Providers, "telegram")
			case "signal":
				n.Providers["signal"] = ProviderDeclaration{AccountFile: "/private/account", RecipientFile: "/private/recipient"}
			case "backup":
				backup.Schedule = "weekly"
			case "limits":
				n.Limits.MaxMessages = 500
			}
			if change != "remove" {
				n.Providers["telegram"] = p
			}
			err := ValidateCommandDeliveryProviderDeclaration(b, a)
			if (err == nil) != (change == "token" || change == "chat") {
				t.Fatal("unexpected provider bound", err)
			}
		})
	}
}
func TestSignalReferenceDeclarationRetainsAccountServiceAndArtifact(t *testing.T) {
	old := ProviderDeclaration{AccountFile: "/private/account", RecipientFile: "/private/recipient", Account: "same-account", Socket: "/service/api.sock", Data: "/account", Executable: "/bin/native", ExecutableSHA256: "same-artifact"}
	for _, change := range []string{"refs", "account", "socket", "data", "artifact", "operator", "no-op"} {
		t.Run(change, func(t *testing.T) {
			next := old
			next.RecipientFile = "/private/next-recipient"
			switch change {
			case "account":
				next.Account = "different"
			case "socket":
				next.Socket = "/other/api.sock"
			case "data":
				next.Data = "/other/account"
			case "artifact":
				next.ExecutableSHA256 = "different"
			case "operator":
				next.RecipientInput = "local"
			case "no-op":
				next = old
			}
			if err := ValidateSignalReferenceChange(old, next); (err == nil) != (change == "refs") {
				t.Fatal("unexpected native reference bound", err)
			}
		})
	}
}

func TestSignalReferenceUpdateRetainsTelegramOperatorInputs(t *testing.T) {
	before := DesiredWorkers{Notification: &NotificationDeclaration{Providers: ProviderDeclarations{"telegram": {TokenInput: "local-token", ChatIDInput: "local-chat"}, "signal": {AccountFile: "/private/account", RecipientFile: "/private/recipient"}}}, Backup: &BackupDeclaration{Freshness: "26h"}}
	after := before
	n := *before.Notification
	after.Notification = &n
	n.Providers = maps.Clone(n.Providers)
	p := n.Providers["signal"]
	p.RecipientFile = "/private/new-recipient"
	n.Providers["signal"] = p
	if err := ValidateDeliveryProviderDeclaration(before, after); err != nil {
		t.Fatal("unchanged Telegram aliases blocked Signal update", err)
	}
	p = n.Providers["telegram"]
	p.TokenFile = "/private/new-token"
	p.ChatIDFile = "/private/new-chat"
	p.TokenInput, p.ChatIDInput = "", ""
	n.Providers["telegram"] = p
	if err := ValidateDeliveryProviderDeclaration(before, after); err == nil {
		t.Fatal("mixed Signal/Telegram rotation accepted")
	}
}
