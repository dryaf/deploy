package config

import "testing"

func TestInitialCommandDeclarationPreservesUnrelatedPolicy(t *testing.T) {
	before := DesiredWorkers{Notification: &NotificationDeclaration{Providers: ProviderDeclarations{"telegram": {TokenFile: "~/.config/token", ChatIDFile: "~/.config/chat"}}}}
	makeAfter := func() DesiredWorkers {
		n := *before.Notification
		n.Providers = ProviderDeclarations{"telegram": before.Notification.Providers["telegram"]}
		p := n.Providers["telegram"]
		p.Account, p.BotName = "123", "test_bot"
		n.Providers["telegram"] = p
		n.Commands = CommandDeclaration{Enabled: true, Mode: "read"}
		return DesiredWorkers{Notification: &n}
	}
	if err := ValidateInitialCommandDeclaration(before, makeAfter()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"token", "chat", "provider", "limit", "already-enabled", "known-account"} {
		t.Run(kind, func(t *testing.T) {
			original := before
			n := *before.Notification
			original.Notification = &n
			original.Notification.Providers = ProviderDeclarations{"telegram": before.Notification.Providers["telegram"]}
			after := makeAfter()
			p := after.Notification.Providers["telegram"]
			switch kind {
			case "token":
				p.TokenFile = "~/.config/another"
			case "chat":
				p.ChatIDFile = "~/.config/another"
			case "provider":
				after.Notification.Providers["signal"] = ProviderDeclaration{}
			case "limit":
				after.Notification.Limits.MaxMessages++
			case "already-enabled":
				original.Notification.Commands.Enabled = true
			case "known-account":
				old := original.Notification.Providers["telegram"]
				old.Account = "456"
				original.Notification.Providers["telegram"] = old
			}
			after.Notification.Providers["telegram"] = p
			if ValidateInitialCommandDeclaration(original, after) == nil {
				t.Fatal("unrelated change accepted")
			}
		})
	}
}
