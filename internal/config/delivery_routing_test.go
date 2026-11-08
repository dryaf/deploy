package config

import (
	"maps"
	"testing"
)

func TestProductionRoutingDeclarationBounds(t *testing.T) {
	for _, family := range []string{"routes", "report-route", "freshness"} {
		for _, damage := range []string{"none", "active", "provider", "receiver", "mixed", "missing-route", "missing-permission", "explicit-freshness"} {
			t.Run(family+"/"+damage, func(t *testing.T) {
				before := DesiredWorkers{Notification: &NotificationDeclaration{Providers: ProviderDeclarations{"telegram": {}}, Routes: map[string]NotificationRoute{"alerts": {Primary: "telegram"}, "audit": {Primary: "telegram"}}, Commands: CommandDeclaration{Mode: "mutation"}}, Backup: &BackupDeclaration{Freshness: "26h", Notifications: DesiredBackupNotifications{Worker: "manager", Route: "alerts"}}}
				after := before
				n, b := *before.Notification, *before.Backup
				after.Notification, after.Backup = &n, &b
				n.Routes = maps.Clone(n.Routes)
				switch family {
				case "routes":
					n.Routes["extra"] = NotificationRoute{Primary: "telegram"}
				case "report-route":
					b.Notifications.Route = "audit"
				case "freshness":
					b.Freshness = "48h"
				}
				validate := ValidateCommandDeliveryRoutesDeclaration
				if family == "report-route" {
					validate = ValidateCommandDeliveryReportRouteDeclaration
				}
				if family == "freshness" {
					validate = ValidateCommandDerivedFreshnessDeclaration
				}
				switch damage {
				case "active":
					n.Commands.Enabled = true
				case "provider":
					n.Providers = ProviderDeclarations{"telegram": {}, "signal": {}}
				case "receiver":
					n.Commands.Mode = "read"
				case "mixed":
					b.Schedule = "weekly"
				case "missing-route":
					if family != "report-route" {
						return
					}
					b.Notifications.Route = "missing"
				case "missing-permission":
					if family != "report-route" {
						return
					}
					before.Notification.Producers = map[string]ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}}}
					n.Producers = before.Notification.Producers
				case "explicit-freshness":
					if family != "freshness" {
						return
					}
					before.Notification.Producers = map[string]ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}}}
					n.Producers = before.Notification.Producers
				}
				if err := validate(before, after); (err == nil) != (damage == "none") {
					t.Fatal("unexpected routing admission", err)
				}
			})
		}
	}
}
