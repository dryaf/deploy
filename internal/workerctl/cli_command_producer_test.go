//go:build linux

package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func testCommandProducerPolicyCLI(t *testing.T, root string, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, checks int) {
	const receiver = "manager/.local/state/deploy/backup-worker/state/command-receiver.json"
	const handoff = "manager/.local/state/deploy/backup-worker/state/notification-cutover.json"
	const control = "manager/.local/state/deploy/worker-control/notification-worker/operation.json"
	originalReceiver, originalHandoff := string(read(t, receiver)), string(read(t, handoff))
	var input install.RuntimeInput
	testutil.Must(t, json.Unmarshal(read(t, "manager/.config/deploy/notification-worker/consumers/runtime.json"), &input))
	producer := input.Worker.Producers[0]
	originalPolicy := protocol.Hash(input.Worker.Interaction)
	policy := string(read(t, "server.yaml"))
	policy = strings.Replace(policy, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
	write("server.yaml", policy)
	run(t, "notification-worker", "apply", true)
	manifest := func() install.ManagedManifest {
		t.Helper()
		var j OperationJournal
		testutil.Must(t, json.Unmarshal(read(t, control), &j))
		o, err := install.InspectRetainedInstallation(context.Background(), j.Pins.Installations["notification-worker"])
		testutil.Must(t, err)
		return o.Manifest
	}
	originalManifest := manifest()
	declaration := func(quota int, access bool) string {
		required, health := producer.Required, producer.PolicyHealth
		routes, extra := "[alerts]", ""
		if access {
			required = false
			health = !health
			routes = "[alerts, audit]"
			extra = ", interactive_context: true, context_kinds: [source]"
		}
		return fmt.Sprintf("      producers: {backup-worker: {routes: %s, required: %t, max_messages: %d, max_silence_seconds: %d, max_evidence_seconds: %d, policy_health: %t%s}}\n", routes, required, quota, producer.MaxSilenceSeconds, producer.MaxEvidenceSeconds, health, extra)
	}
	withDeclaration := func(line string) string {
		return strings.Replace(policy, "    notification_worker:\n", "    notification_worker:\n"+line, 1)
	}
	allCommands := func() {
		for _, w := range []string{"notification-worker", "backup-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, w, action, true)
			}
		}
	}
	// Both declaration directions must preserve the exact original activation.
	for _, line := range []string{declaration(producer.MaxMessages, false), ""} {
		write("server.yaml", withDeclaration(line))
		p := run(t, "notification-worker", "plan", true)
		if !strings.Contains(p["next"].(string), "equivalent producer declaration") {
			t.Fatal("plan omitted equivalent declaration", p)
		}
		allCommands()
		if protocol.Hash(manifest()) != protocol.Hash(originalManifest) {
			t.Fatal("equivalent declaration paused/restarted or renewed delivery")
		}
	}
	write("producer-policy-evidence.log", "synthetic offline producer policy observations; not live qualification")
	for cycle, line := range []string{declaration(producer.MaxMessages-10, false), declaration(producer.MaxMessages-10, true), ""} {
		write("server.yaml", withDeclaration(line))
		run(t, "notification-worker", "plan", true)
		result := run(t, "notification-worker", "apply", true)
		if result["state"] != "needs_qualification" {
			t.Fatal("effective producer policy did not pause", result)
		}
		var answers strings.Builder
		for range checks {
			answers.WriteString("passed\n" + root + "/producer-policy-evidence.log\n")
		}
		qualifyNotificationThroughTerminal(t, root, answers.String(), false)
		allCommands()
		if cycle == 0 {
			policy = strings.Replace(policy, "scope: {whole_worker: true}", "scope: {subjects: [source/source]}", 1)
			policy = strings.Replace(policy, "permissions: [read, incident.manage]", "permissions: [read]", 1)
			write("server.yaml", withDeclaration(line))
			allCommands()
		}
	}
	// A later equivalent declaration retains the reduced policy and new activation.
	final := manifest()
	write("server.yaml", withDeclaration(declaration(producer.MaxMessages, false)))
	allCommands()
	if protocol.Hash(manifest()) != protocol.Hash(final) {
		t.Fatal("post-update declaration changed delivery")
	}
	policy = strings.Replace(withDeclaration(declaration(producer.MaxMessages, false)), "commands:\n        enabled: false", "commands:\n        enabled: true", 1)
	write("server.yaml", policy)
	allCommands()
	if string(read(t, receiver)) != originalReceiver || string(read(t, handoff)) != originalHandoff {
		t.Fatal("producer changes rewrote original receiver or reporting handoff")
	}
	current := manifest()
	next, err := install.ReadRuntime(current.Paths.Config)
	testutil.Must(t, err)
	if protocol.Hash(next.Worker.Interaction) != originalPolicy || next.Worker.Producers[0].Producer != producer.Producer {
		t.Fatal("producer edits changed original runtime receiver or producer identity")
	}
}
