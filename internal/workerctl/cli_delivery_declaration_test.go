//go:build linux

package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"strings"
	"testing"
)

func testDeliveryDeclarationCLI(t *testing.T, root string, policy func(bool, string), write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any) {
	ctx := context.Background()
	policy(true, "daily")
	base := string(read(t, "server.yaml"))
	run(t, "backup-worker", "apply", true)
	const control = "manager/.local/state/deploy/worker-control/notification-worker/"
	var j OperationJournal
	testutil.Must(t, json.Unmarshal(read(t, control+"operation.json"), &j))
	gateway := j.Pins.Installations["notification-worker"]
	qualify := func() {
		t.Helper()
		o, e := install.InspectInstallation(ctx, gateway)
		testutil.Must(t, e)
		session, e := install.BeginQualification(ctx, gateway, protocol.Hash(o.Manifest))
		testutil.Must(t, e)
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
	raw, err := os.ReadFile(original.Manifest.Paths.Config)
	testutil.Must(t, err)
	var input install.RuntimeInput
	testutil.Must(t, json.Unmarshal(raw, &input))
	c, err := input.Resolve()
	testutil.Must(t, err)
	producer := c.Worker.Producers[0]
	route := input.Worker.Routes["alerts"]
	limits := fmt.Sprintf("      limits: {max_messages: %d}\n", c.Worker.MaxMessages)
	audit := fmt.Sprintf("      limits: {max_messages: %d, receipt_audit_days: %d}\n", c.Worker.MaxMessages, c.Worker.ReceiptAuditDays)
	producers := fmt.Sprintf("      producers:\n        backup-worker: {required: %t, routes: [alerts], max_messages: %d, max_silence_seconds: %d, max_evidence_seconds: %d, policy_health: %t, interactive_context: %t}\n", producer.Required, producer.MaxMessages, producer.MaxSilenceSeconds, producer.MaxEvidenceSeconds, producer.PolicyHealth, producer.InteractiveContext)
	routes := fmt.Sprintf("      routes: {alerts: {primary: %s, fallback: '%s'}}\n", route.Primary, route.Fallback)
	render := func(fields string) string {
		return strings.Replace(base, "    notification_worker:\n", "    notification_worker:\n"+fields, 1)
	}
	declarations := 0
	verifyNoop := func(next string) {
		t.Helper()
		before, e := install.InspectInstallation(ctx, gateway)
		testutil.Must(t, e)
		var managerBefore, managerAfter map[string]any
		testutil.Must(t, json.Unmarshal(read(t, "manager/manager.json"), &managerBefore))
		write("server.yaml", next)
		run(t, "notification-worker", "plan", true)
		result := run(t, "notification-worker", "apply", true)
		if next, ok := result["next"].(string); ok && strings.Contains(next, "declaration recorded") {
			declarations++
		}
		if result["state"] != "unchanged" && result["state"] != "reporting_connected" {
			t.Fatal("declaration renewed effective policy", result)
		}
		for _, worker := range []string{"backup-worker", "notification-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, worker, action, true)
			}
		}
		after, e := install.InspectInstallation(ctx, gateway)
		testutil.Must(t, e)
		testutil.Must(t, json.Unmarshal(read(t, "manager/manager.json"), &managerAfter))
		if protocol.Hash(before.Manifest) != protocol.Hash(after.Manifest) || managerBefore["PID"] != managerAfter["PID"] || managerBefore["Starts"] != managerAfter["Starts"] {
			t.Fatal("declaration restarted delivery or changed qualification")
		}
	}
	for _, fields := range []string{limits, audit, audit + producers, audit + producers + routes, audit + producers, audit} {
		verifyNoop(render(fields))
	}
	if declarations < 2 {
		t.Fatal("declaration continuation was not exercised")
	}
	// A real change after declaration-only history must still pause and qualify.
	changed := strings.Replace(render(audit), fmt.Sprintf("max_messages: %d", c.Worker.MaxMessages), fmt.Sprintf("max_messages: %d", c.Worker.MaxMessages-1), 1)
	write("server.yaml", changed)
	result := run(t, "notification-worker", "apply", true)
	if result["state"] != "needs_qualification" {
		t.Fatal("effective change bypassed qualification", result)
	}
	qualify()
	run(t, "notification-worker", "apply", true)
	verifyNoop(strings.Replace(changed, fmt.Sprintf(", receipt_audit_days: %d", c.Worker.ReceiptAuditDays), "", 1))
}
