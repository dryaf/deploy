//go:build linux

package workerctl

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestWorkerCLICommandRoutingReadThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "command-routes", "read")
}
func TestWorkerCLICommandRoutingMutationThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "command-routes", "mutation")
}
func TestWorkerCLIDeliveryReportRouteThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-report-route")
}
func routingManifest(t *testing.T, read func(*testing.T, string) []byte) install.ManagedManifest {
	t.Helper()
	var j OperationJournal
	testutil.Must(t, json.Unmarshal(read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json"), &j))
	o, err := install.InspectRetainedInstallation(context.Background(), j.Pins.Installations["notification-worker"])
	testutil.Must(t, err)
	return o.Manifest
}
func qualifyRoutingCLI(t *testing.T, root string, write func(string, string), read func(*testing.T, string) []byte) {
	t.Helper()
	m := routingManifest(t, read)
	session, err := install.BeginQualification(context.Background(), m.Root, protocol.Hash(m))
	testutil.Must(t, err)
	write("routing-evidence.log", "synthetic offline routing observations; no live qualification")
	var answers strings.Builder
	for range session.Checks {
		answers.WriteString("passed\n" + root + "/routing-evidence.log\n")
	}
	qualifyNotificationThroughTerminal(t, root, answers.String(), false)
}
func selectReportRoute(policy, route string) string {
	for _, old := range []string{"alerts", "audit"} {
		needle := "notifications: {worker: manager, route: " + old + "}"
		if strings.Contains(policy, needle) {
			return strings.Replace(policy, needle, "notifications: {worker: manager, route: "+route+"}", 1)
		}
	}
	return strings.Replace(policy, "    backup_worker:\n", "    backup_worker:\n      notifications: {worker: manager, route: "+route+"}\n", 1)
}
func routingWorkers(t *testing.T, run func(*testing.T, string, string, bool) map[string]any) {
	t.Helper()
	for _, w := range []string{"backup-worker", "notification-worker"} {
		for _, a := range []string{"apply", "resume", "status", "plan"} {
			run(t, w, a, true)
		}
	}
}
func testCommandRoutingCLI(t *testing.T, root string, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any) {
	const receiver = "manager/.local/state/deploy/backup-worker/state/command-receiver.json"
	const cutover = "manager/.local/state/deploy/backup-worker/state/notification-cutover.json"
	originalReceiver, originalCutover := string(read(t, receiver)), string(read(t, cutover))
	original := routingManifest(t, read)
	input, err := install.ReadRuntime(original.Paths.Config)
	testutil.Must(t, err)
	interaction := protocol.Hash(input.Worker.Interaction)
	policy := strings.Replace(string(read(t, "server.yaml")), "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
	write("server.yaml", policy)
	run(t, "notification-worker", "apply", true)
	policy = strings.Replace(policy, "scope: {whole_worker: true}", "scope: {subjects: [source/source]}", 1)
	policy = strings.Replace(policy, "permissions: [read, incident.manage]", "permissions: [read]", 1)
	write("server.yaml", policy)
	run(t, "notification-worker", "apply", true)
	var before backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, "manager/.local/state/deploy/backup-worker/setup.json"), &before))
	for cycle, entry := range []string{"backup-worker", "backup-worker", "notification-worker", "notification-worker"} {
		switch cycle {
		case 0:
			policy = strings.Replace(policy, "audit: {primary: telegram}", "audit: {primary: telegram}, trace: {primary: telegram}", 1)
		case 1:
			policy = selectReportRoute(policy, "audit")
		case 2:
			policy = strings.Replace(policy, "freshness: 26h", "freshness: 48h", 1)
		case 3:
			policy = selectReportRoute(policy, "alerts")
		}
		write("server.yaml", policy)
		run(t, "notification-worker", "plan", true)
		run(t, entry, "apply", true)
		paused := routingManifest(t, read)
		if paused.Mode != "verification" || paused.Identity != original.Identity || paused.DeliveryOwnerHash != original.DeliveryOwnerHash {
			t.Fatal("routing bypassed qualification or changed owner")
		}
		qualifyRoutingCLI(t, root, write, read)
		routingWorkers(t, run)
	}
	var after backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, "manager/.local/state/deploy/backup-worker/setup.json"), &after))
	if before.WorkerID != after.WorkerID || before.ArchiveID != after.ArchiveID || protocol.Hash(before.Producer) != protocol.Hash(after.Producer) || protocol.Hash(before.SourceKeys) != protocol.Hash(after.SourceKeys) {
		t.Fatal("routing replaced backup identity")
	}
	write("server.yaml", strings.Replace(policy, "commands:\n        enabled: false", "commands:\n        enabled: true", 1))
	run(t, "notification-worker", "apply", true)
	routingWorkers(t, run)
	current := routingManifest(t, read)
	runtime, err := install.ReadRuntime(current.Paths.Config)
	testutil.Must(t, err)
	if protocol.Hash(runtime.Worker.Interaction) != interaction || current.CommandHistoryHash == "" || current.Commands == nil {
		t.Fatal("routing changed receiver enrollment or failed re-enable")
	}
	if string(read(t, receiver)) != originalReceiver || string(read(t, cutover)) != originalCutover {
		t.Fatal("routing rewrote original receiver or handoff")
	}
	run(t, "backup-worker", "activate", false)
}
func testDeliveryReportRouteCLI(t *testing.T, root string, policy func(bool, string), write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any) {
	policy(true, "daily")
	const explicit = "      producers: {backup-worker: {routes: [alerts, audit]}}\n"
	current := strings.Replace(string(read(t, "server.yaml")), "    notification_worker:\n", "    notification_worker:\n      routes: {alerts: {primary: telegram}, audit: {primary: telegram}}\n"+explicit, 1)
	write("server.yaml", current)
	run(t, "backup-worker", "apply", true)
	qualifyRoutingCLI(t, root, write, read)
	routingWorkers(t, run)
	original := routingManifest(t, read)
	handoff := string(read(t, "manager/.local/state/deploy/backup-worker/state/notification-cutover.json"))
	current = selectReportRoute(current, "audit")
	write("server.yaml", current)
	run(t, "backup-worker", "apply", true)
	routingWorkers(t, run)
	if protocol.Hash(routingManifest(t, read)) != protocol.Hash(original) {
		t.Fatal("allowed report selection renewed runtime/qualification")
	}
	current = strings.Replace(current, explicit, "", 1)
	write("server.yaml", current)
	run(t, "notification-worker", "apply", true)
	qualifyRoutingCLI(t, root, write, read)
	routingWorkers(t, run)
	for cycle, route := range []string{"alerts", "audit"} {
		current = selectReportRoute(current, route)
		write("server.yaml", current)
		entry := "notification-worker"
		if cycle == 1 {
			entry = "backup-worker"
		}
		run(t, entry, "apply", true)
		if routingManifest(t, read).Mode != "verification" {
			t.Fatal("default report selection bypassed qualification")
		}
		qualifyRoutingCLI(t, root, write, read)
		routingWorkers(t, run)
	}
	if string(read(t, "manager/.local/state/deploy/backup-worker/state/notification-cutover.json")) != handoff {
		t.Fatal("report selection rewrote original handoff")
	}
}
