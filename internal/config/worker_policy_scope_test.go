package config

import (
	"encoding/json"
	"testing"
)

func TestNotificationScopeSeparatesSpecialistPolicyWithoutDroppingGatewayInputs(t *testing.T) {
	d := DesiredWorkers{Backup: &BackupDeclaration{Schedule: "daily", Freshness: "72h", Notifications: DesiredBackupNotifications{Worker: "manager", Route: "alerts"}}, Notification: &NotificationDeclaration{Providers: ProviderDeclarations{"telegram": {TokenFile: "~/token", ChatIDFile: "~/chat"}}, Routes: map[string]NotificationRoute{"alerts": {Primary: "telegram"}}}}
	baseline, err := d.NotificationFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	full, _ := d.Fingerprint()
	copy := func() DesiredWorkers {
		raw, e := json.Marshal(d)
		if e != nil {
			t.Fatal(e)
		}
		var next DesiredWorkers
		if e = json.Unmarshal(raw, &next); e != nil {
			t.Fatal(e)
		}
		return next
	}
	for _, kind := range []string{"schedule", "retention", "budget", "source", "archive", "monitor"} {
		next := copy()
		switch kind {
		case "schedule":
			next.Backup.Schedule = "hourly"
		case "retention":
			next.Backup.Retention = &RetentionConfig{}
		case "budget":
			next.Backup.MaxSnapshotBytes = 12345
		case "source":
			next.Backup.Sources = []string{"new-source"}
		case "archive":
			next.Backup.Storage.Path = "~/archive"
		case "monitor":
			next.Backup.Monitoring.BackupPingURLFile = "~/monitor"
		}
		got, _ := next.NotificationFingerprint()
		changed, _ := next.Fingerprint()
		if got != baseline || changed == full {
			t.Fatalf("%s lost specialist policy or changed gateway policy", kind)
		}
	}
	for _, kind := range []string{"freshness", "route", "worker", "disabled", "reporting-disabled", "credential", "limit", "led", "commands", "provider", "explicit-producer"} {
		next := copy()
		off := false
		switch kind {
		case "freshness":
			next.Backup.Freshness = "48h"
		case "route":
			next.Backup.Notifications.Route = "different"
		case "worker":
			next.Backup.Notifications.Worker = "other"
		case "disabled":
			next.Backup.Enabled = &off
		case "reporting-disabled":
			next.Backup.Notifications.Enabled = &off
		case "credential":
			p := next.Notification.Providers["telegram"]
			p.TokenFile = "~/other-token"
			next.Notification.Providers["telegram"] = p
		case "limit":
			next.Notification.Limits.MaxMessages = 500
		case "led":
			next.Notification.StatusLEDs.Enabled = true
		case "commands":
			next.Notification.Commands.Enabled = true
		case "provider":
			next.Notification.Providers["signal"] = ProviderDeclaration{}
		case "explicit-producer":
			next.Notification.Producers = map[string]ProducerDeclaration{"backup-worker": {MaxEvidenceSeconds: 100}}
		}
		got, _ := next.NotificationFingerprint()
		if got == baseline {
			t.Fatalf("%s changed gateway policy without changing its pin", kind)
		}
	}
	if baseline == full {
		t.Fatal("scope version was not separated from historical whole-inventory pins")
	}
}

func TestExplicitProducerEvidenceBoundOwnsItsFreshnessScope(t *testing.T) {
	d := DesiredWorkers{Backup: &BackupDeclaration{Freshness: "72h", Notifications: DesiredBackupNotifications{Worker: "manager", Route: "alerts"}}, Notification: &NotificationDeclaration{Producers: map[string]ProducerDeclaration{"backup-worker": {MaxEvidenceSeconds: 100}}}}
	before, _ := d.NotificationFingerprint()
	d.Backup.Freshness = "24h"
	after, _ := d.NotificationFingerprint()
	if before != after {
		t.Fatal("explicit producer bounds still depend on the default backup freshness")
	}
	p := d.Notification.Producers["backup-worker"]
	p.MaxEvidenceSeconds = 200
	d.Notification.Producers["backup-worker"] = p
	after, _ = d.NotificationFingerprint()
	if before == after {
		t.Fatal("changed explicit bound did not change gateway policy")
	}
}
