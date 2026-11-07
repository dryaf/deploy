//go:build linux

package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
	"golang.org/x/crypto/ssh"
)

func testDeliveryDerivedFreshnessCLI(t *testing.T, root string, policy func(bool, string), write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, port, movedPort int, addition, combined bool) {
	ctx := context.Background()
	policy(true, "daily")
	base := string(read(t, "server.yaml"))
	run(t, "backup-worker", "apply", true)
	const control = "manager/.local/state/deploy/worker-control/notification-worker/"
	const setup = "manager/.local/state/deploy/backup-worker/setup.json"
	var operation OperationJournal
	testutil.Must(t, json.Unmarshal(read(t, control+"operation.json"), &operation))
	gateway := operation.Pins.Installations["notification-worker"]
	qualify := func() {
		t.Helper()
		o, err := install.InspectInstallation(ctx, gateway)
		testutil.Must(t, err)
		session, err := install.BeginQualification(ctx, gateway, protocol.Hash(o.Manifest))
		testutil.Must(t, err)
		write("evidence.log", "offline fixture observations; no live provider")
		var answers strings.Builder
		for range session.Checks {
			answers.WriteString("passed\n" + root + "/evidence.log\n")
		}
		qualifyNotificationThroughTerminal(t, root, answers.String(), false)
	}
	qualify()
	run(t, "notification-worker", "apply", true)
	run(t, "backup-worker", "apply", true)
	original, err := install.InspectInstallation(ctx, gateway)
	testutil.Must(t, err)
	var before backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &before))
	entries := []string{"notification-worker", "backup-worker"}
	if addition {
		entries = entries[:1]
	}
	for cycle, entry := range entries {
		deadline := fmt.Sprintf("%dh", 48+24*cycle)
		next := strings.Replace(base, "freshness: 26h", "freshness: "+deadline, 1)
		write("server.yaml", next)
		run(t, entry, "plan", true)
		result := run(t, entry, "apply", true)
		if entry == "notification-worker" && result["state"] != "needs_qualification" || entry == "backup-worker" && result["state"] != "qualification_required" {
			t.Fatal("freshness bypassed qualification", result)
		}
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, "backup-worker", action, true)
		}
		paused, err := install.InspectInstallation(ctx, gateway)
		testutil.Must(t, err)
		if paused.Manifest.Mode != "verification" || paused.Manifest.Identity != original.Manifest.Identity || paused.Manifest.DeliveryOwnerHash != original.Manifest.DeliveryOwnerHash {
			t.Fatal("freshness changed delivery ownership")
		}
		qualify()
		run(t, "notification-worker", "apply", true)
		for _, worker := range []string{"backup-worker", "notification-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, worker, action, true)
			}
		}
		var after backup.WorkerSetupJournal
		testutil.Must(t, json.Unmarshal(read(t, setup), &after))
		if after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.Producer, before.Producer) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || after.Declaration.Desired.Backup.Freshness != deadline {
			t.Fatal("freshness replaced backup ownership or lost declaration")
		}
		var runtime backup.WorkerInstallation
		testutil.Must(t, json.Unmarshal(read(t, "manager/.config/deploy/backup-worker/runtime.json"), &runtime))
		if runtime.Policy.Freshness != deadline {
			t.Fatal("backup runtime did not follow YAML")
		}
	}
	if addition {
		testSharedSourceAdditionCLI(t, root, write, read, run, qualify, combined)
		return
	}
	// An ordinary backup update can follow shared policy history, and another
	// shared update can follow it without replacing the original handoff.
	for _, field := range []string{"schedule", "budget"} {
		next := string(read(t, "server.yaml"))
		if field == "schedule" {
			next = strings.Replace(next, "schedule: \"daily\"", "schedule: \"hourly\"", 1)
		} else {
			next = strings.Replace(next, "max_snapshot_bytes: 1048576", "max_snapshot_bytes: 2097152", 1)
		}
		write("server.yaml", next)
		run(t, "backup-worker", "plan", true)
		run(t, "backup-worker", "apply", true)
		for _, worker := range []string{"backup-worker", "notification-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, worker, action, true)
			}
		}
	}
	write("server.yaml", strings.Replace(string(read(t, "server.yaml")), "freshness: 72h", "freshness: 96h", 1))
	run(t, "notification-worker", "apply", true)
	qualify()
	run(t, "notification-worker", "apply", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}

	// Explicit producer timing is independent of subsequent backup deadlines.
	current, err := install.InspectInstallation(ctx, gateway)
	testutil.Must(t, err)
	raw, err := os.ReadFile(current.Manifest.Paths.Config)
	testutil.Must(t, err)
	var input install.RuntimeInput
	testutil.Must(t, json.Unmarshal(raw, &input))
	runtime, err := input.Resolve()
	testutil.Must(t, err)
	producer := runtime.Worker.Producers[0]
	fields := fmt.Sprintf("      producers:\n        backup-worker: {required: %t, routes: [alerts], max_messages: %d, max_silence_seconds: %d, max_evidence_seconds: %d, policy_health: %t, interactive_context: %t}\n", producer.Required, producer.MaxMessages, producer.MaxSilenceSeconds, producer.MaxEvidenceSeconds, producer.PolicyHealth, producer.InteractiveContext)
	write("server.yaml", strings.Replace(string(read(t, "server.yaml")), "    notification_worker:\n", "    notification_worker:\n"+fields, 1))
	run(t, "notification-worker", "apply", true)
	write("server.yaml", strings.Replace(string(read(t, "server.yaml")), "freshness: 96h", "freshness: 120h", 1))
	run(t, "backup-worker", "apply", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
	unchanged, err := install.InspectInstallation(ctx, gateway)
	testutil.Must(t, err)
	if protocol.Hash(unchanged.Manifest) != protocol.Hash(current.Manifest) {
		t.Fatal("independent backup deadline restarted or requalified gateway")
	}

	// A retained source may move after alternating notification and backup
	// changes. Lost inspection replies retain the original key and history.
	sourceTree := planTree(t, root+"/app")
	var beforeMove backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &beforeMove))
	moved := strings.Replace(string(read(t, "server.yaml")), "host: 127.0.0.1", "host: 127.0.0.3", 1)
	moved = strings.Replace(moved, fmt.Sprintf("ssh_port: %d", port), fmt.Sprintf("ssh_port: %d", movedPort), 1)
	write("server.yaml", moved)
	run(t, "backup-worker", "plan", true)
	write("lose-source-reply", "once")
	run(t, "backup-worker", "apply", false)
	if !reflect.DeepEqual(sourceTree, planTree(t, root+"/app")) {
		t.Fatal("lost endpoint inspection reply changed source ownership")
	}
	run(t, "backup-worker", "resume", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
	var afterMove backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &afterMove))
	if afterMove.WorkerID != beforeMove.WorkerID || afterMove.ArchiveID != beforeMove.ArchiveID || !reflect.DeepEqual(afterMove.SourceKeys, beforeMove.SourceKeys) || !reflect.DeepEqual(afterMove.SourceEnrollments, beforeMove.SourceEnrollments) || afterMove.SharedPolicyHistory == nil {
		t.Fatal("shared endpoint move replaced ownership or lost history")
	}
	var movedRuntime backup.WorkerInstallation
	testutil.Must(t, json.Unmarshal(read(t, "manager/.config/deploy/backup-worker/runtime.json"), &movedRuntime))
	if movedRuntime.Sources[0].Host != "127.0.0.3" || movedRuntime.Sources[0].Port != movedPort {
		t.Fatal("shared endpoint move did not publish the selected address")
	}
	afterSource := planTree(t, root+"/app")
	delete(sourceTree, root+"/app/.local/state/deploy/backup-source/rate.json")
	delete(afterSource, root+"/app/.local/state/deploy/backup-source/rate.json")
	if !reflect.DeepEqual(sourceTree, afterSource) {
		t.Fatal("shared endpoint move modified source authority")
	}
	stable, err := install.InspectInstallation(ctx, gateway)
	testutil.Must(t, err)
	if protocol.Hash(stable.Manifest) != protocol.Hash(unchanged.Manifest) {
		t.Fatal("source endpoint move changed notification qualification")
	}

	// Notification policy can continue from the completed endpoint receipt.
	write("server.yaml", strings.Replace(moved, "    notification_worker:\n", "    notification_worker:\n      limits: {max_messages: 1200}\n", 1))
	run(t, "notification-worker", "apply", true)
	qualify()
	run(t, "notification-worker", "apply", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}

}

