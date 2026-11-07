package backup

import (
	"encoding/json"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestDeclarativeBackupUpdateClassifiesBeforeMutation(t *testing.T) {
	policy := configfixture.WorkerPolicy(t)
	d := SetupDeclaration{Version: 1, UID: 1000, User: "backup", Home: "/home/backup", Architecture: "arm64"}
	before := WorkerInstallation{Policy: policy}
	request := WorkerSetupRequest{Policy: policy, Declaration: &d}
	for _, alias := range policy.Sources {
		before.Sources = append(before.Sources, source.WorkerSource{Alias: alias, Host: "example.com", User: "app", Port: 22})
		request.Sources = append(request.Sources, SetupSource{Alias: alias, Host: "example.com", User: "app", Port: 22})
	}
	for _, tc := range []struct {
		name    string
		allowed bool
		change  func(*WorkerSetupRequest)
	}{
		{"same", true, func(*WorkerSetupRequest) {}},
		{"schedule", true, func(r *WorkerSetupRequest) { r.Policy.Schedule = "daily"; r.Policy.Freshness = "26h" }},
		{"retention", true, func(r *WorkerSetupRequest) { r.Policy.Retention = &config.RetentionConfig{KeepLast: 12} }},
		{"capacity", true, func(r *WorkerSetupRequest) { r.Policy.Storage.MinFreeBytes *= 2 }},
		{"timeouts", true, func(r *WorkerSetupRequest) { r.Policy.Timeouts.Transfer = "11m" }},
		{"report-period", true, func(r *WorkerSetupRequest) { r.Policy.Notifications.RepeatAfter = "8h" }},
		{"archive", false, func(r *WorkerSetupRequest) { r.Policy.Storage.Path = "/other/archive" }},
		{"sources", false, func(r *WorkerSetupRequest) { r.Policy.Sources = append(r.Policy.Sources, "other") }},
		{"endpoint", false, func(r *WorkerSetupRequest) { r.Sources[0].Host = "other.example.com" }},
		{"reporting", false, func(r *WorkerSetupRequest) { r.Policy.Notifications.Mode = "disabled" }},
		{"provider", false, func(r *WorkerSetupRequest) {
			r.Policy.Notifications.Telegram = &config.WorkerTelegram{TokenFile: "/other/token", ChatIDFile: "/other/chat"}
		}},
		{"monitor", true, func(r *WorkerSetupRequest) { r.Policy.Monitoring.BackupPingURLFile = "/other/ping" }},
		{"delivery-monitor", true, func(r *WorkerSetupRequest) { r.Policy.Monitoring.DailyDeliveryPingURLFile = "/other/delivery" }},
		{"remove-monitor", true, func(r *WorkerSetupRequest) { r.Policy.Monitoring = config.WorkerMonitoring{} }},
		{"target", false, func(r *WorkerSetupRequest) { r.Declaration.Home = "/home/other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(request)
			var next WorkerSetupRequest
			if err := json.Unmarshal(raw, &next); err != nil {
				t.Fatal(err)
			}
			tc.change(&next)
			if err := validateDeclarativeBackupUpdate(before, &d, next); (err == nil) != tc.allowed {
				t.Fatal(tc.name, err)
			}
		})
	}
	if err := validateDeclarativeBackupUpdate(before, nil, request); err == nil {
		t.Fatal("legacy installation adopted implicitly")
	}
}
