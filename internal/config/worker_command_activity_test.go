package config

import "testing"

func TestVerificationCommandActivityRequiresUnchangedPolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		before := DesiredWorkers{Notification: &NotificationDeclaration{Commands: CommandDeclaration{Enabled: enabled, Mode: "read"}}}
		n := *before.Notification
		n.Commands.Enabled = !enabled
		after := DesiredWorkers{Notification: &n}
		if err := ValidateVerificationCommandActivityDeclaration(before, after); err != nil {
			t.Fatal(err)
		}
		n.Commands.Mode = "mutation"
		if ValidateVerificationCommandActivityDeclaration(before, after) == nil {
			t.Fatal("combined mode change accepted")
		}
	}
}