func testSharedSourceAdditionCLI(t *testing.T, root string, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, qualify func(), combined bool) {
	const setup = "manager/.local/state/deploy/backup-worker/setup.json"

	// Adding another source after these histories retains the old source and
	// the newly reserved enrollment even when its administrator reply is lost.
	var beforeAddition backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &beforeAddition))
	port2, public2 := startBackupCLIRestrictionServer(t, root+"/app2", "127.0.0.4")
	write("known_hosts", string(read(t, "known_hosts"))+fmt.Sprintf("[127.0.0.4]:%d %s", port2, ssh.MarshalAuthorizedKey(public2)))
	added := strings.Replace(string(read(t, "server.yaml")), "sources: [source]", "sources: [source, extra]", 1)
	added += fmt.Sprintf("  extra:\n    host: 127.0.0.4\n    user: app2\n    ssh_port: %d\n    ssh_known_hosts: /tmp/wux/known_hosts\n", port2)
	if combined {
		added = strings.Replace(added, "sources: [source, extra]", "sources: [extra]\n      retire_sources: {source: replaced by extra}", 1)
		write("lose-retirement-reply", "once")
	}
	write("server.yaml", added)
	write("lose-extra-source-reply", "once")
	run(t, "backup-worker", "apply", false)
	remoteEnrollment := read(t, "app2/.local/state/deploy/backup-source/identity.json")
	var pending backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &pending))
	if pending.SourceKeys["extra"] == "" || pending.SourceExpansion == nil {
		t.Fatal("lost enrollment reply discarded reserved key")
	}
	if combined {
		run(t, "backup-worker", "resume", false)
		var published backup.WorkerSetupJournal
		testutil.Must(t, json.Unmarshal(read(t, setup), &published))
		if published.SharedPolicyExpansion == nil || published.SourceRetirement == nil || published.SharedPolicyUpdate == nil || !reflect.DeepEqual(published.SharedPolicyHistory, beforeAddition.SharedPolicyHistory) {
			t.Fatal("replacement lost pending publication history")
		}
	}
	run(t, "backup-worker", "resume", true)
	if string(remoteEnrollment) != string(read(t, "app2/.local/state/deploy/backup-source/identity.json")) {
		t.Fatal("resume replaced the acknowledged remote enrollment")
	}
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
	var afterAddition backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &afterAddition))
	if afterAddition.WorkerID != beforeAddition.WorkerID || afterAddition.ArchiveID != beforeAddition.ArchiveID || afterAddition.SourceKeys["extra"] == "" || afterAddition.SourceExpansion != nil {
		t.Fatal("shared source addition lost an enrollment or replaced ownership")
	}
	if combined {
		if len(afterAddition.SourceKeys) != 1 || afterAddition.SourceKeys["source"] != "" || afterAddition.RetiredSources["source"].SourceID != beforeAddition.SourceEnrollments["source"].SourceID || afterAddition.SharedPolicyExpansion != nil || afterAddition.SharedPolicyUpdate != nil {
			t.Fatal("replacement lost completed enrollment or disposition")
		}
	} else if len(afterAddition.SourceKeys) != 2 || afterAddition.SourceKeys["source"] != beforeAddition.SourceKeys["source"] || afterAddition.SourceEnrollments["source"] != beforeAddition.SourceEnrollments["source"] {
		t.Fatal("addition changed existing source")
	}
	write("server.yaml", strings.Replace(added, "    notification_worker:\n", "    notification_worker:\n      limits: {max_messages: 1400}\n", 1))
	run(t, "notification-worker", "apply", true)
	qualify()
	run(t, "notification-worker", "apply", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}

	if combined {
		testSharedBackupImageCLI(t, write, read, run, qualify)
		testSharedBackupHelperCLI(t, write, read, run, qualify)
		return
	}
	// Retire the original source only after publishing the remaining set.
	// A lost remote revocation reply must leave the disposition pending.
	var beforeRetirement backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &beforeRetirement))
	retiring := strings.Replace(string(read(t, "server.yaml")), "sources: [source, extra]", "sources: [extra]\n      retire_sources: {source: replaced by extra}", 1)
	write("server.yaml", retiring)
	write("lose-retirement-reply", "once")
	run(t, "backup-worker", "apply", false)
	var pendingRetirement backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &pendingRetirement))
	if pendingRetirement.SourceRetirement == nil || pendingRetirement.SharedPolicyUpdate == nil || pendingRetirement.SourceKeys["source"] == "" || !reflect.DeepEqual(pendingRetirement.SharedPolicyHistory, beforeRetirement.SharedPolicyHistory) {
		t.Fatal("unconfirmed retirement advanced shared history")
	}
	run(t, "backup-worker", "resume", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
	var retired backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &retired))
	if retired.SourceRetirement != nil || retired.SharedPolicyUpdate != nil || retired.RetiredSources["source"].SourceID != beforeRetirement.SourceEnrollments["source"].SourceID || retired.SourceKeys["source"] != "" || retired.SourceKeys["extra"] != beforeRetirement.SourceKeys["extra"] || retired.WorkerID != beforeRetirement.WorkerID || retired.ArchiveID != beforeRetirement.ArchiveID {
		t.Fatal("retirement lost disposition or remaining ownership")
	}
	write("server.yaml", strings.Replace(retiring, "max_messages: 1400", "max_messages: 1600", 1))
	run(t, "notification-worker", "apply", true)
	qualify()
	run(t, "notification-worker", "apply", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}

}

