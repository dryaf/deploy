//go:build linux

package workerctl

import (
	"context"
	"encoding/json"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
	"strings"
	"testing"
)

func TestWorkerCLISharedActivitySignalOnlyThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-signal")
}

func TestWorkerCLISharedActivityThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "shared-activity")
}
func TestWorkerCLISharedCommandActivityReadThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "command-shared-activity", "read")
}
func TestWorkerCLISharedCommandActivityMutationThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "command-shared-activity", "mutation")
}

func sharedActivityCLIQualification(t *testing.T, fixtureRoot string, write func(string, string), read func(*testing.T, string) []byte) int {
	t.Helper()
	var j OperationJournal
	testutil.Must(t, json.Unmarshal(read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json"), &j))
	root := j.Pins.Installations["notification-worker"]
	o, err := install.InspectRetainedInstallation(context.Background(), root)
	testutil.Must(t, err)
	hash := protocol.Hash(o.Manifest)
	session, err := install.BeginQualification(context.Background(), root, hash)
	testutil.Must(t, err)
	write("shared-staged-activity-evidence.log", "synthetic offline staged restoration observations")
	var answers strings.Builder
	for range session.Checks {
		answers.WriteString("passed\n" + fixtureRoot + "/shared-staged-activity-evidence.log\n")
	}
	qualifyNotificationThroughTerminal(t, fixtureRoot, answers.String(), false)

	return len(session.Checks)
}
func testSharedActivityCLI(t *testing.T, root string, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, checks int, commands bool) {
	const control = "manager/.local/state/deploy/worker-control/notification-worker/operation.json"
	const handoff = "manager/.local/state/deploy/backup-worker/state/notification-cutover.json"
	const receiver = "manager/.local/state/deploy/backup-worker/state/command-receiver.json"
	original := string(read(t, "server.yaml"))
	if commands {
		// Enabled receiver admission cannot be combined with dependency disable.
		write("server.yaml", strings.Replace(original, "    backup_worker:\n", "    backup_worker:\n      enabled: false\n", 1))
		journal := string(read(t, control))
		run(t, "notification-worker", "apply", false)
		if journal != string(read(t, control)) {
			t.Fatal("active command refusal retained activity")
		}
		original = strings.Replace(original, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
		write("server.yaml", original)
		run(t, "notification-worker", "apply", true)
	}
	run(t, "backup-worker", "apply", true)
	originalHandoff := string(read(t, handoff))
	originalReceiver := ""
	if commands {
		originalReceiver = string(read(t, receiver))
	}
	write("shared-activity-evidence.log", "synthetic offline shared activity evidence, not live qualification")
	for cycle, field := range []string{"gateway", "backup", "reporting"} {
		disabled := original
		switch field {
		case "gateway":
			disabled = strings.Replace(original, "    notification_worker:\n", "    notification_worker:\n      enabled: false\n", 1)
		case "backup":
			disabled = strings.Replace(original, "    backup_worker:\n", "    backup_worker:\n      enabled: false\n", 1)
		case "reporting":
			disabled = strings.Replace(original, "    backup_worker:\n", "    backup_worker:\n      notifications: {enabled: false}\n", 1)
		}
		if disabled == original {
			t.Fatal("activity fixture field missing", field)
		}
		write("server.yaml", disabled)
		worker := "notification-worker"
		if cycle%2 == 1 {
			worker = "backup-worker"
		}
		v := run(t, worker, "apply", true)
		if v["state"] != "disabled" {
			t.Fatal("shared disable did not stop", v)
		}
		for _, w := range []string{"notification-worker", "backup-worker"} {
			for _, a := range []string{"apply", "resume", "status", "plan"} {
				run(t, w, a, true)
			}
		}
		// Refuse policy changes while disabled without replacing its receipt.
		before := string(read(t, control))
		write("server.yaml", strings.Replace(disabled, `schedule: "daily"`, `schedule: "hourly"`, 1))
		run(t, "notification-worker", "apply", false)
		if before != string(read(t, control)) {
			t.Fatal("disabled policy edit retained a new operation")
		}
		write("server.yaml", original)
		v = run(t, worker, "apply", true)
		if v["state"] != "needs_qualification" {
			t.Fatal("shared restore bypassed qualification", v)
		}
		run(t, "backup-worker", "activate", false)
		// The terminal route exercises the coordinator while its backup fence stays closed.
		var answers strings.Builder
		for range checks {
			answers.WriteString("passed\n" + root + "/shared-activity-evidence.log\n")
		}
		qualifyNotificationThroughTerminal(t, root, answers.String(), false)
		v = run(t, worker, "resume", true)
		if v["state"] != "reporting_connected" {
			t.Fatal("shared activity did not resume reporting", v)
		}
		for _, w := range []string{"notification-worker", "backup-worker"} {
			for _, a := range []string{"apply", "resume", "status", "plan"} {
				run(t, w, a, true)
			}
		}
		if originalHandoff != string(read(t, handoff)) || commands && originalReceiver != string(read(t, receiver)) {
			t.Fatal("activity replaced reporting/receiver authority")
		}
	}
	if commands {
		enabled := strings.Replace(original, "commands:\n        enabled: false", "commands:\n        enabled: true", 1)
		write("server.yaml", enabled)
		v := run(t, "notification-worker", "apply", true)
		if v["state"] != "commands_admitted" {
			t.Fatal("explicit command restoration failed", v)
		}
		if originalReceiver != string(read(t, receiver)) {
			t.Fatal("command restoration replaced receiver")
		}
		for _, worker := range []string{"backup-worker", "notification-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, worker, action, true)
			}
		}
	}
}
