package config

import (
	"maps"
	"testing"
)

func TestCommandDeliveryFreshnessDeclarationBounds(t *testing.T) {
	before := DesiredWorkers{Notification: &NotificationDeclaration{Producers: map[string]ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}, MaxSilenceSeconds: 120, MaxEvidenceSeconds: 90000, MaxMessages: 1000}}}, Backup: &BackupDeclaration{}}
	for _, change := range []string{"timing", "no-op", "active-intake", "implicit-producer", "quota", "routes", "health", "new-producer", "backup", "global-limits"} {
		t.Run(change, func(t *testing.T) {
			after := before
			n, b := *before.Notification, *before.Backup
			after.Notification, after.Backup = &n, &b
			n.Producers = maps.Clone(n.Producers)
			p := n.Producers["backup-worker"]
			p.MaxSilenceSeconds, p.MaxEvidenceSeconds = 180, 100000
			switch change {
			case "no-op":
				p = before.Notification.Producers["backup-worker"]
			case "active-intake":
				n.Commands.Enabled = true
			case "implicit-producer":
				delete(n.Producers, "backup-worker")
			case "quota":
				p.MaxMessages++
			case "routes":
				p.Routes = []string{"different"}
			case "health":
				p.PolicyHealth = !p.PolicyHealth
			case "new-producer":
				n.Producers["foreign"] = p
			case "backup":
				b.Freshness = "48h"
			case "global-limits":
				n.Limits.MaxMessages = 900
			}
			if change != "implicit-producer" {
				n.Producers["backup-worker"] = p
			}
			err := ValidateCommandDeliveryFreshnessDeclaration(before, after)
			if (err == nil) != (change == "timing") {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