func testSharedBackupImageCLI(t *testing.T, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, qualify func()) {
	const setup = "manager/.local/state/deploy/backup-worker/setup.json"
	var before backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &before))
	manager := read(t, "manager/manager.json")
	image := "fixture.invalid/backup@sha256:" + strings.Repeat("b", 64)
	old := "      storage: {min_free_bytes: 1048576}\n      artifacts: {catalog: /tmp/wux/catalog.json}"
	next := "      storage: {min_free_bytes: 1048576}\n      artifacts: {catalog: /tmp/wux/catalog.json, image: " + image + "}"
	yaml := string(read(t, "server.yaml"))
	if !strings.Contains(yaml, old) {
		t.Fatal("image fixture declaration missing")
	}
	write("server.yaml", strings.Replace(yaml, old, next, 1))
	run(t, "backup-worker", "plan", true)
	write("lose-source-reply", "once")
	run(t, "backup-worker", "apply", false)
	var pending backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &pending))
	if pending.SharedPolicyUpdate == nil || pending.SharedPolicyUpdate.Version != 6 {
		t.Fatal("image interruption lost its retained review")
	}
	operation := pending.SharedPolicyUpdate.Operation
	run(t, "backup-worker", "resume", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
	var after backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &after))
	var installed backup.WorkerInstallation
	testutil.Must(t, json.Unmarshal(read(t, "manager/.config/deploy/backup-worker/runtime.json"), &installed))
	if after.SharedPolicyHistory == nil || after.SharedPolicyHistory.Operation != operation || installed.Image != image || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.SourceEnrollments, before.SourceEnrollments) || !reflect.DeepEqual(after.RetiredSources, before.RetiredSources) || after.Declaration.HelperSHA256 != before.Declaration.HelperSHA256 {
		t.Fatal("image update lost configured artifact or retained ownership")
	}
	if string(manager) != string(read(t, "manager/manager.json")) {
		t.Fatal("backup image update restarted gateway")
	}
	write("server.yaml", strings.Replace(string(read(t, "server.yaml")), "max_messages: 1400", "max_messages: 1800", 1))
	run(t, "notification-worker", "apply", true)
	qualify()
	run(t, "notification-worker", "apply", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
}

