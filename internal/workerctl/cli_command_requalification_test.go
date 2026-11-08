//go:build linux

package workerctl

import (
	"fmt"
	"strings"
	"testing"
)

func testCommandRequalificationCLI(t *testing.T, root string, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, checks int, kind string) {
	const receiver = "manager/.local/state/deploy/backup-worker/state/command-receiver.json"
	const cutover = "manager/.local/state/deploy/backup-worker/state/notification-cutover.json"
	originalReceiver, originalCutover := string(read(t, receiver)), string(read(t, cutover))
	enabled := string(read(t, "server.yaml"))
	disabled := strings.Replace(enabled, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
	if enabled == disabled {
		t.Fatal("missing command declaration")
	}
	write("command-renewal-evidence.log", "synthetic offline observations, not live qualification")
	for cycle := range 2 {
		write("server.yaml", disabled)
		run(t, "notification-worker", "apply", true)
		if kind == "command-freshness" {
			old := fmt.Sprintf("max_silence_seconds: %d, max_evidence_seconds: %d", 120+cycle*10, 90000+cycle*100)
			next := fmt.Sprintf("max_silence_seconds: %d, max_evidence_seconds: %d", 130+cycle*10, 90100+cycle*100)
			enabled = strings.Replace(enabled, old, next, 1)
			disabled = strings.Replace(enabled, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
			write("server.yaml", disabled)
			run(t, "notification-worker", "plan", true)
			run(t, "notification-worker", "apply", true)
		}
		if kind == "command-limits" {
			enabled = strings.Replace(enabled, "      limits: {max_messages: 1100}\n", "", 1)
			enabled = strings.Replace(enabled, "    notification_worker:\n", fmt.Sprintf("    notification_worker:\n      limits: {max_messages: %d}\n", 1100+cycle*100), 1)
			disabled = strings.Replace(enabled, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
			write("server.yaml", disabled)
			run(t, "notification-worker", "apply", true)
		}
		var answers strings.Builder
		for range checks {
			answers.WriteString("passed\n" + root + "/command-renewal-evidence.log\n")
		}
		qualifyNotificationThroughTerminal(t, root, answers.String(), false)
		for _, worker := range []string{"notification-worker", "backup-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, worker, action, true)
			}
		}
		if cycle == 0 {
			enabled = strings.Replace(enabled, "scope: {whole_worker: true}", "scope: {subjects: [source/source]}", 1)
			enabled = strings.Replace(enabled, "permissions: [read, incident.manage]", "permissions: [read]", 1)
			disabled = strings.Replace(enabled, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
			write("server.yaml", disabled)
			for _, worker := range []string{"notification-worker", "backup-worker"} {
				for _, action := range []string{"apply", "resume", "status", "plan"} {
					run(t, worker, action, true)
				}
			}
		}
		write("server.yaml", enabled)
		for _, worker := range []string{"notification-worker", "backup-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, worker, action, true)
			}
		}
	}
	if string(read(t, receiver)) != originalReceiver || string(read(t, cutover)) != originalCutover {
		t.Fatal("qualification renewal rewrote original reporting or receiver ownership")
	}
}
