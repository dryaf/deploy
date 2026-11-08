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

func testCommandProvidersCLI(t *testing.T, root string, write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, checks int) {
	const receiver = "manager/.local/state/deploy/backup-worker/state/command-receiver.json"
	const handoff = "manager/.local/state/deploy/backup-worker/state/notification-cutover.json"
	originalReceiver, originalHandoff := string(read(t, receiver)), string(read(t, handoff))
	var original install.RuntimeInput
	testutil.Must(t, json.Unmarshal(read(t, "manager/.config/deploy/notification-worker/consumers/runtime.json"), &original))
	policy := string(read(t, "server.yaml"))
	policy = strings.Replace(policy, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
	write("server.yaml", policy)
	run(t, "notification-worker", "apply", true)
	manifest := func() install.ManagedManifest {
		t.Helper()
		var j OperationJournal
		testutil.Must(t, json.Unmarshal(read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json"), &j))
		o, err := install.InspectRetainedInstallation(context.Background(), j.Pins.Installations["notification-worker"])
		testutil.Must(t, err)
		return o.Manifest
	}
	owner := manifest().DeliveryOwnerHash
	all := func() {
		for _, w := range []string{"notification-worker", "backup-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, w, action, true)
			}
		}
	}
	write("provider-evidence.log", "synthetic offline provider observations; not live qualification")
	for cycle := range 3 {
		write(fmt.Sprintf("manager/rotated-token-%d", cycle), "123:rotated-secret")
		// The fixture's initial references are target-private absolute paths.
		oldToken := original.Providers["telegram"].TokenFile
		if cycle > 0 {
			oldToken = fmt.Sprintf("%s/manager/rotated-token-%d", root, cycle-1)
		}
		policy = strings.ReplaceAll(policy, oldToken, fmt.Sprintf("%s/manager/rotated-token-%d", root, cycle))
		if cycle == 1 {
			write("manager/rotated-chat", "987")
			policy = strings.ReplaceAll(policy, original.Providers["telegram"].ChatFile, root+"/manager/rotated-chat")
		}
		write("server.yaml", policy)
		result := run(t, "notification-worker", "apply", true)
		if result["state"] != "needs_qualification" {
			t.Fatal("provider update did not require qualification", result)
		}
		var answers strings.Builder
		for range checks {
			answers.WriteString("passed\n" + root + "/provider-evidence.log\n")
		}
		qualifyNotificationThroughTerminal(t, root, answers.String(), false)
		all()
		if cycle == 0 {
			policy = strings.Replace(policy, "scope: {whole_worker: true}", "scope: {subjects: [source/source]}", 1)
			policy = strings.Replace(policy, "permissions: [read, incident.manage]", "permissions: [read]", 1)
			write("server.yaml", policy)
			all()
		}
	}
	policy = strings.Replace(policy, "commands:\n        enabled: false", "commands:\n        enabled: true", 1)
	write("server.yaml", policy)
	all()
	current := manifest()
	next, err := install.ReadRuntime(current.Paths.Config)
	testutil.Must(t, err)
	if string(read(t, receiver)) != originalReceiver || string(read(t, handoff)) != originalHandoff || current.DeliveryOwnerHash != owner || protocol.Hash(original.Worker.Interaction) != protocol.Hash(next.Worker.Interaction) || next.Telegram.Account != original.Telegram.Account {
		t.Fatal("provider update changed original receiver, owner, handoff or intake policy")
	}
}
