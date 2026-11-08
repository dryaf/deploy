//go:build linux

package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/testutil"
)

const signalProviderCLIFixture = `package main
import("fmt";"os";"slices")
func main(){if slices.Contains(os.Args,"--version"){fmt.Println("signal-cli 0.14.9");return};if slices.Contains(os.Args,"listAccounts"){fmt.Print(` + "`" + `[{"number":"+10000000000","aci":"11111111-1111-4111-8111-111111111111"}]` + "`" + `);return};if slices.Contains(os.Args,"link"){fmt.Println("synthetic-private-link-no-network");return};if slices.Contains(os.Args,"send")||slices.Contains(os.Args,"receive"){return};os.Exit(7)}
`

func signalProviderCLIManager(manager string) string {
	manager = strings.Replace(manager, "path=root/'manager.json'", `signal_call='deploy-notification-signal.service' in sys.argv
if 'daemon-reload' in sys.argv and not signal_call:
 subprocess.run([sys.argv[0],'--user','daemon-reload','deploy-notification-signal.service'],check=True)
path=root/('signal-manager.json' if signal_call else 'manager.json')`, 1)
	manager = strings.Replace(manager, "unit=root/'.config/systemd/user/deploy-notification-worker.service'", "unit=root/'.config/systemd/user'/('deploy-notification-signal.service' if signal_call else 'deploy-notification-worker.service')", 1)
	manager = strings.Replace(manager, "assert argv[1:3]==['notification-worker','__managed-serve']", "assert argv[1]=='notification-worker' and argv[2] in ('__managed-serve','__fresh-signal-serve')", 1)
	manager = strings.Replace(manager, "'deploy-notification-worker.service' in args and '--all' in args", "('deploy-notification-worker.service' in args or signal_call) and '--all' in args", 1)
	manager = strings.ReplaceAll(manager, "state['LoadedUnit']", "state.get('LoadedUnit','')")
	manager = strings.ReplaceAll(manager, "unit.read_text()==state.get('LoadedUnit','')", "(unit.read_text() if unit.exists() else '')==state.get('LoadedUnit','')")
	return manager
}

