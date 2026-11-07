//go:build linux

package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func testDeliveryProvidersCLI(t *testing.T, root string, policy func(bool, string), write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any, limits ...bool) {
	limitCase := len(limits) > 0 && limits[0]
	producerCase := len(limits) > 1 && limits[1]
	routesCase := len(limits) > 2 && limits[2]
	destinationCase := len(limits) > 3 && limits[3]
	ctx := context.Background()
	policy(true, "daily")
	declared := string(read(t, "server.yaml"))
	run(t, "backup-worker", "apply", true)
	const control = "manager/.local/state/deploy/worker-control/notification-worker/"
	var enrollment OperationJournal
	testutil.Must(t, json.Unmarshal(read(t, control+"operation.json"), &enrollment))
	gateway := enrollment.Pins.Installations["notification-worker"]
	qualify := func() {
		t.Helper()
		o, err := install.InspectInstallation(ctx, gateway)
		testutil.Must(t, err)
		session, err := install.BeginQualification(ctx, gateway, protocol.Hash(o.Manifest))
		testutil.Must(t, err)
		write("evidence.log", "synthetic offline provider observations; no live exchange")
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
	const setup = "manager/.local/state/deploy/backup-worker/setup.json"
	testutil.Must(t, json.Unmarshal(read(t, setup), &before))
	consumer := read(t, control+"consumer-enrollment.json")
	bootstrap := read(t, "manager/.config/deploy/notification-worker/runtime.json")
	cycles := 2
	if limitCase || producerCase || routesCase || destinationCase {
		cycles = 3
	}
	for cycle := 0; cycle < cycles; cycle++ {
		budgetChange := limitCase && cycle != 1
		producerChange := producerCase && cycle != 1
		routesChange := routesCase && cycle != 1
		tokenName, chatName := fmt.Sprintf("delivery-token-%d", cycle), fmt.Sprintf("delivery-chat-%d", cycle)
		write(tokenName, fmt.Sprintf("123:offline-rotated-token-%d", cycle))
		write(chatName, "456")
		next := strings.Replace(declared, "token_file: /tmp/wux/token", "token_file: /tmp/wux/"+tokenName, 1)
		next = strings.Replace(next, "chat_id_file: /tmp/wux/chat", "chat_id_file: /tmp/wux/"+chatName, 1)
		if destinationCase {
			chat := "900"
			if cycle == 2 {
				chat = "901"
				write(tokenName, "777:offline-new-account")
			}
			write(chatName, chat)
			if cycle > 0 {
				next = strings.Replace(declared, fmt.Sprintf("/tmp/wux/delivery-token-%d", cycle-1), "/tmp/wux/"+tokenName, 1)
				next = strings.Replace(next, fmt.Sprintf("/tmp/wux/delivery-chat-%d", cycle-1), "/tmp/wux/"+chatName, 1)
			}
		}
		if !budgetChange && !producerChange && !routesChange && cycle == 0 {
			write("wrong-chat", "")
			write("server.yaml", strings.Replace(next, "chat_id_file: /tmp/wux/"+chatName, "chat_id_file: /tmp/wux/wrong-chat", 1))
			tree := planTree(t, root+"/"+control)
			run(t, "notification-worker", "apply", false)
			if !reflect.DeepEqual(tree, planTree(t, root+"/"+control)) {
				t.Fatal("changed audience retained production credential intent")
			}
		}
		if budgetChange {
			base := declared
			for _, budget := range []int{9000, 8000, 7000} {
				base = strings.Replace(base, fmt.Sprintf("      limits: {max_messages: %d}\n", budget), "", 1)
			}
			next = strings.Replace(base, "    notification_worker:\n", fmt.Sprintf("    notification_worker:\n      limits: {max_messages: %d}\n", 9000-1000*cycle), 1)
		}
		if producerChange {
			base := declared
			if cycle == 2 {
				base = strings.Replace(base, "      producers:\n        backup-worker: {routes: [alerts], max_messages: 1100, max_silence_seconds: 300, max_evidence_seconds: 172800, policy_health: true}\n", "", 1)
			}
			extra := ""
			if cycle == 2 {
				extra = "required: false, "
			}
			next = strings.Replace(base, "    notification_worker:\n", "    notification_worker:\n      producers:\n        backup-worker: {"+extra+"routes: [alerts], max_messages: 1100, max_silence_seconds: 300, max_evidence_seconds: 172800, policy_health: true}\n", 1)
		}
		if routesChange {
			routeDeclaration := "      routes: {alerts: {primary: telegram}, audit: {primary: telegram}}\n"
			if cycle == 0 {
				next = strings.Replace(declared, "    notification_worker:\n", "    notification_worker:\n"+routeDeclaration, 1)
			} else {
				next = strings.Replace(declared, routeDeclaration, "", 1)
			}
		}
		write("server.yaml", next)
		run(t, "notification-worker", "plan", true)
		result := run(t, "notification-worker", "apply", true)
		if result["state"] != "needs_qualification" {
			t.Fatal("credential update bypassed new qualification", result)
		}
		for _, action := range []string{"resume", "status", "plan"} {
			run(t, "notification-worker", action, true)
		}
		paused, err := install.InspectInstallation(ctx, gateway)
		testutil.Must(t, err)
		if paused.Manifest.Mode != "verification" || paused.Manifest.Identity != original.Manifest.Identity || paused.Manifest.DeliveryOwnerHash != original.Manifest.DeliveryOwnerHash {
			t.Fatal("credential rotation changed paused owner")
		}
		body := string(read(t, "manager/.config/systemd/user/deploy-backup-report.service"))
		paths := []string{"/tmp/wux/token", "/tmp/wux/chat"}
		if !budgetChange && !producerChange && !routesChange {
			paths = append(paths, "/tmp/wux/"+tokenName, "/tmp/wux/"+chatName)
		}
		for _, path := range paths {
			if !strings.Contains(body, path) {
				t.Fatal("report service lost credential exclusion", path)
			}
		}
		qualify()
		run(t, "notification-worker", "apply", true)
		for _, worker := range []string{"backup-worker", "notification-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, worker, action, true)
			}
		}
		after, err := install.InspectInstallation(ctx, gateway)
		testutil.Must(t, err)
		if after.Manifest.Mode != "delivery" || after.Manifest.Identity != original.Manifest.Identity || after.Manifest.DeliveryOwnerHash != original.Manifest.DeliveryOwnerHash {
			t.Fatal("credential admission replaced owner")
		}
		var state backup.WorkerSetupJournal
		testutil.Must(t, json.Unmarshal(read(t, setup), &state))
		if state.WorkerID != before.WorkerID || state.ArchiveID != before.ArchiveID || !reflect.DeepEqual(state.Producer, before.Producer) || !reflect.DeepEqual(state.SourceKeys, before.SourceKeys) {
			t.Fatal("credential rotation replaced backup identities")
		}
		if !bytes.Equal(consumer, read(t, control+"consumer-enrollment.json")) || !bytes.Equal(bootstrap, read(t, "manager/.config/deploy/notification-worker/runtime.json")) {
			t.Fatal("credential rotation rewrote bootstrap or enrollment")
		}
		if limitCase || producerCase || routesCase || destinationCase {
			declared = next
		}
	}
}
