package config

import (
	"maps"
	"testing"
)

func TestCommandDeliveryProducerDeclarationBounds(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		for _, change := range []string{"quota", "required", "routes", "health", "context", "explicit", "derived", "active", "permissions", "provider", "backup", "limits", "new-identity", "signal", "no-op"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				before := DesiredWorkers{Notification: &NotificationDeclaration{Commands: CommandDeclaration{Mode: mode}, Providers: ProviderDeclarations{"telegram": {}}, Producers: map[string]ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}, MaxMessages: 1000, MaxSilenceSeconds: 120, MaxEvidenceSeconds: 93600}}}, Backup: &BackupDeclaration{Freshness: "26h"}}
				after := before
				n, b := *before.Notification, *before.Backup
				after.Notification, after.Backup = &n, &b
				n.Producers = maps.Clone(n.Producers)
				p := n.Producers["backup-worker"]
				switch change {
				case "quota":
					p.MaxMessages--
				case "required":
					off := false
					p.Required = &off
				case "routes":
					p.Routes = []string{"alerts", "audit"}
				case "health":
					p.PolicyHealth = true
				case "context":
					p.InteractiveContext = true
					p.ContextKinds = []string{"incident"}
				case "explicit":
					before.Notification = &NotificationDeclaration{Commands: before.Notification.Commands, Providers: before.Notification.Providers}
				case "derived":
					n.Producers = nil
				default:
					p.MaxMessages--
				}
				if change != "derived" {
					n.Producers["backup-worker"] = p
				}
				switch change {
				case "active":
					n.Commands.Enabled = true
				case "permissions":
					n.Commands.Permissions = []PermissionDeclaration{{Actor: "new-actor"}}
				case "provider":
					n.Providers = ProviderDeclarations{"telegram": {TokenFile: "/private/new"}}
				case "backup":
					b.Schedule = "weekly"
				case "limits":
					n.Limits.MaxMessages = 500
				case "signal":
					n.Providers = ProviderDeclarations{"telegram": {}, "signal": {}}
				case "new-identity":
					n.Producers["foreign"] = p
				case "no-op":
					n.Producers = before.Notification.Producers
				}
				allowed := change == "quota" || change == "required" || change == "routes" || change == "health" || change == "context" || change == "explicit" || change == "derived"
				if err := ValidateCommandDeliveryProducerDeclaration(before, after); (err == nil) != allowed {
					t.Fatalf("unexpected producer policy classification: %v", err)
				}
			})
		}
	}
}