func testSharedBackupHelperCLI(t *testing.T, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, qualify func()) {
	const setup = "manager/.local/state/deploy/backup-worker/setup.json"
	var before backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &before))
	originalCutover := read(t, "manager/.local/state/deploy/backup-worker/state/notification-cutover.json")
	candidate := append(read(t, "deploy"), []byte("offline helper upgrade fixture")...)
	write("upgrade-helper", string(candidate))
	testutil.Must(t, os.Chmod("/tmp/wux/upgrade-helper", 0700))
	digest := inputDigest(candidate)
	image := "fixture.invalid/backup@sha256:" + strings.Repeat("c", 64)
	yaml := string(read(t, "server.yaml"))
	old := "image: fixture.invalid/backup@sha256:" + strings.Repeat("b", 64) + "}"
	next := "image: " + image + ", helper: /tmp/wux/upgrade-helper, helper_sha256: " + digest + "}"
	if !strings.Contains(yaml, old) {
		t.Fatal("helper fixture lacks image declaration")
	}
	write("server.yaml", strings.Replace(yaml, old, next, 1))
	run(t, "backup-worker", "plan", true)
	write("lose-source-reply", "once")
	run(t, "backup-worker", "apply", false)
	var pending backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &pending))
	if pending.SharedPolicyUpdate == nil || pending.SharedPolicyUpdate.Version != 7 {
		t.Fatal("helper interruption lost original upgrade")
	}
	operation := pending.SharedPolicyUpdate.Operation
	run(t, "backup-worker", "resume", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
	var after backup.WorkerSetupJournal
	testutil.Must(t, json.Unmarshal(read(t, setup), &after))
	if after.SharedPolicyHistory == nil || after.SharedPolicyHistory.Operation != operation || after.Declaration.HelperSHA256 != digest || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.SourceEnrollments, before.SourceEnrollments) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.Producer, before.Producer) {
		t.Fatal("helper upgrade lost retained ownership")
	}
	if inputDigest(read(t, "manager/.local/lib/deploy/backup-worker/deploy")) != digest {
		t.Fatal("new helper not installed")
	}
	if string(originalCutover) != string(read(t, "manager/.local/state/deploy/backup-worker/state/notification-cutover.json")) {
		t.Fatal("helper upgrade replaced reporting handoff")
	}
	write("server.yaml", strings.Replace(string(read(t, "server.yaml")), "max_messages: 1800", "max_messages: 2000", 1))
	run(t, "notification-worker", "apply", true)
	qualify()
	run(t, "notification-worker", "apply", true)
	for _, worker := range []string{"backup-worker", "notification-worker"} {
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			run(t, worker, action, true)
		}
	}
}