func testSignalProvidersCLI(t *testing.T, root string, policy func(bool, string), write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any) {
	ctx := context.Background()
	policy(true, "daily")
	raw := read(t, "signal-cli")
	nativeHash := inputDigest(raw)
	write("signal-account", "+10000000000")
	write("signal-recipient", "22222222-2222-4222-8222-222222222222")
	declared := string(read(t, "server.yaml"))
	extra := fmt.Sprintf("        signal: {account_file: /tmp/wux/signal-account, recipient_file: /tmp/wux/signal-recipient, executable: /tmp/wux/signal-cli, executable_sha256: %s}\n", nativeHash)
	declared = strings.Replace(declared, "      providers:\n", "      routes: {alerts: {primary: signal}}\n      providers:\n"+extra, 1)
	if strings.Contains(t.Name(), "SignalOnly") {
		declared = strings.Replace(declared, "        telegram:\n          token_file: /tmp/wux/token\n          chat_id_file: /tmp/wux/chat\n", "", 1)
	}
	write("server.yaml", declared)
	inventory, err := config.ReadInventory(root + "/server.yaml")
	testutil.Must(t, err)
	desired := *inventory.Servers["manager"].Desired
	hash, err := desired.NotificationFingerprint()
	testutil.Must(t, err)
	helper := read(t, "deploy")
	facts := TargetFacts{OS: "linux", Arch: runtime.GOARCH, UID: os.Getuid(), User: "manager", Home: root + "/manager"}
	paths, err := ResolvePaths("notification-worker", desired.Notification.Paths, facts)
	testutil.Must(t, err)
	request := ControlRequest{Version: 1, Action: "apply", Worker: "notification-worker", Server: "manager", Desired: desired, DesiredHash: hash, DesiredScope: 1, Facts: facts, HelperSHA256: inputDigest(helper), SignalSHA256: nativeHash, InteractiveLink: true}
	// Link the synthetic CLI through the same setup coordinator used by the
	// guided terminal. All following updates use compiled short CLI entry points.
	native, _, secrets, err := prepareDeclaredSignal(ctx, request, paths, root+"/deploy", "", io.Discard, defaultSignalSetupLifecycle())
	testutil.Must(t, err)
	t.Cleanup(func() {
		out, err := exec.Command(root+"/systemctl", "--user", "stop", "deploy-notification-signal.service").CombinedOutput()
		if err != nil {
			t.Errorf("native fixture cleanup: %v %s", err, out)
		}
	})
	serviceBefore := read(t, "manager/.local/state/deploy/worker-control/notification-worker/signal/service.json")
	nativeBefore := protocol.Hash(native)
	var nativeProcess struct{ PID, Starts int }
	testutil.Must(t, json.Unmarshal(read(t, "manager/signal-manager.json"), &nativeProcess))
	_, err = applyNotificationBootstrapWithSignal(ctx, request, paths, root+"/deploy", notificationLifecycle{install.EnsureVerification, install.InspectVerification}, &native, secrets)
	testutil.Must(t, err)
	run(t, "backup-worker", "apply", true)
	// Shared staged provider changes retain the original native child as well.
	write("signal-account-staged", "+10000000000")
	write("signal-recipient-staged", "22222222-2222-4222-8222-222222222222")
	staged := strings.Replace(declared, "/tmp/wux/signal-account", "/tmp/wux/signal-account-staged", 1)
	staged = strings.Replace(staged, "/tmp/wux/signal-recipient", "/tmp/wux/signal-recipient-staged", 1)
	write("server.yaml", staged)
	run(t, "notification-worker", "apply", true)
	qualificationChecks := 0
	qualify := func() {
		t.Helper()
		o, err := install.InspectInstallation(ctx, paths.Installation)
		testutil.Must(t, err)
		q, err := install.BeginQualification(ctx, paths.Installation, protocol.Hash(o.Manifest))
		testutil.Must(t, err)
		qualificationChecks = len(q.Checks)
		write("signal-evidence.log", "offline synthetic Signal qualification observations")
		var answers strings.Builder
		for range q.Checks {
			answers.WriteString("passed\n" + root + "/signal-evidence.log\n")
		}
		qualifyNotificationThroughTerminal(t, root, answers.String(), false)
	}
	qualify()
	run(t, "notification-worker", "apply", true)
	run(t, "backup-worker", "apply", true)
	handoff := read(t, "manager/.local/state/deploy/backup-worker/state/notification-cutover.json")
	if strings.Contains(t.Name(), "SharedActivity") {
		testSharedActivityCLI(t, root, write, read, run, qualificationChecks, false)
		var running struct{ PID, Starts int }
		testutil.Must(t, json.Unmarshal(read(t, "manager/signal-manager.json"), &running))
		o, e := install.InspectRetainedInstallation(ctx, paths.Installation)
		testutil.Must(t, e)
		current, e := install.InspectFreshSignal(ctx, *o.Manifest.FreshSignal, false)
		testutil.Must(t, e)
		if protocol.Hash(current) != nativeBefore {
			t.Fatal("shared activity replaced native account/inbox service")
		}
		if running != nativeProcess || !reflect.DeepEqual(serviceBefore, read(t, "manager/.local/state/deploy/worker-control/notification-worker/signal/service.json")) || !reflect.DeepEqual(handoff, read(t, "manager/.local/state/deploy/backup-worker/state/notification-cutover.json")) {
			t.Fatal("shared activity changed native ownership or process")
		}
		return
	}
	for cycle := range 3 {
		account, recipient := fmt.Sprintf("signal-account-%d", cycle), fmt.Sprintf("signal-recipient-%d", cycle)
		write(account, "+10000000000")
		value := "22222222-2222-4222-8222-222222222222"
		if cycle > 0 {
			value = "33333333-3333-4333-8333-333333333333"
		}
		write(recipient, value)
		next := strings.Replace(declared, "/tmp/wux/signal-account", "/tmp/wux/"+account, 1)
		next = strings.Replace(next, "/tmp/wux/signal-recipient", "/tmp/wux/"+recipient, 1)
		write("server.yaml", next)
		result := run(t, "notification-worker", "apply", true)
		if result["state"] != "needs_qualification" {
			t.Fatal("Signal references reused qualification", result)
		}
		qualify()
		run(t, "notification-worker", "apply", true)
		for _, w := range []string{"backup-worker", "notification-worker"} {
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, w, action, true)
			}
		}
		o, err := install.InspectInstallation(ctx, paths.Installation)
		testutil.Must(t, err)
		c, err := install.ReadRuntime(o.Manifest.Paths.Config)
		testutil.Must(t, err)
		message := protocol.Message{ID: fmt.Sprint("offline-signal-", cycle), Producer: c.Worker.Producers[0].Producer, Text: "offline synthetic destination test"}
		adapter := provider.Adapter{Profiles: c.Providers}
		sent := adapter.Send(ctx, "signal", message)
		// Native receive maintenance shares one account slot. Only an explicit
		// safe retry response may be retried; an uncertain send is never repeated.
		for attempt := 0; attempt < 4 && !sent.Accepted && !sent.Ambiguous && !sent.Permanent && sent.RetryAfter > 0; attempt++ {
			time.Sleep(sent.RetryAfter)
			sent = adapter.Send(ctx, "signal", message)
		}
		if !sent.Accepted {
			t.Fatal("native relay rejected approved recipient", sent)
		}
		profile, err := install.InspectManagedReportProfile(paths.Installation, o.Manifest.Identity, c.Worker.Producers[0].Producer, "alerts")
		testutil.Must(t, err)
		body := string(read(t, "manager/.config/systemd/user/deploy-backup-report.service"))
		for _, path := range []string{"/tmp/wux/signal-account", "/tmp/wux/signal-recipient", "/tmp/wux/signal-account-staged", "/tmp/wux/signal-recipient-staged", "/tmp/wux/" + account, "/tmp/wux/" + recipient} {
			if !slices.Contains(profile.Credentials, path) || !strings.Contains(body, path) {
				t.Fatal("report service released private native reference", path)
			}
		}
		var running struct{ PID, Starts int }
		testutil.Must(t, json.Unmarshal(read(t, "manager/signal-manager.json"), &running))
		if running != nativeProcess {
			t.Fatal("provider migration restarted native service")
		}
		current, err := install.InspectFreshSignal(ctx, *o.Manifest.FreshSignal, false)
		testutil.Must(t, err)
		if protocol.Hash(current) != nativeBefore || !reflect.DeepEqual(handoff, read(t, "manager/.local/state/deploy/backup-worker/state/notification-cutover.json")) || !reflect.DeepEqual(serviceBefore, read(t, "manager/.local/state/deploy/worker-control/notification-worker/signal/service.json")) {
			t.Fatal("Signal reference edits replaced native or reporting ownership")
		}
	}
	if strings.Contains(t.Name(), "SignalOnly") {
		currentPolicy := string(read(t, "server.yaml"))
		withTelegram := strings.Replace(currentPolicy, "      providers:\n", "      providers:\n        telegram: {token_file: /tmp/wux/token, chat_id_file: /tmp/wux/chat}\n", 1)
		for _, next := range []string{withTelegram, currentPolicy} {
			write("server.yaml", next)
			result := run(t, "notification-worker", "apply", true)
			if result["state"] != "needs_qualification" {
				t.Fatal("profile presence bypassed qualification", result)
			}
			qualify()
			run(t, "notification-worker", "apply", true)
			for _, w := range []string{"backup-worker", "notification-worker"} {
				for _, action := range []string{"apply", "resume", "status", "plan"} {
					run(t, w, action, true)
				}
			}
		}
	}

}
