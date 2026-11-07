//go:build linux

package workerctl

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// Run the real CLI, transferred helper, source enrollment, SQLite/archive and
// gateway in disposable homes. The outer namespace has no external network and
// a read-only host filesystem. SSH, the user manager and container qualification
// are fixture adapters, so this exercises orchestration, not live qualification.
func TestWorkerCLIBackupTelegramDestinationThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "backup-telegram-destination")
}
func TestWorkerCLIThroughSSHBackupGateway(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "")
}
func TestWorkerCLISharedCommandHelperThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "helper-upgrade")
}
func TestWorkerCLISharedSourceReplacementThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-source-replacement")
}
func TestWorkerCLISharedSourceAdditionThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-source-addition")
}
func TestWorkerCLIDeliveryDerivedFreshnessThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-derived-freshness")
}
func TestWorkerCLIDeliveryDeclarationThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-declaration")
}
func TestWorkerCLIDeliveryDestinationThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-destination")
}
func TestWorkerCLIDeliveryRoutesThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-routes")
}
func TestWorkerCLIDeliveryProducerThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-producer")
}
func TestWorkerCLIDeliveryLimitsThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-limits")
}
func TestWorkerCLIDeliveryProvidersThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-providers")
}

func TestWorkerCLIDeliveryLEDOutputThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "delivery-output")
}

func TestWorkerCLICommandLimitsReadThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "command-limits", "read")
}
func TestWorkerCLICommandLimitsMutationThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "command-limits", "mutation")
}

func TestWorkerCLICommandRequalificationReadThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "requalification", "read")
}
func TestWorkerCLICommandRequalificationMutationThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "requalification", "mutation")
}

func TestWorkerCLICommandPermissionsThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "permissions")
}

func TestWorkerCLISharedPolicyThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "shared-policy")
}
func TestWorkerCLISharedCommandPolicyThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "shared-policy-commands")
}

func TestWorkerCLICombinedSourceReplacementThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "source-combined")
}

func TestWorkerCLISourceReplacementThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "source-replacement")
}

func TestWorkerCLICommandEnrollmentChangesReadThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "enrollment-changes", "read")
}
func TestWorkerCLICommandEnrollmentChangesMutationThroughSSH(t *testing.T) {
	testWorkerCLIThroughSSHBackupGateway(t, "enrollment-changes", "mutation")
}

// Each fixture owns its directories and network/mount namespace. Bound concurrency
// to three; ordinary environment-mutating tests finish before parallel fixtures run.
var backupCLIFixtureSlots = make(chan struct{}, 3)

func testWorkerCLIThroughSSHBackupGateway(t *testing.T, commandSuite string, selectedMode ...string) {
	if os.Getenv("DEPLOY_BACKUP_CLI_CHILD") == "1" {
		testBackupCLIInNamespace(t, commandSuite)
		return
	}
	if os.Getuid() == 0 {
		t.Skip("rootless Linux worker fixture")
	}
	t.Parallel()
	backupCLIFixtureSlots <- struct{}{}
	t.Cleanup(func() { <-backupCLIFixtureSlots })
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap unavailable")
	}
	probe := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-user", "--unshare-net", "--", "/usr/bin/true")
	if out, err := process.LimitedOutput(probe, 10*time.Second, 4096); err != nil {
		t.Skipf("user namespaces unavailable: %v %s", err, out)
	}
	// Archive enrollment requires ext4. Use the workspace filesystem, not /tmp
	// (often tmpfs), and expose it at a short path for Unix socket limits.
	base, err := filepath.Abs("../../dist")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, ".worker-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	var fs unix.Statfs_t
	if err = unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != 0xef53 {
		t.Skip("backup CLI fixture requires local ext4")
	}
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(dir, "deploy"), "../../cmd/deploy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, e := process.LimitedOutput(build, time.Minute, 8192); e != nil {
		t.Fatal(e, string(out))
	}
	if err = os.Chmod(filepath.Join(dir, "deploy"), 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manager", "app", "app2"} {
		if err = os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		write(name+".passwd", fmt.Sprintf("root:x:0:0:root:/root:/bin/sh\n%s:x:%d:%d:fixture:/tmp/wux/%s:/bin/sh\n", name, os.Getuid(), os.Getgid(), name), 0600)
	}
	write("ssh", backupCLISSH, 0700)
	write("loginctl", "#!/bin/sh\nprintf 'yes\\n'\n", 0700)
	write("podman", backupCLIPodman, 0700)
	write("busctl", backupCLIBus, 0700)
	write("generator", "#!/bin/sh\nfor f in \"$QUADLET_UNIT_DIRS\"/*.container; do n=${f##*/}; n=${n%.container}; printf '%s\\n' \"---$n.service---\"; done\n", 0700)
	manager := strings.Replace(nativeCLIManager, "root=pathlib.Path(os.environ['DEPLOY_WORKER_E2E_ROOT'])", "root=pathlib.Path('/tmp/wux/manager')", 1)
	manager = strings.Replace(manager, "root/'config/systemd/user/", "root/'.config/systemd/user/", 1)
	manager = strings.Replace(manager, "if action=='show-environment': sys.exit(0)", backupCLIManager, 1)
	manager = strings.Replace(manager, "state.update(LoadState='loaded',FragmentPath=str(unit),Names=unit.name)", "state.update(LoadState='loaded',FragmentPath=str(unit),Names=unit.name,LoadedUnit=unit.read_text())", 1)
	write("systemctl", manager, 0700)
	startBackupCLISourceBroker(t, dir)
	// A synthetic root keeps its ownership mapped inside the user namespace.
	// Binding the host root exposes its unmapped UID and correctly fails the LED
	// endpoint's trusted-parent check. Only runtime prerequisites are mounted;
	// the root is remounted read-only after assembling the fixture mounts.
	args := []string{"--unshare-user", "--unshare-net", "--unshare-pid", "--die-with-parent", "--tmpfs", "/", "--ro-bind", "/usr", "/usr", "--ro-bind", "/etc", "/etc", "--ro-bind", "/sys", "/sys", "--symlink", "usr/bin", "/bin", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--bind", dir, "/tmp/wux", "--ro-bind", self, "/tmp/wux/test", "--ro-bind", filepath.Join(dir, "manager.passwd"), "/etc/passwd"}
	for _, tool := range []string{"ssh", "systemctl", "loginctl", "podman", "busctl"} {
		args = append(args, "--ro-bind", filepath.Join(dir, tool), "/usr/bin/"+tool)
	}
	pattern := backupCLINamespacePattern(t.Name(), flag.Lookup("test.run").Value.String())
	if commandSuite != "" && !strings.Contains(pattern, "/") {
		if commandSuite == "delivery-providers" || commandSuite == "delivery-limits" || commandSuite == "delivery-producer" || commandSuite == "delivery-routes" || commandSuite == "delivery-destination" || commandSuite == "delivery-declaration" || (commandSuite == "delivery-derived-freshness" || (commandSuite == "delivery-source-addition" || commandSuite == "delivery-source-replacement")) {
			pattern += "/" + commandSuite + "$"
		} else if commandSuite == "backup-telegram-destination" {
			pattern += "/backup-telegram-destination$"
		} else if commandSuite == "delivery-output" {
			pattern += "/(delivery|commands)-led-output$"
		} else if strings.HasPrefix(commandSuite, "shared-policy") {
			pattern += "/" + commandSuite + "$"
		} else if commandSuite == "source-combined" {
			pattern += "/shared-(false|true)-source-combined$"
		} else if commandSuite == "source-replacement" {
			pattern += "/shared-(false|true)-source-replacement$"
		} else if len(selectedMode) == 1 {
			pattern += "/" + regexp.QuoteMeta(selectedMode[0]) + "-command-" + commandSuite + "$"
		} else {
			pattern += "/(read|mutation)-command-" + commandSuite + "$"
		}
	}
	args = append(args, "--ro-bind", filepath.Join(dir, "generator"), "/usr/lib/systemd/system-generators/podman-system-generator", "--remount-ro", "/", "--chdir", "/tmp/wux", "--", "/tmp/wux/test", "-test.run="+pattern, "-test.v", "-test.timeout=8m")
	cmd := exec.Command("bwrap", args...)
	cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_CLI_CHILD=1", "PATH=/usr/bin:/bin", "HOME=/tmp/wux/manager", "USER=manager", "XDG_CONFIG_HOME=", "XDG_STATE_HOME=", "XDG_DATA_HOME=", "XDG_RUNTIME_DIR=")
	var output process.Buffer
	output.Limit = 64 << 10
	cmd.Stdout, cmd.Stderr = &output, &output
	if e := process.Run(cmd, 9*time.Minute, time.Second); e != nil {
		diagnostics, _ := os.ReadFile(filepath.Join(dir, "ssh-errors.log"))
		managerErrors, _ := os.ReadFile(filepath.Join(dir, "manager/manager-errors.log"))
		runtimeLog, _ := os.ReadFile(filepath.Join(dir, "manager/runtime.log"))
		t.Fatalf("isolated backup CLI: %v\n%s\n%s\n%s\nRuntime: %s", e, output.Bytes(), diagnostics, managerErrors, runtimeLog)
	} else {
		t.Log(output.String())
	}
}

// The namespace only contains this fixture's prerequisites. Always constrain
// the top-level test, including for an empty or broad make-test filter. Preserve
// matching subtest selections using Go testing's ungrouped slash/pipe semantics.
func backupCLINamespacePattern(name, original string) string {
	exact := "^" + regexp.QuoteMeta(name) + "$"
	var selected []string
	for _, branch := range splitBackupCLIPattern(original, '|') {
		parts := splitBackupCLIPattern(branch, '/')
		match, err := regexp.MatchString(parts[0], name)
		if err == nil && match {
			if len(parts) == 1 {
				return exact
			}
			selected = append(selected, exact+"/"+strings.Join(parts[1:], "/"))
		}
	}
	if len(selected) == 0 {
		return exact
	}
	return strings.Join(selected, "|")
}

func splitBackupCLIPattern(pattern string, separator byte) []string {
	var parts []string
	start, classes, groups := 0, 0, 0
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '[':
			classes++
		case ']':
			classes = max(0, classes-1)
		case '(':
			if classes == 0 {
				groups++
			}
		case ')':
			if classes == 0 {
				groups--
			}
		default:
			if pattern[i] == separator && classes == 0 && groups == 0 {
				parts = append(parts, pattern[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, pattern[start:])
}

func TestBackupCLINamespacePatternRestrictsChildSuite(t *testing.T) {
	const name = "TestWorkerCLIThroughSSHBackupGateway"
	const exact = "^" + name + "$"
	for _, test := range []struct{ pattern, want string }{
		{"", exact}, {"Test", exact}, {".*", exact}, {"Test.*|Other", exact},
		{exact + "/enrolled-led-policy$", exact + "/enrolled-led-policy$"},
		{"Other|" + exact + "/enrolled-led-policy$", exact + "/enrolled-led-policy$"},
		{exact + "/first|Other/second|" + exact + "/third", exact + "/first|" + exact + "/third"},
		{exact + "/(one|two)/[a/b]", exact + "/(one|two)/[a/b]"},
		{"(Test.*|Other)/enrolled", exact + "/enrolled"},
	} {
		if got := backupCLINamespacePattern(name, test.pattern); got != test.want {
			t.Errorf("pattern %q: got %q, want %q", test.pattern, got, test.want)
		}
	}
}

func testBackupCLIInNamespace(t *testing.T, commandSuite string) {
	const root = "/tmp/wux"
	// An environment variable alone must never authorize the child path. Require
	// the actual read-only namespace and every installed fixture tool inode before
	// any CLI operation. Cleanup also names the fixture directly rather than PATH.
	var filesystem unix.Statfs_t
	if err := unix.Statfs("/", &filesystem); err != nil || filesystem.Flags&unix.ST_RDONLY == 0 {
		t.Fatal("backup CLI child requires a read-only host namespace")
	}
	for _, entry := range []struct{ target, fixture string }{
		{"/etc/passwd", "manager.passwd"},
		{"/usr/bin/systemctl", "systemctl"},
		{"/usr/bin/loginctl", "loginctl"},
		{"/usr/bin/podman", "podman"},
		{"/usr/bin/busctl", "busctl"},
		{"/usr/bin/ssh", "ssh"},
		{"/usr/lib/systemd/system-generators/podman-system-generator", "generator"},
	} {
		target, err := os.Stat(entry.target)
		if err != nil {
			t.Fatal(err)
		}
		fixture, err := os.Stat(filepath.Join(root, entry.fixture))
		if err != nil || !os.SameFile(target, fixture) {
			t.Fatalf("backup CLI child requires the mounted %s fixture", entry.fixture)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(root + "/deploy")
	if err != nil {
		t.Fatal(err)
	}
	cat := Catalog{Version: 1, Release: "backup-cli-fixture", Linux: map[string]ArchitectureArtifacts{runtime.GOARCH: {Helper: Artifact{"deploy", inputDigest(raw)}, BackupImage: "fixture.invalid/backup@sha256:" + strings.Repeat("a", 64)}}}
	raw, err = json.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	write("catalog.json", string(raw))
	write("token", "123:offline-backup-cli-token")
	write("chat", "456")
	port, public := startBackupCLIRestrictionServer(t, root+"/app")
	movedPort, movedPublic := startBackupCLIRestrictionServer(t, root+"/app", "127.0.0.3")
	write("known_hosts", fmt.Sprintf("[127.0.0.1]:%d %s[127.0.0.3]:%d %s", port, ssh.MarshalAuthorizedKey(public), movedPort, ssh.MarshalAuthorizedKey(movedPublic)))
	policy := func(shared bool, schedule string) {
		n := ""
		if shared {
			n = "    notification_worker:\n      providers:\n        telegram:\n          token_file: /tmp/wux/token\n          chat_id_file: /tmp/wux/chat\n      artifacts: {catalog: /tmp/wux/catalog.json}\n"
		}
		write("server.yaml", fmt.Sprintf("version: 3\nservers:\n  manager:\n    host: 127.0.0.2\n    user: manager\n%s    backup_worker:\n      sources: [source]\n      schedule: %q\n      freshness: 26h\n      max_snapshot_bytes: 1048576\n      storage: {min_free_bytes: 1048576}\n      artifacts: {catalog: /tmp/wux/catalog.json}\n  source:\n    host: 127.0.0.1\n    user: app\n    ssh_port: %d\n    ssh_known_hosts: /tmp/wux/known_hosts\n", n, schedule, port))
	}
	run := func(t *testing.T, worker, action string, success bool) map[string]any {
		t.Helper()
		args := []string{"--servers", root + "/server.yaml", "--non-interactive", worker}
		if action != "apply" {
			args = append(args, action)
		}
		args = append(args, "manager", "--json")
		cmd := exec.Command(root+"/deploy", args...)
		var out, errs process.Buffer
		out.Limit, errs.Limit = 1<<20, 16<<10
		cmd.Stdout, cmd.Stderr = &out, &errs
		e := process.Run(cmd, 60*time.Second, time.Second)
		if (e == nil) != success {
			t.Fatalf("%s %s success=%t: %v\n%s\n%s", worker, action, success, e, out.Bytes(), errs.Bytes())
		}
		var v map[string]any
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			t.Fatalf("%s %s: %v %s %s", worker, action, err, out.Bytes(), errs.Bytes())
		}
		if strings.Contains(out.String()+errs.String(), "offline-backup-cli-token") {
			t.Fatal("credential bytes escaped CLI")
		}
		t.Log(worker, action, v["state"], v["code"])
		return v
	}
	read := func(t *testing.T, name string) []byte {
		t.Helper()
		b, e := os.ReadFile(filepath.Join(root, name))
		if e != nil {
			t.Fatal(e)
		}
		return b
	}

	if commandSuite == "delivery-providers" || commandSuite == "delivery-limits" || commandSuite == "delivery-producer" || commandSuite == "delivery-routes" || commandSuite == "delivery-destination" || commandSuite == "delivery-declaration" || (commandSuite == "delivery-derived-freshness" || (commandSuite == "delivery-source-addition" || commandSuite == "delivery-source-replacement")) {
		t.Run(commandSuite, func(t *testing.T) {
			if commandSuite == "delivery-derived-freshness" || (commandSuite == "delivery-source-addition" || commandSuite == "delivery-source-replacement") {
				testDeliveryDerivedFreshnessCLI(t, root, policy, write, read, run, port, movedPort, commandSuite == "delivery-source-addition" || commandSuite == "delivery-source-replacement", commandSuite == "delivery-source-replacement")
				return
			}
			if commandSuite == "delivery-declaration" {
				testDeliveryDeclarationCLI(t, root, policy, write, read, run)
				return
			}
			testDeliveryProvidersCLI(t, root, policy, write, read, run, commandSuite == "delivery-limits", commandSuite == "delivery-producer", commandSuite == "delivery-routes", commandSuite == "delivery-destination")
		})
		return
	}
	if commandSuite == "backup-telegram-destination" {
		t.Run(commandSuite, func(t *testing.T) {
			policy(false, "daily")
			initial := strings.Replace(string(read(t, "server.yaml")), "    backup_worker:\n", "    backup_worker:\n      notifications:\n        primary: telegram\n        telegram: {token_file: /tmp/wux/token, chat_id_file: /tmp/wux/chat}\n", 1)
			write("server.yaml", initial)
			run(t, "backup-worker", "apply", true)
			var before backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, "manager/.local/state/deploy/backup-worker/setup.json"), &before); err != nil {
				t.Fatal(err)
			}
			write("new-chat", "-100987654321")
			changed := strings.Replace(initial, "chat_id_file: /tmp/wux/chat", "chat_id_file: /tmp/wux/new-chat", 1)
			write("server.yaml", changed)
			run(t, "backup-worker", "apply", true)
			for _, action := range []string{"apply", "resume", "status", "plan"} {
				run(t, "backup-worker", action, true)
			}
			var after backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, "manager/.local/state/deploy/backup-worker/setup.json"), &after); err != nil {
				t.Fatal(err)
			}
			if before.WorkerID != after.WorkerID || before.ArchiveID != after.ArchiveID || !reflect.DeepEqual(before.SourceKeys, after.SourceKeys) || before.RequestHash == after.RequestHash {
				t.Fatal("destination update lost backup identity")
			}
			var installed backup.WorkerInstallation
			if err := json.Unmarshal(read(t, "manager/.config/deploy/backup-worker/runtime.json"), &installed); err != nil {
				t.Fatal(err)
			}
			if installed.Policy.Notifications.Telegram.ChatIDFile != "/tmp/wux/new-chat" || installed.Qualification["notification_delivery"] || string(read(t, "chat")) != "456" {
				t.Fatal("destination publication changed old inputs or invented qualification")
			}
			run(t, "backup-worker", "activate", false)
		})
		return
	}
	if strings.HasPrefix(commandSuite, "shared-policy") {
		t.Run(commandSuite, func(t *testing.T) {
			policy(true, "daily")
			// Exercise every ordinary command once per policy group. Intermediate
			// revisions still check both read-only views; the fault tests cover
			// repeated retries at each durable boundary independently.
			verifyWorkers := func(full bool) {
				actions := []string{"status", "plan"}
				if full {
					actions = []string{"apply", "resume", "status", "plan"}
				}
				for _, w := range []string{"backup-worker", "notification-worker"} {
					for _, action := range actions {
						run(t, w, action, true)
					}
				}
			}

			if strings.HasSuffix(commandSuite, "-commands") {
				startBackupCLILED(t, root+"/led.sock")
				write("led-facts", "synthetic hardware inventory")
				initial := string(read(t, "server.yaml"))
				commands := "    notification_worker:\n      status_leds:\n        enabled: true\n        socket: /tmp/wux/led.sock\n        daemon: {mode: existing}\n        bindings:\n          backup: {producer: backup-worker, subjects: ['*'], led: green}\n      commands:\n        enabled: false\n        mode: mutation\n        workers: {backup: backup-worker}\n        conversations:\n          ops: {provider: telegram, account: '123', native_id: '456'}\n        permissions:\n          - {actor: '789', conversation: ops, worker: backup, permissions: [read, incident.manage], scope: {whole_worker: true}}\n"
				initial = strings.Replace(initial, "    notification_worker:\n", commands, 1)
				initial = strings.Replace(initial, "        telegram:\n", "        telegram:\n          account: '123'\n          bot_name: FixtureBot\n", 1)
				write("server.yaml", initial)
				run(t, "notification-worker", "apply", true)
				draft := strings.Replace(initial, "commands:\n        enabled: false", "commands:\n        enabled: true", 1)
				draft = strings.Replace(draft, "actor: '789'", "actor: '987'", 1)
				write("server.yaml", draft)
				run(t, "notification-worker", "apply", true)
				for _, action := range []string{"resume", "status", "plan"} {
					run(t, "notification-worker", action, true)
				}
				// Ordinary unbound policy changes retain the future command
				// declaration without inventing a backup identity.
				limited := strings.Replace(draft, "    notification_worker:\n", "    notification_worker:\n      limits: {max_messages: 1500}\n", 1)
				write("server.yaml", limited)
				run(t, "notification-worker", "apply", true)
				write("server.yaml", draft)
				run(t, "notification-worker", "apply", true)
				write("server.yaml", initial)
				run(t, "notification-worker", "apply", true)
			}
			run(t, "backup-worker", "apply", true)
			if strings.HasSuffix(commandSuite, "-commands") {
				initial := string(read(t, "server.yaml"))
				for _, enabled := range []bool{false} {
					next := initial
					if !enabled {
						next = strings.Replace(next, "status_leds:\n        enabled: true", "status_leds:\n        enabled: false", 1)
					}
					write("server.yaml", next)
					run(t, "notification-worker", "apply", true)
				}
			}
			original := read(t, "server.yaml")
			setup := "manager/.local/state/deploy/backup-worker/setup.json"
			var before backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, setup), &before); err != nil {
				t.Fatal(err)
			}
			identity := read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")
			enrollment := read(t, "manager/.local/state/deploy/notification-worker/state/bootstrap-enrollment.json")
			for revision, limit := range []int{2000, 800, 1300} {
				next := strings.Replace(string(original), "    notification_worker:\n", fmt.Sprintf("    notification_worker:\n      limits: {max_messages: %d}\n", limit), 1)
				write("server.yaml", next)
				run(t, "notification-worker", "plan", true)
				run(t, "notification-worker", "apply", true)
				verifyWorkers(revision == 2)
				var after backup.WorkerSetupJournal
				if err := json.Unmarshal(read(t, setup), &after); err != nil {
					t.Fatal(err)
				}
				if before.WorkerID != after.WorkerID || before.ArchiveID != after.ArchiveID || !reflect.DeepEqual(before.Producer, after.Producer) || !reflect.DeepEqual(before.SourceKeys, after.SourceKeys) || !reflect.DeepEqual(before.OwnedFiles, after.OwnedFiles) {
					t.Fatal("limits replaced backup authority")
				}
				if string(identity) != string(read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")) || string(enrollment) != string(read(t, "manager/.local/state/deploy/notification-worker/state/bootstrap-enrollment.json")) {
					t.Fatal("limits replaced gateway enrollment")
				}
				manager := read(t, "manager/manager.json")
				run(t, "notification-worker", "apply", true)
				if string(manager) != string(read(t, "manager/manager.json")) {
					t.Fatal("unchanged apply restarted gateway")
				}
			}
			if strings.HasSuffix(commandSuite, "-commands") {
				declared := string(read(t, "server.yaml"))
				// The gateway was already enrolled and updated without commands.
				// Add intake policy without replacing its backup reservation.
				declared = strings.Replace(declared, "commands:\n        enabled: false", "commands:\n        enabled: true", 1)
				write("server.yaml", declared)
				run(t, "notification-worker", "apply", true)
				verifyWorkers(true)
				if string(enrollment) != string(read(t, "manager/.local/state/deploy/notification-worker/state/bootstrap-enrollment.json")) {
					t.Fatal("command enrollment replaced producer identity")
				}
				// Review changed actors, aliases, audiences and capabilities
				// before creating the receiver. The same producer stays reserved.
				changed := strings.Replace(declared, "workers: {backup: backup-worker}", "workers: {renamed: backup-worker}", 1)
				changed = strings.Replace(changed, "worker: backup, permissions:", "worker: renamed, permissions:", 1)
				changed = strings.Replace(changed, "actor: '789'", "actor: '987'", 1)
				changed = strings.Replace(changed, "native_id: '456'", "native_id: '654'", 1)
				changed = strings.Replace(changed, "mode: mutation", "mode: read", 1)
				changed = strings.Replace(changed, "permissions: [read, incident.manage]", "permissions: [read]", 1)
				write("server.yaml", changed)
				run(t, "notification-worker", "apply", true)
				verifyWorkers(true)
				// Expand the still-unadmitted policy to incident mode and its
				// explicitly declared rights; later admission uses this policy.
				changed = strings.Replace(changed, "mode: read", "mode: mutation", 1)
				changed = strings.Replace(changed, "permissions: [read]", "permissions: [read, incident.manage]", 1)
				write("server.yaml", changed)
				run(t, "notification-worker", "apply", true)
				verifyWorkers(true)
				declared = changed
				write("command-token", "123:offline-command-updated-token")
				write("command-chat", "654")
				declared = strings.Replace(declared, "token_file: /tmp/wux/token", "token_file: /tmp/wux/command-token", 1)
				declared = strings.Replace(declared, "chat_id_file: /tmp/wux/chat", "chat_id_file: /tmp/wux/command-chat", 1)
				write("server.yaml", declared)
				run(t, "notification-worker", "apply", true)
				verifyWorkers(true)
				for _, enabled := range []bool{false, true} {
					activity := declared
					if !enabled {
						activity = strings.Replace(activity, "commands:\n        enabled: true", "commands:\n        enabled: false", 1)
					}
					write("server.yaml", activity)
					run(t, "notification-worker", "apply", true)
					verifyWorkers(true)
					if !enabled {
						write("command-token-next", "123:offline-command-next-token")
						activity = strings.Replace(activity, "token_file: /tmp/wux/command-token", "token_file: /tmp/wux/command-token-next", 1)
						declared = strings.Replace(declared, "token_file: /tmp/wux/command-token", "token_file: /tmp/wux/command-token-next", 1)
						write("server.yaml", activity)
						run(t, "notification-worker", "apply", true)
						verifyWorkers(true)
					}
				}
				write("server.yaml", strings.Replace(declared, "status_leds:\n        enabled: false", "status_leds:\n        enabled: true", 1))
				run(t, "notification-worker", "apply", true)
				verifyWorkers(true)
				// Mapping changes after budgets must retain their exact policy
				// predecessor; later timing and route edits use the new layout.
				declared = string(read(t, "server.yaml"))
				mapped := strings.Replace(declared, "subjects: ['*']", "subjects: [source]", 1)
				retained := planTree(t, root+"/manager/.local/state/deploy/worker-control/notification-worker")
				write("server.yaml", strings.Replace(mapped, `schedule: "daily"`, `schedule: "hourly"`, 1))
				run(t, "notification-worker", "apply", false)
				if !reflect.DeepEqual(retained, planTree(t, root+"/manager/.local/state/deploy/worker-control/notification-worker")) {
					t.Fatal("unapplied backup edit retained mapping intent")
				}
				write("server.yaml", mapped)
				run(t, "notification-worker", "apply", true)
				verifyWorkers(true)
			}
			if strings.HasPrefix(commandSuite, "shared-policy") {
				if !strings.HasSuffix(commandSuite, "-commands") {
					// The default producer follows backup freshness. Either short
					// command must coordinate both runtimes and the archive pin.
					original := string(read(t, "server.yaml"))
					for i, freshness := range []string{"48h", "26h"} {
						write("server.yaml", strings.Replace(original, "freshness: 26h", "freshness: "+freshness, 1))
						target := "notification-worker"
						if i == 1 {
							target = "backup-worker"
						}
						run(t, target, "apply", true)
						verifyWorkers(true)
						var installed backup.WorkerInstallation
						if err := json.Unmarshal(read(t, "manager/.config/deploy/backup-worker/runtime.json"), &installed); err != nil {
							t.Fatal(err)
						}
						if installed.Policy.Freshness != freshness {
							t.Fatal("backup freshness did not follow YAML")
						}
						if string(enrollment) != string(read(t, "manager/.local/state/deploy/notification-worker/state/bootstrap-enrollment.json")) {
							t.Fatal("freshness replaced producer identity")
						}
					}

					// Declare the destination before selecting it for backup reports.
					routeBase := string(read(t, "server.yaml"))
					routed := strings.Replace(routeBase, "    notification_worker:\n", "    notification_worker:\n      routes: {alerts: {primary: telegram}, audit: {primary: telegram}}\n", 1)
					write("server.yaml", routed)
					run(t, "notification-worker", "apply", true)
					runtimeBeforeRoute := string(read(t, "manager/.config/deploy/backup-worker/runtime.json"))
					for i, route := range []string{"audit", "alerts"} {
						selected := strings.Replace(routed, "    backup_worker:\n", "    backup_worker:\n      notifications: {worker: manager, route: "+route+"}\n", 1)
						write("server.yaml", selected)
						target := "backup-worker"
						if i == 1 {
							target = "notification-worker"
						}
						run(t, target, "apply", true)
						verifyWorkers(true)
						if runtimeBeforeRoute != string(read(t, "manager/.config/deploy/backup-worker/runtime.json")) {
							t.Fatal("report route rewrote backup runtime")
						}
					}
					write("server.yaml", routeBase)
					run(t, "notification-worker", "apply", true)
					verifyWorkers(true)
				}
				base := string(read(t, "server.yaml"))
				for revision, silence := range []int{120, 180, 300} {
					evidence := 172800
					if silence == 120 {
						evidence = 93600
					}
					declaration := strings.Replace(base, "    notification_worker:\n", fmt.Sprintf("    notification_worker:\n      producers:\n        backup-worker: {routes: [alerts], max_messages: 1000, max_silence_seconds: %d, max_evidence_seconds: %d, policy_health: true}\n", silence, evidence), 1)
					if silence == 120 {
						retained := planTree(t, root+"/manager/.local/state/deploy/worker-control/notification-worker")
						write("server.yaml", strings.Replace(declaration, `schedule: "daily"`, `schedule: "hourly"`, 1))
						run(t, "notification-worker", "apply", false)
						if !reflect.DeepEqual(retained, planTree(t, root+"/manager/.local/state/deploy/worker-control/notification-worker")) {
							t.Fatal("unapplied backup policy retained gateway operation")
						}
					}
					write("server.yaml", declaration)
					manager := read(t, "manager/manager.json")
					run(t, "notification-worker", "plan", true)
					run(t, "notification-worker", "apply", true)
					if silence == 120 && string(manager) != string(read(t, "manager/manager.json")) {
						t.Fatal("explicit unchanged timing restarted gateway")
					}
					verifyWorkers(revision == 2)
				}
				// Interleaved budget updates must retain revised timing policy.
				declared := string(read(t, "server.yaml"))
				write("server.yaml", strings.Replace(declared, "max_messages: 1300", "max_messages: 1400", 1))
				run(t, "notification-worker", "apply", true)
			}
			// Change route declarations after budget and freshness edits, then
			// prove that qualification and admission retain their original pins.
			baseRoutes := string(read(t, "server.yaml"))
			for revision, extra := range []string{"audit", "logs", ""} {
				routes := "      routes: {alerts: {primary: telegram}}\n"
				if extra != "" {
					routes = fmt.Sprintf("      routes: {alerts: {primary: telegram}, %s: {primary: telegram}}\n", extra)
				}
				write("server.yaml", strings.Replace(baseRoutes, "    notification_worker:\n", "    notification_worker:\n"+routes, 1))
				run(t, "notification-worker", "plan", true)
				run(t, "notification-worker", "apply", true)
				verifyWorkers(revision == 2)
			}
			// Explicit producer quotas and capability policy retain the reserved
			// identity; admission is still separate from configuration changes.
			baseProducer := string(read(t, "server.yaml"))
			for revision, extra := range []string{
				"required: false, interactive_context: true, context_kinds: [source], ",
				"required: true, interactive_context: true, context_kinds: [source, worker], ",
				"required: true, ",
			} {
				next := strings.Replace(baseProducer, "backup-worker: {routes:", "backup-worker: {"+extra+"routes:", 1)
				next = strings.Replace(next, "max_messages: 1000", "max_messages: 700", 1)
				write("server.yaml", next)
				run(t, "notification-worker", "plan", true)
				run(t, "notification-worker", "apply", true)
				verifyWorkers(revision == 2)
				if string(identity) != string(read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")) || string(enrollment) != string(read(t, "manager/.local/state/deploy/notification-worker/state/bootstrap-enrollment.json")) {
					t.Fatal("producer policy replaced gateway enrollment")
				}
			}
			if !strings.HasSuffix(commandSuite, "-commands") {
				baseProviders := string(read(t, "server.yaml"))
				for round, chat := range []string{"456", "987", "456"} {
					tokenName := fmt.Sprintf("token-%d", round)
					chatName := fmt.Sprintf("chat-%d", round)
					write(tokenName, fmt.Sprintf("123:rotated-fixture-%d", round))
					write(chatName, chat)
					next := strings.Replace(baseProviders, "token_file: /tmp/wux/token", "token_file: /tmp/wux/"+tokenName, 1)
					next = strings.Replace(next, "chat_id_file: /tmp/wux/chat", "chat_id_file: /tmp/wux/"+chatName, 1)
					write("server.yaml", next)
					if round == 0 {
						saved := read(t, "token")
						write("token", "123:unreviewed-original")
						tree := planTree(t, root+"/manager/.local/state/deploy/worker-control/notification-worker")
						run(t, "notification-worker", "apply", false)
						if !reflect.DeepEqual(tree, planTree(t, root+"/manager/.local/state/deploy/worker-control/notification-worker")) {
							t.Fatal("changed original credential retained provider update")
						}
						write("token", string(saved))
					}
					run(t, "notification-worker", "plan", true)
					run(t, "notification-worker", "apply", true)
					verifyWorkers(round == 2)
				}
				// A later budget update must retain the selected provider pins.
				write("server.yaml", strings.Replace(string(read(t, "server.yaml")), "max_messages: 1400", "max_messages: 1450", 1))
				run(t, "notification-worker", "apply", true)
			}
			if strings.HasSuffix(commandSuite, "-commands") {
				// Return to the original selectors after several policy groups,
				// then commit another budget to exercise a second mapping gap.
				declared := string(read(t, "server.yaml"))
				write("server.yaml", strings.Replace(declared, "subjects: [source]", "subjects: ['*']", 1))
				run(t, "notification-worker", "apply", true)
				verifyWorkers(true)
				write("server.yaml", strings.Replace(string(read(t, "server.yaml")), "max_messages: 1400", "max_messages: 1420", 1))
				run(t, "notification-worker", "apply", true)
				verifyWorkers(true)
			}
			// Exercise actual short qualification after the migration, using
			// explicitly synthetic evidence confined to this offline fixture.
			run(t, "backup-worker", "activate", false)
			const coordinator = "manager/.local/state/deploy/worker-control/notification-worker/"
			var enrolled OperationJournal
			if err := json.Unmarshal(read(t, coordinator+"operation.json"), &enrolled); err != nil {
				t.Fatal(err)
			}
			gateway := enrolled.Pins.Installations["notification-worker"]
			observed, err := install.InspectInstallation(context.Background(), gateway)
			if err != nil {
				t.Fatal(err)
			}
			session, err := install.BeginQualification(context.Background(), gateway, protocol.Hash(observed.Manifest))
			if err != nil {
				t.Fatal(err)
			}
			write("limits-evidence.log", "synthetic offline observations; no live provider checks")
			var answers strings.Builder
			if session.LEDEnvironmentRequired {
				write("limits-led-config.json", "synthetic offline LED configuration")
				answers.WriteString("synthetic-v1\n" + root + "/limits-led-config.json\n")
			}
			for range session.Checks {
				answers.WriteString("passed\n" + root + "/limits-evidence.log\n")
			}
			qualifyNotificationThroughTerminal(t, root, answers.String(), session.LEDEnvironmentRequired)
			run(t, "notification-worker", "apply", true)
			for _, w := range []string{"backup-worker", "notification-worker"} {
				for _, action := range []string{"apply", "resume", "status", "plan"} {
					run(t, w, action, true)
				}
			}
			if commandSuite == "shared-policy" {
				before, err := install.InspectInstallation(context.Background(), gateway)
				testutil.Must(t, err)
				enrollmentBefore := read(t, coordinator+"consumer-enrollment.json")
				for range 2 {
					qualifyNotificationThroughTerminal(t, root, answers.String(), false)
					paused, err := install.InspectInstallation(context.Background(), gateway)
					testutil.Must(t, err)
					if paused.Manifest.Mode != "verification" || paused.Manifest.DeliveryOwnerHash != before.Manifest.DeliveryOwnerHash {
						t.Fatal("short qualification did not retain paused delivery owner")
					}
					for _, action := range []string{"status", "plan"} {
						run(t, "notification-worker", action, true)
					}
					run(t, "notification-worker", "apply", true)
					verifyWorkers(true)
					renewed, err := install.InspectInstallation(context.Background(), gateway)
					testutil.Must(t, err)
					if renewed.Manifest.Mode != "delivery" || renewed.Manifest.Identity != before.Manifest.Identity || renewed.Manifest.DeliveryOwnerHash != before.Manifest.DeliveryOwnerHash || string(enrollmentBefore) != string(read(t, coordinator+"consumer-enrollment.json")) {
						t.Fatal("requalification replaced ownership or enrollment")
					}
				}
			}
			// Apply one production change at a time; mixed activity and budget edits
			// must not retain an operation or pause an otherwise delivering gateway.
			retained := planTree(t, root+"/"+coordinator)
			oldLimit := "max_messages: 1300"
			if strings.HasPrefix(commandSuite, "shared-policy") {
				oldLimit = "max_messages: 1420"
				if !strings.HasSuffix(commandSuite, "-commands") {
					oldLimit = "max_messages: 1450"
				}
			}
			mixed := strings.Replace(string(read(t, "server.yaml")), oldLimit, "max_messages: 1500", 1)
			mixed = strings.Replace(mixed, "    notification_worker:\n", "    notification_worker:\n      enabled: false\n", 1)
			write("server.yaml", mixed)
			run(t, "notification-worker", "apply", false)
			if !reflect.DeepEqual(retained, planTree(t, root+"/"+coordinator)) {
				t.Fatal("unsupported production change retained an operation")
			}

		})
		return
	}
	// Exercise both installation orders with independent retained homes. Namespace
	// teardown kills any fixture descendants even if an assertion fails.
	for _, notificationFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("notification-first-%t", notificationFirst), func(t *testing.T) {
			policy(true, "daily")
			if notificationFirst {
				v := run(t, "notification-worker", "apply", true)
				if v["state"] != "waiting_for_producers" {
					t.Fatal(v)
				}
			}
			setup := "manager/.local/state/deploy/backup-worker/setup.json"
			identity := "manager/.local/state/deploy/notification-worker/state/identity.json"
			write("lose-source-reply", "once")
			run(t, "backup-worker", "apply", false)
			var prepared backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, setup), &prepared); err != nil || prepared.WorkerID == "" || prepared.Stage == "complete" || prepared.SourceKeys["source"] == "" {
				t.Fatal("lost enrollment reply did not retain prepared authority", err)
			}
			reservedGateway := read(t, identity)
			v := run(t, "backup-worker", "apply", true)
			if v["state"] != "qualification_required" {
				t.Fatal(v)
			}
			originalSetup, originalIdentity := read(t, setup), read(t, identity)
			originalManager := read(t, "manager/manager.json")
			for _, worker := range []string{"backup-worker", "notification-worker"} {
				for _, action := range []string{"apply", "resume", "status", "plan"} {
					run(t, worker, action, true)
				}
			}
			if string(read(t, setup)) != string(originalSetup) || string(read(t, identity)) != string(originalIdentity) || string(read(t, "manager/manager.json")) != string(originalManager) {
				t.Fatal("unchanged CLI replaced setup/gateway authority or restarted its service")
			}
			inventory := read(t, "server.yaml")
			sourceCalls := read(t, "source-calls.log")
			write("server.yaml", strings.Replace(string(inventory), "host: 127.0.0.1", "host: 127.0.0.3", 1))
			endpointPlan := run(t, "backup-worker", "plan", true)
			if endpointPlan["code"] != "source_migration_required" || string(read(t, "source-calls.log")) != string(sourceCalls) || string(read(t, setup)) != string(originalSetup) || string(read(t, "manager/manager.json")) != string(originalManager) {
				t.Fatal("endpoint planning hid the change, contacted a source or changed runtime state", endpointPlan)
			}
			if got := run(t, "backup-worker", "status", true); got["code"] != "source_migration_required" {
				t.Fatal("status hid changed source endpoint", got)
			}
			if got := run(t, "backup-worker", "run", false); got["code"] != "source_migration_required" {
				t.Fatal("manual run accepted old source endpoint", got)
			}
			run(t, "backup-worker", "activate", false)
			run(t, "backup-worker", "qualify", false)
			if string(read(t, "source-calls.log")) != string(sourceCalls) || string(read(t, setup)) != string(originalSetup) || string(read(t, "manager/manager.json")) != string(originalManager) {
				t.Fatal("endpoint mismatch changed worker activity or contacted source")
			}
			write("server.yaml", string(inventory))
			var j backup.WorkerSetupJournal
			if err := json.Unmarshal(originalSetup, &j); err != nil || j.Producer == nil || j.Stage != "complete" {
				t.Fatal("missing real producer reservation", err)
			}
			if j.WorkerID != prepared.WorkerID || j.ArchiveID != prepared.ArchiveID || j.SourceKeys["source"] != prepared.SourceKeys["source"] || string(originalIdentity) != string(reservedGateway) {
				t.Fatal("lost source reply regenerated identity or key")
			}
			var sourceIdentity source.SourceIdentity
			if err := json.Unmarshal(read(t, "app/.local/state/deploy/backup-source/identity.json"), &sourceIdentity); err != nil || sourceIdentity.Workers[j.WorkerID].Fingerprint != j.SourceKeys["source"] {
				t.Fatal("source did not enroll the worker's reserved key", err)
			}
			// A staged setup must never be mistaken for production activation.
			run(t, "backup-worker", "activate", false)
			v = run(t, "backup-worker", "run", true)
			if v["state"] != "start_requested" {
				t.Fatal(v)
			}
			// Ordinary backup policy updates retain the same gateway scope/producer.
			policy(true, "weekly")
			run(t, "backup-worker", "apply", false)
			if string(read(t, setup)) != string(originalSetup) {
				t.Fatal("invalid schedule changed retained setup")
			}
			policy(true, "hourly")
			run(t, "backup-worker", "apply", true)
			var updated backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, setup), &updated); err != nil || updated.WorkerID != j.WorkerID || updated.ArchiveID != j.ArchiveID || updated.Producer == nil || *updated.Producer != *j.Producer || string(read(t, identity)) != string(originalIdentity) {
				t.Fatal("ordinary update replaced shared identity", err)
			}
			run(t, "backup-worker", "resume", true)
			if string(read(t, "manager/manager.json")) != string(originalManager) {
				t.Fatal("backup policy update restarted the unchanged gateway")
			}
			// Move the endpoint to the same source account. The source helper is
			// inspected without enrollment writes, even after a lost reply.
			beforeSource := planTree(t, root+"/app")
			inventory = read(t, "server.yaml")
			movedInventory := strings.Replace(string(inventory), "host: 127.0.0.1", "host: 127.0.0.3", 1)
			movedInventory = strings.Replace(movedInventory, fmt.Sprintf("ssh_port: %d", port), fmt.Sprintf("ssh_port: %d", movedPort), 1)
			write("server.yaml", movedInventory)
			write("lose-source-reply", "once")
			run(t, "backup-worker", "apply", false)
			if !reflect.DeepEqual(beforeSource, planTree(t, root+"/app")) {
				t.Fatal("endpoint inspection changed source ownership")
			}
			retainedSourceIdentity := read(t, "app/.local/state/deploy/backup-source/identity.json")
			foreign := sourceIdentity
			foreign.ID = "s_" + strings.Repeat("f", 32)
			foreignRaw, err := json.Marshal(foreign)
			if err != nil {
				t.Fatal(err)
			}
			write("app/.local/state/deploy/backup-source/identity.json", string(foreignRaw))
			foreignTree := planTree(t, root+"/app")
			run(t, "backup-worker", "resume", false)
			if !reflect.DeepEqual(foreignTree, planTree(t, root+"/app")) {
				t.Fatal("foreign source was enrolled or repaired")
			}
			write("app/.local/state/deploy/backup-source/identity.json", string(retainedSourceIdentity))
			run(t, "backup-worker", "resume", true)
			afterSource := planTree(t, root+"/app")
			// Restricted capability qualification legitimately consumes source
			// rate budget. Every ownership/key/helper file must remain identical.
			delete(beforeSource, root+"/app/.local/state/deploy/backup-source/rate.json")
			delete(afterSource, root+"/app/.local/state/deploy/backup-source/rate.json")
			if !reflect.DeepEqual(beforeSource, afterSource) || string(read(t, identity)) != string(originalIdentity) || string(read(t, "manager/manager.json")) != string(originalManager) {
				t.Fatal("endpoint update mutated source or gateway authority")
			}
			var moved backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, setup), &moved); err != nil || moved.WorkerID != updated.WorkerID || moved.ArchiveID != updated.ArchiveID || !reflect.DeepEqual(moved.SourceKeys, updated.SourceKeys) || !reflect.DeepEqual(moved.SourceEnrollments, updated.SourceEnrollments) {
				t.Fatal("endpoint move changed retained identities", err)
			}
			var movedRuntime backup.WorkerInstallation
			if err := json.Unmarshal(read(t, "manager/.config/deploy/backup-worker/runtime.json"), &movedRuntime); err != nil || movedRuntime.Sources[0].Host != "127.0.0.3" || movedRuntime.Sources[0].Port != movedPort {
				t.Fatal("new endpoint was not published", err)
			}
			if result := run(t, "backup-worker", "plan", true); result["state"] != "up_to_date" {
				t.Fatal("moved endpoint still differs", result)
			}
			// Loss of setup authority must never initialize another worker/archive
			// or replace the already enrolled gateway producer.
			if err := os.Rename(root+"/"+setup, root+"/"+setup+".retained"); err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{"status", "apply"} {
				run(t, "backup-worker", action, false)
			}
			if _, err := os.Stat(root + "/" + setup); !os.IsNotExist(err) {
				t.Fatal("missing setup journal was recreated", err)
			}
			if string(read(t, identity)) != string(originalIdentity) || string(read(t, "manager/manager.json")) != string(originalManager) {
				t.Fatal("missing backup authority altered its shared gateway")
			}
			if err := os.Rename(root+"/"+setup+".retained", root+"/"+setup); err != nil {
				t.Fatal(err)
			}
			// Stop the real fixture gateway before archiving these disposable homes.
			cmd := exec.Command(root+"/systemctl", "--user", "stop", "deploy-notification-worker.service")
			if out, err := process.LimitedOutput(cmd, 45*time.Second, 4096); err != nil {
				t.Fatal(err, string(out))
			}
			for _, name := range []string{"manager", "app"} {
				if err := os.Rename(root+"/"+name, fmt.Sprintf("%s/%s-%t", root, name, notificationFirst)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root+"/"+name, 0700); err != nil {
					t.Fatal(err)
				}
			}
		})
		if t.Failed() {
			return
		}
	}
	for _, shared := range []bool{false, true} {
		name := "source-retirement"
		if commandSuite == "source-replacement" {
			name = "source-replacement"
		}
		if commandSuite == "source-combined" {
			name = "source-combined"
		}
		t.Run(fmt.Sprintf("shared-%t-%s", shared, name), func(t *testing.T) {
			port2, public2 := startBackupCLIRestrictionServer(t, root+"/app2", "127.0.0.4")
			known := read(t, "known_hosts")
			write("known_hosts", string(known)+fmt.Sprintf("[127.0.0.4]:%d %s", port2, ssh.MarshalAuthorizedKey(public2)))
			policy(shared, "daily")
			initial := strings.Replace(string(read(t, "server.yaml")), "sources: [source]", "sources: [source, retired]", 1)
			initial += fmt.Sprintf("  retired:\n    host: 127.0.0.4\n    user: app2\n    ssh_port: %d\n    ssh_known_hosts: /tmp/wux/known_hosts\n", port2)
			var original *backup.WorkerSetupJournal
			if commandSuite == "source-replacement" || commandSuite == "source-combined" {
				write("server.yaml", strings.Replace(initial, "sources: [source, retired]", "sources: [retired]", 1))
				run(t, "backup-worker", "apply", true)
				original = &backup.WorkerSetupJournal{}
				if err := json.Unmarshal(read(t, "manager/.local/state/deploy/backup-worker/setup.json"), original); err != nil {
					t.Fatal(err)
				}
			}
			if commandSuite != "source-combined" {
				write("server.yaml", initial)
				run(t, "backup-worker", "apply", true)
			}
			setup := "manager/.local/state/deploy/backup-worker/setup.json"
			var before backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, setup), &before); err != nil {
				t.Fatal(err)
			}
			if original != nil && commandSuite != "source-combined" && (before.WorkerID != original.WorkerID || before.ArchiveID != original.ArchiveID || !reflect.DeepEqual(before.Producer, original.Producer) || before.SourceKeys["retired"] != original.SourceKeys["retired"] || len(before.SourceKeys) != 2) {
				t.Fatal("replacement addition changed original worker or source")
			}
			var sourceIdentity []byte
			if commandSuite != "source-combined" {
				sourceIdentity = read(t, "app/.local/state/deploy/backup-source/identity.json")
			}
			retiring := strings.Replace(initial, "sources: [source, retired]", "sources: [source]\n      retire_sources: {retired: service decommissioned}", 1)
			write("server.yaml", retiring)
			plan := run(t, "backup-worker", "plan", true)
			if !strings.Contains(fmt.Sprint(plan["next"]), "retire_sources") {
				t.Fatal("missing retirement guidance", plan)
			}
			pendingCode := "source_retirement_pending"
			if commandSuite == "source-combined" {
				pendingCode = "source_replacement_pending"
				write("lose-source-reply", "once")
				run(t, "backup-worker", "apply", false)
				var pending backup.WorkerSetupJournal
				if err := json.Unmarshal(read(t, setup), &pending); err != nil {
					t.Fatal(err)
				}
				if pending.SourceExpansion == nil || pending.SourceRetirement == nil || pending.WorkerID != before.WorkerID {
					t.Fatal("lost enrollment forgot combined source operation")
				}
				var oldSource source.SourceIdentity
				if err := json.Unmarshal(read(t, "app2/.local/state/deploy/backup-source/identity.json"), &oldSource); err != nil {
					t.Fatal(err)
				}
				if !oldSource.Workers[before.WorkerID].Enabled {
					t.Fatal("old source revoked before replacement was installed")
				}
				sourceIdentity = read(t, "app/.local/state/deploy/backup-source/identity.json")
				if got := run(t, "backup-worker", "status", true); got["code"] != pendingCode {
					t.Fatal(got)
				}
				write("server.yaml", strings.Replace(retiring, "host: 127.0.0.1", "host: 127.0.0.3", 1))
				run(t, "backup-worker", "apply", false)
				write("server.yaml", retiring)
			}
			write("lose-retirement-reply", "once")
			run(t, "backup-worker", "apply", false)
			var remote source.SourceIdentity
			if err := json.Unmarshal(read(t, "app2/.local/state/deploy/backup-source/identity.json"), &remote); err != nil {
				t.Fatal(err)
			}
			if remote.Workers[before.WorkerID].Enabled || remote.Workers[before.WorkerID].RetirementReason != "service decommissioned" {
				t.Fatal("lost reply did not retain exact disabled authorization")
			}
			if got := run(t, "backup-worker", "plan", true); got["code"] != pendingCode {
				t.Fatal(got)
			}
			if got := run(t, "backup-worker", "activate", false); got["code"] != pendingCode {
				t.Fatal("retirement activation guidance lost", got)
			}
			if got := run(t, "backup-worker", "status", true); got["code"] != pendingCode {
				t.Fatal("pending retirement status hidden", got)
			}
			write("server.yaml", strings.Replace(retiring, "host: 127.0.0.4", "host: 127.0.0.5", 1))
			run(t, "backup-worker", "apply", false)
			write("server.yaml", retiring)
			run(t, "backup-worker", "resume", true)
			var completed backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, setup), &completed); err != nil {
				t.Fatal(err)
			}
			if completed.WorkerID != before.WorkerID || completed.ArchiveID != before.ArchiveID || len(completed.SourceKeys) != 1 || completed.RetiredSources["retired"].SourceID != remote.ID {
				t.Fatal("retirement lost retained identity")
			}
			if !bytes.Equal(sourceIdentity, read(t, "app/.local/state/deploy/backup-source/identity.json")) {
				t.Fatal("retirement changed remaining source authorization")
			}
			retained := read(t, setup)
			run(t, "backup-worker", "apply", true)
			if !bytes.Equal(retained, read(t, setup)) {
				t.Fatal("completed retirement retry changed journal")
			}
			if got := run(t, "backup-worker", "status", true); got["code"] != "ok" {
				t.Fatal(got)
			}
			write("server.yaml", strings.Split(retiring, "\n  retired:\n")[0]+"\n")
			run(t, "backup-worker", "apply", true)
			if !reflect.DeepEqual(completed.Producer, before.Producer) {
				t.Fatal("retirement replaced shared producer")
			}
			if shared {
				if commandSuite == "source-combined" {
					for _, action := range []string{"apply", "resume", "status", "plan"} {
						run(t, "notification-worker", action, true)
					}
				}
				cmd := exec.Command(root+"/systemctl", "--user", "stop", "deploy-notification-worker.service")
				if out, err := process.LimitedOutput(cmd, 45*time.Second, 4096); err != nil {
					t.Fatal(err, string(out))
				}
			}
			for _, name := range []string{"manager", "app", "app2"} {
				if err := os.Rename(root+"/"+name, fmt.Sprintf("%s/%s-retirement-%t", root, name, shared)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root+"/"+name, 0700); err != nil {
					t.Fatal(err)
				}
			}

		})

		if t.Failed() {
			return
		}
	}

	for _, commandCase := range []struct {
		mode                                                       string
		activity, permissions, enrollment, helper, requalification bool
	}{
		{mode: "read"}, {mode: "mutation"},
		{mode: "read", activity: true}, {mode: "mutation", activity: true},
		{mode: "read", activity: true, permissions: true}, {mode: "mutation", activity: true, permissions: true},
		{mode: "read", activity: true, permissions: true, enrollment: true}, {mode: "mutation", activity: true, permissions: true, enrollment: true},
		{mode: "read", activity: true, helper: true}, {mode: "mutation", activity: true, helper: true},
		{mode: "read", activity: true, requalification: true}, {mode: "mutation", activity: true, requalification: true},
	} {
		suite := ""
		if commandCase.permissions {
			suite = "permissions"
		}
		if commandCase.enrollment {
			suite = "enrollment-changes"
		}
		if commandCase.helper {
			suite = "helper-upgrade"
		}
		if commandCase.requalification {
			suite = "requalification"
			if commandSuite == "command-limits" {
				suite = commandSuite
			}
		}
		if suite != commandSuite {
			continue
		}
		commandMode := commandCase.mode
		suffix := "-command-enrollment"
		if commandCase.activity {
			suffix = "-command-activity"
		}
		if commandCase.permissions {
			suffix = "-command-permissions"
		}
		if commandCase.enrollment {
			suffix = "-command-enrollment-changes"
		}
		if commandCase.helper {
			suffix = "-command-helper-upgrade"
		}
		if commandCase.requalification {
			suffix = "-command-" + commandSuite
		}
		t.Run(commandMode+suffix, func(t *testing.T) {
			policy(true, "daily")
			base := string(read(t, "server.yaml"))
			base = strings.Replace(base, "        telegram:\n", "        telegram:\n          account: '123'\n          bot_name: FixtureBot\n", 1)
			commands := "    notification_worker:\n      commands:\n        enabled: true\n        mode: read\n        workers: {backup: backup-worker}\n        conversations:\n          ops: {provider: telegram, account: '123', native_id: '456'}\n        permissions:\n          - {actor: '789', conversation: ops, worker: backup, permissions: [read], scope: {whole_worker: true}}\n"
			if commandMode == "mutation" {
				commands = strings.Replace(commands, "mode: read", "mode: mutation", 1)
				commands = strings.Replace(commands, "permissions: [read]", "permissions: [read, incident.manage]", 1)
			}
			if commandCase.enrollment {
				commands = strings.Replace(commands, "        permissions:\n", "          retired: {provider: telegram, account: '123', native_id: '457'}\n        permissions:\n", 1)
				commands += "          - {actor: '790', conversation: retired, worker: backup, permissions: [read], scope: {whole_worker: true}}\n"
			}
			declared := strings.Replace(base, "    notification_worker:\n", commands, 1)
			write("server.yaml", strings.Replace(declared, "          bot_name: FixtureBot\n", "", 1))
			missing := run(t, "notification-worker", "apply", false)
			if missing["state"] != "needs_input" || missing["code"] != "missing_input" {
				t.Fatal("missing bot username bypassed guided input reporting", missing)
			}
			if _, err := os.Stat(root + "/manager/.local/state/deploy/notification-worker/state/identity.json"); !os.IsNotExist(err) {
				t.Fatal("missing input initialized gateway", err)
			}
			write("server.yaml", declared)
			v := run(t, "notification-worker", "apply", true)
			if v["state"] != "waiting_for_producers" {
				t.Fatal(v)
			}
			identity := read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")
			run(t, "backup-worker", "apply", true)
			var input install.RuntimeInput
			if err := json.Unmarshal(read(t, "manager/.config/deploy/notification-worker/consumers/runtime.json"), &input); err != nil {
				t.Fatal(err)
			}
			if input.Bootstrap || input.Worker.Interaction == nil || input.Telegram == nil || len(input.Worker.Interaction.Workers) != 1 || input.Worker.Interaction.Workers[0].Alias != "backup" {
				t.Fatal("real short CLI lost command enrollment")
			}
			for _, descriptor := range input.Worker.Interaction.Workers[0].Commands {
				if commandMode == "read" && descriptor.Effect != "read" {
					t.Fatal("short command enrollment granted mutation")
				}
			}
			if commandMode == "mutation" && input.Worker.Interaction.Workers[0].Commands["ack"].Permission != "incident.manage" {
				t.Fatal("short CLI lost explicit incident management")
			}
			journal := read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json")
			manager := read(t, "manager/manager.json")
			for _, worker := range []string{"backup-worker", "notification-worker"} {
				for _, action := range []string{"apply", "resume", "status", "plan"} {
					run(t, worker, action, true)
				}
			}
			if !bytes.Equal(identity, read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")) || !bytes.Equal(journal, read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json")) || !bytes.Equal(manager, read(t, "manager/manager.json")) {
				t.Fatal("command enrollment retry changed ownership or service lifetime")
			}
			// Without recorded qualification, commands cannot proceed to receiver setup.
			if _, err := os.Stat(root + "/manager/.local/state/deploy/backup-worker/state/command-receiver.json"); !os.IsNotExist(err) {
				t.Fatal("unqualified command receiver installed", err)
			}
			if commandCase.activity {
				// This isolated fixture supplies synthetic qualification to exercise
				// admission plumbing. It has no external network or live credentials.
				ctx := context.Background()
				var enrolled OperationJournal
				if err := json.Unmarshal(journal, &enrolled); err != nil {
					t.Fatal(err)
				}
				gateway := enrolled.Pins.Installations["notification-worker"]
				observation, err := install.InspectInstallation(ctx, gateway)
				if err != nil {
					t.Fatal(err)
				}
				manifest := protocol.Hash(observation.Manifest)
				session, err := install.BeginQualification(ctx, gateway, manifest)
				if err != nil {
					t.Fatal(err)
				}
				for _, check := range session.Checks {
					_, err = install.ObserveQualification(ctx, gateway, manifest, install.QualificationObservation{Session: session.ID, Scope: check.Scope, ID: check.ID, Result: "passed", EvidenceHash: protocol.Hash("offline fixture: synthetic admission evidence")})
					if err != nil {
						t.Fatal(err)
					}
				}
				session, err = install.CommitQualification(ctx, gateway, manifest, session.ID)
				if err != nil || !session.EvidenceComplete {
					t.Fatal("fixture qualification failed", session, err)
				}
				v := run(t, "notification-worker", "apply", true)
				if v["state"] != "commands_admitted" {
					t.Fatal("initial fixture admission failed", v)
				}
				receiverName := "manager/.local/state/deploy/backup-worker/state/command-receiver.json"
				receiver := read(t, receiverName)
				var receiverRecord struct {
					Request backup.CommandReceiverRequest `json:"request"`
				}
				if err := json.Unmarshal(receiver, &receiverRecord); err != nil {
					t.Fatal(err)
				}
				if commandCase.requalification {
					testCommandRequalificationCLI(t, root, write, read, run, len(session.Checks), commandSuite == "command-limits")
				} else if commandCase.helper {
					testSharedCommandHelperCLI(t, root, write, read, run, receiverRecord.Request)
				} else {
					wideDeclared, reduced := declared, false
					for _, enabled := range []bool{false, true, false} {
						policy := declared
						if !enabled {
							policy = strings.Replace(policy, "        enabled: true\n", "        enabled: false\n", 1)
						}
						write("server.yaml", policy)
						run(t, "notification-worker", "plan", true)
						want := "commands_disabled"
						if enabled {
							want = "commands_admitted"
						}
						for _, action := range []string{"apply", "resume", "status", "plan"} {
							v = run(t, "notification-worker", action, true)
							if action != "plan" && v["state"] != want {
								t.Fatal("command activity status mismatch", action, v)
							}
						}
						for _, action := range []string{"apply", "resume", "status", "plan"} {
							run(t, "backup-worker", action, true)
						}
						if commandCase.permissions && !reduced {
							if commandCase.enrollment {
								declared = strings.ReplaceAll(declared, "workers: {backup: backup-worker}", "workers: {archive: backup-worker}")
								declared = strings.ReplaceAll(declared, "worker: backup,", "worker: archive,")
								declared = strings.Replace(declared, "          retired: {provider: telegram, account: '123', native_id: '457'}\n", "", 1)
								declared = strings.Replace(declared, "          - {actor: '790', conversation: retired, worker: archive, permissions: [read], scope: {whole_worker: true}}\n", "", 1)
								write("server.yaml", strings.Replace(declared, "        enabled: true\n", "        enabled: false\n", 1))
								for _, action := range []string{"plan", "apply", "resume", "status", "plan"} {
									run(t, "notification-worker", action, true)
								}
								for _, action := range []string{"apply", "resume", "status", "plan"} {
									run(t, "backup-worker", action, true)
								}
							}

							declared = strings.Replace(declared, "scope: {whole_worker: true}", "scope: {subjects: [source/source]}", 1)
							declared = strings.Replace(declared, "permissions: [read, incident.manage]", "permissions: [read]", 1)
							write("server.yaml", strings.Replace(declared, "        enabled: true\n", "        enabled: false\n", 1))
							for _, action := range []string{"plan", "apply", "resume", "status", "plan"} {
								run(t, "notification-worker", action, true)
							}
							for _, action := range []string{"apply", "resume", "status", "plan"} {
								run(t, "backup-worker", action, true)
							}
							if commandCase.enrollment && commandMode == "mutation" {
								declared = strings.Replace(declared, "mode: mutation", "mode: read", 1)
								write("server.yaml", strings.Replace(declared, "        enabled: true\n", "        enabled: false\n", 1))
								for _, action := range []string{"plan", "apply", "resume", "status", "plan"} {
									run(t, "notification-worker", action, true)
								}
								for _, action := range []string{"apply", "resume", "status", "plan"} {
									run(t, "backup-worker", action, true)
								}
							}
							reduced = true
						}
						if reduced {
							o, err := install.InspectInstallation(ctx, gateway)
							if err != nil {
								t.Fatal(err)
							}
							policy, err := install.InspectManagedCommandPolicy(ctx, gateway, protocol.Hash(o.Manifest), receiverRecord.Request.Producer)
							if err != nil {
								t.Fatal(err)
							}
							if commandCase.enrollment && enabled && o.Manifest.Commands.Mode != "read" {
								t.Fatal("mode downgrade was not admitted", o.Manifest.Commands.Mode)
							}
							if commandCase.enrollment && (len(policy.Destinations) != 1 || policy.Destinations[0].ID != "ops" || len(policy.Workers) != 1 || policy.Workers[0].Alias != "archive") {
								t.Fatal("enrollment migration not retained", policy)
							}
							if len(policy.Permissions) != 1 || !reflect.DeepEqual(policy.Permissions[0].Permissions, []string{"read"}) || policy.Permissions[0].Scope.WholeWorker || !reflect.DeepEqual(policy.Permissions[0].Scope.Subjects, []string{"source/source"}) {
								t.Fatal("permission reduction was not retained", policy.Permissions)
							}
						}
						if !bytes.Equal(receiver, read(t, receiverName)) || !bytes.Equal(identity, read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")) {
							t.Fatal("command toggle rewrote receiver or identity")
						}
					}
					if commandCase.permissions {
						before := read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json")
						write("server.yaml", strings.Replace(strings.Replace(wideDeclared, "actor: '789'", "actor: '999'", 1), "        enabled: true\n", "        enabled: false\n", 1))
						run(t, "notification-worker", "apply", false)
						if !bytes.Equal(before, read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json")) {
							t.Fatal("rejected expansion changed coordinator")
						}
						write("server.yaml", strings.Replace(declared, "        enabled: true\n", "        enabled: false\n", 1))
						run(t, "notification-worker", "resume", true)
					}
					if commandCase.enrollment {
						disabled := strings.Replace(declared, "        enabled: true\n", "        enabled: false\n", 1)
						mutation := strings.Replace(disabled, "mode: read", "mode: mutation", 1)
						before := read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json")
						write("server.yaml", mutation)
						if commandMode == "read" {
							run(t, "notification-worker", "apply", false)
							if !bytes.Equal(before, read(t, "manager/.local/state/deploy/worker-control/notification-worker/operation.json")) {
								t.Fatal("unsupported mode changed coordinator")
							}
							write("server.yaml", disabled)
						} else {
							for _, enabled := range []bool{false, true, false} {
								policy := mutation
								if enabled {
									policy = strings.Replace(policy, "        enabled: false\n", "        enabled: true\n", 1)
								}
								write("server.yaml", policy)
								for _, action := range []string{"apply", "resume", "status", "plan"} {
									run(t, "notification-worker", action, true)
								}
								for _, action := range []string{"apply", "status"} {
									run(t, "backup-worker", action, true)
								}
								o, err := install.InspectInstallation(ctx, gateway)
								if err != nil {
									t.Fatal(err)
								}
								if enabled && o.Manifest.Commands.Mode != "all" {
									t.Fatal("original mutation mode not restored")
								}
								p, err := install.InspectManagedCommandPolicy(ctx, gateway, protocol.Hash(o.Manifest), receiverRecord.Request.Producer)
								if err != nil || len(p.Permissions) != 1 || !reflect.DeepEqual(p.Permissions[0].Permissions, []string{"read"}) {
									t.Fatal("mode change restored rights", err)
								}
							}
						}
					}
					if commandCase.permissions {
						// Explicitly restore only authority already admitted by the receiver.
						restored := strings.Replace(string(read(t, "server.yaml")), "scope: {subjects: [source/source]}", "scope: {whole_worker: true}", 1)
						if commandMode == "mutation" {
							restored = strings.Replace(restored, "permissions: [read]", "permissions: [read, incident.manage]", 1)
						}
						write("server.yaml", restored)
						for _, worker := range []string{"notification-worker", "backup-worker"} {
							for _, action := range []string{"apply", "resume", "status", "plan"} {
								run(t, worker, action, true)
							}
						}
						o, e := install.InspectInstallation(ctx, gateway)
						if e != nil {
							t.Fatal(e)
						}
						p, e := install.InspectManagedCommandPolicy(ctx, gateway, protocol.Hash(o.Manifest), receiverRecord.Request.Producer)
						if e != nil || len(p.Permissions) != 1 || !p.Permissions[0].Scope.WholeWorker || o.Manifest.Commands.Mode != "drain" {
							t.Fatal("permission restoration lost", p, e)
						}
						write("server.yaml", strings.Replace(restored, "        enabled: false\n", "        enabled: true\n", 1))
						for _, action := range []string{"apply", "resume", "status", "plan"} {
							run(t, "notification-worker", action, true)
						}
						run(t, "backup-worker", "apply", true)
						if !bytes.Equal(receiver, read(t, receiverName)) {
							t.Fatal("restoration replaced original receiver")
						}
					}
				}
			}
			cmd := exec.Command(root+"/systemctl", "--user", "stop", "deploy-notification-worker.service")
			if out, err := process.LimitedOutput(cmd, 45*time.Second, 4096); err != nil {
				t.Fatal(err, string(out))
			}
			for _, name := range []string{"manager", "app"} {
				if err := os.Rename(root+"/"+name, root+"/"+name+"-"+commandMode+suffix); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root+"/"+name, 0700); err != nil {
					t.Fatal(err)
				}
			}
		})
		if t.Failed() {
			return
		}

	}

	if commandSuite == "delivery-output" {
		testProductionLEDCLI(t, root, policy, write, read, run)
	}

	t.Run("enrolled-led-policy", func(t *testing.T) {
		startBackupCLILED(t, root+"/led.sock")
		write("led-facts", "synthetic hardware inventory")
		defer os.Remove(root + "/led-facts")
		policy(true, "daily")
		base := string(read(t, "server.yaml"))
		ledPolicy := func(enabled bool, subjects string) {
			fragment := fmt.Sprintf("    notification_worker:\n      status_leds:\n        enabled: %t\n        socket: /tmp/wux/led.sock\n        daemon: {mode: existing}\n        bindings:\n          backup: {producer: backup-worker, subjects: [%q], led: green}\n", enabled, subjects)
			write("server.yaml", strings.Replace(base, "    notification_worker:\n", fragment, 1))
		}
		ledPolicy(true, "*")
		run(t, "backup-worker", "apply", true)
		setup := "manager/.local/state/deploy/backup-worker/setup.json"
		var before backup.WorkerSetupJournal
		if err := json.Unmarshal(read(t, setup), &before); err != nil {
			t.Fatal(err)
		}
		identity := read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")
		enrollment := read(t, "manager/.local/state/deploy/worker-control/notification-worker/consumer-enrollment.json")
		for revision, next := range []struct {
			enabled  bool
			subjects string
		}{{true, "source/source"}, {false, "source/source"}, {true, "source/source"}, {true, "*"}} {
			ledPolicy(next.enabled, next.subjects)
			run(t, "notification-worker", "plan", true)
			run(t, "notification-worker", "apply", true)
			for _, worker := range []string{"backup-worker", "notification-worker"} {
				for _, action := range []string{"apply", "resume", "status", "plan"} {
					run(t, worker, action, true)
				}
			}
			var after backup.WorkerSetupJournal
			if err := json.Unmarshal(read(t, setup), &after); err != nil {
				t.Fatal(err)
			}
			if after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.Producer, before.Producer) {
				t.Fatal("LED policy replaced backup identity")
			}
			if !bytes.Equal(identity, read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")) || !bytes.Equal(enrollment, read(t, "manager/.local/state/deploy/worker-control/notification-worker/consumer-enrollment.json")) {
				t.Fatal("LED policy rewrote gateway enrollment")
			}
			if revision == 0 {
				base = strings.Replace(base, "schedule: \"daily\"", "schedule: \"hourly\"", 1)
				ledPolicy(next.enabled, next.subjects)
				run(t, "backup-worker", "apply", true)
				run(t, "notification-worker", "resume", true)
			}

		}
		cmd := exec.Command(root+"/systemctl", "--user", "stop", "deploy-notification-worker.service")
		if out, err := process.LimitedOutput(cmd, 45*time.Second, 4096); err != nil {
			t.Fatal(err, string(out))
		}
	})

}

// The qualification connection checks the key actually enrolled by the source
// helper. All forwarding, PTY and command requests are denied in this fixture.
func startBackupCLIRestrictionServer(t *testing.T, home string, addresses ...string) (int, ssh.PublicKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	address := "127.0.0.1"
	if len(addresses) > 0 {
		address = addresses[0]
	}
	l, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatal(err)
	}
	c := &ssh.ServerConfig{PublicKeyCallback: func(m ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		var identity source.SourceIdentity
		body, err := os.ReadFile(filepath.Join(home, ".local/state/deploy/backup-source/identity.json"))
		if err == nil {
			err = json.Unmarshal(body, &identity)
		}
		if err == nil && m.User() == filepath.Base(home) {
			for _, a := range identity.Workers {
				if a.Enabled && a.Fingerprint == ssh.FingerprintSHA256(key) {
					return nil, nil
				}
			}
		}
		return nil, fmt.Errorf("unregistered fixture key")
	}}
	c.AddHostKey(signer)
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(35 * time.Second))
				server, channels, requests, e := ssh.NewServerConn(conn, c)
				if e != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for ch := range channels {
					if ch.ChannelType() != "session" {
						ch.Reject(ssh.Prohibited, "fixture restriction")
						continue
					}
					stream, reqs, e := ch.Accept()
					if e != nil {
						continue
					}
					go func() {
						defer stream.Close()
						for req := range reqs {
							if req.Type == "exec" {
								req.Reply(true, nil)
								stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
								return
							}
							req.Reply(false, nil)
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() { l.Close(); <-done; wg.Wait() })
	return l.Addr().(*net.TCPAddr).Port, signer.PublicKey()
}

const backupCLISSH = `#!/usr/bin/python3
import base64,json,os,pathlib,socket,sys
root=pathlib.Path('/tmp/wux')
errors=os.open(root/'ssh-errors.log',os.O_WRONLY|os.O_CREAT|os.O_APPEND,0o600)
os.dup2(errors,2);os.close(errors)
args=sys.argv[1:]
account='app2' if any(a.startswith('app2@') for a in args) or ('-l' in args and args[args.index('-l')+1]=='app2') else 'app'
is_source=any(a.startswith(account+'@') for a in args) or ('-l' in args and args[args.index('-l')+1]==account)
home=root/(account if is_source else 'manager')
command=args[-1]
env=os.environ.copy()
env.update(HOME=str(home),USER=home.name,PATH='/usr/bin:/bin',XDG_CONFIG_HOME='',XDG_STATE_HOME='',XDG_DATA_HOME='',XDG_RUNTIME_DIR='')
if is_source:
 if command in ('127.0.0.1','127.0.0.3','127.0.0.4'):
  identity=json.loads((home/'.local/state/deploy/backup-source/identity.json').read_text())
  enabled=[(w,a) for w,a in identity['workers'].items() if a['enabled']]
  assert len(enabled)==1
  worker,auth=enabled[0]
  import shlex
  command=shlex.join([str(home/'.local/lib/deploy/backup-source/deploy'),'source-serve','--worker-id',worker,'--key-id',auth['fingerprint']])
 payload=sys.stdin.buffer.read(128*1024*1024+65536)
 with socket.socket(socket.AF_UNIX) as peer:
  peer.connect(str(root/'source.sock'))
  peer.sendall(json.dumps({'Account':account,'Command':command,'Input':base64.b64encode(payload).decode()}).encode())
  peer.shutdown(socket.SHUT_WR)
  with peer.makefile('rb') as stream: response=json.load(stream)
 sys.stdout.buffer.write(base64.b64decode(response['Output'] or ''))
 sys.stderr.buffer.write(base64.b64decode(response['Error'] or ''))
 sys.exit(response['Status'])
if command.startswith('python3 -I -c ') and 'pwd.getpwuid' in command and (root/'led-facts').exists():
 import subprocess
 observed=subprocess.run(['/bin/sh','-c',command],env=env,stdout=subprocess.PIPE,check=True)
 facts=json.loads(observed.stdout)
 facts.update(board='Raspberry Pi offline fixture',leds=['ACT'],led_paths={'ACT':'/sys/devices/fixture/ACT'})
 print(json.dumps(facts));sys.exit(0)
os.execve('/bin/sh',['/bin/sh','-c',command],env)
`

// Separate source namespaces are created by the parent test because some hosts
// prohibit nesting unprivileged namespaces. The Unix broker carries only this
// test's SSH adapter traffic; every source subprocess still has a read-only host
// filesystem, no external network, and the independently resolved source home.
func startBackupCLISourceBroker(t *testing.T, dir string) {
	t.Helper()
	// The frozen-checkpoint path can exceed sockaddr_un's limit. A retained
	// directory descriptor gives bind a short path to this same owned fixture;
	// namespace clients still use /tmp/wux/source.sock.
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", fmt.Sprintf("/proc/self/fd/%d/source.sock", directory.Fd()))
	if err != nil {
		directory.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Minute))
				var request struct {
					Account string
					Command string
					Input   []byte
				}
				var response struct {
					Output, Error []byte
					Status        int
				}
				if err := json.NewDecoder(io.LimitReader(conn, 192<<20)).Decode(&request); err != nil || request.Account != "app" && request.Account != "app2" {
					response.Status, response.Error = 1, []byte("invalid source fixture frame")
				} else {
					calls, err := os.OpenFile(filepath.Join(dir, "source-calls.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
					if err != nil {
						return
					}
					_, _ = calls.WriteString("source\n")
					calls.Close()
					args := []string{"--ro-bind", "/", "/", "--unshare-user", "--unshare-net", "--unshare-pid", "--die-with-parent", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--bind", dir, "/tmp/wux", "--ro-bind", filepath.Join(dir, request.Account+".passwd"), "/etc/passwd"}
					for _, tool := range []string{"systemctl", "loginctl", "podman", "busctl"} {
						args = append(args, "--ro-bind", filepath.Join(dir, tool), "/usr/bin/"+tool)
					}
					args = append(args, "--chdir", "/tmp/wux/"+request.Account, "--", "/bin/sh", "-c", request.Command)
					cmd := exec.Command("bwrap", args...)
					cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/tmp/wux/" + request.Account, "USER=" + request.Account, "LANG=C"}
					cmd.Stdin = bytes.NewReader(request.Input)
					var out, errs process.Buffer
					out.Limit, errs.Limit = 64<<10, 8192
					cmd.Stdout, cmd.Stderr = &out, &errs
					if err := process.Run(cmd, 90*time.Second, time.Second); err != nil {
						response.Status = 1
					}
					response.Output, response.Error = out.Bytes(), errs.Bytes()
					if response.Status == 0 && strings.Contains(request.Command, "__source_admin") && bytes.Contains(request.Input, []byte(`"operation":"revoke-retained"`)) {
						if err := os.Remove(filepath.Join(dir, "lose-retirement-reply")); err == nil {
							response.Status, response.Output, response.Error = 1, nil, []byte("simulated lost source retirement reply\n")
						}
					}

					if response.Status == 0 && request.Account == "app2" && strings.Contains(request.Command, "__source_admin") {
						if err := os.Remove(filepath.Join(dir, "lose-extra-source-reply")); err == nil {
							response.Status, response.Output, response.Error = 1, nil, []byte("simulated lost added-source enrollment reply\n")
						}
					}
					if response.Status == 0 && strings.Contains(request.Command, "__source_admin") {
						if err := os.Remove(filepath.Join(dir, "lose-source-reply")); err == nil {
							response.Status, response.Output, response.Error = 1, nil, []byte("simulated lost source enrollment reply\n")
						}
					}
				}
				_ = json.NewEncoder(conn).Encode(response)
			}()
		}
	}()
	t.Cleanup(func() { l.Close(); <-done; directory.Close() })
}

const backupCLIPodman = `#!/usr/bin/python3
import hashlib,json,pathlib,platform,subprocess,sys
if sys.argv[1]=='ps': sys.exit(0)
if sys.argv[1]=='run' and sys.argv[-1]=='__shared-reporting-probe':
 sys.exit(subprocess.call(['/tmp/wux/deploy','backup-worker',sys.argv[-1]]))
if sys.argv[1]=='pull': sys.exit(0)
assert sys.argv[1]=='run' and sys.argv[-1]=='__worker_selftest',sys.argv
artifact='/tmp/wux/upgrade-helper' if 'fixture.invalid/backup@sha256:'+'c'*64 in sys.argv else '/tmp/wux/deploy'
print(json.dumps({'schema':1,'architecture':{'x86_64':'amd64','aarch64':'arm64'}[platform.machine()],'isolated':True,'helper_sha256':hashlib.sha256(pathlib.Path(artifact).read_bytes()).hexdigest()}))
`

const backupCLIBus = `#!/usr/bin/python3
import json,pathlib,shlex,sys
if sys.argv[-1]=='ExecStartEx':
 state=json.loads(pathlib.Path('/tmp/wux/manager/manager.json').read_text())
 command=next(line[len('ExecStart='):] for line in state['LoadedUnit'].splitlines() if line.startswith('ExecStart='))
 argv=shlex.split(command)
 print(json.dumps({'type':'a(sasasttttuii)','data':[[argv[0],argv,[],0,0,0,0,0,0,0]]}));sys.exit(0)
value=['/tmp/wux/manager/.config/systemd/user'] if sys.argv[-1]=='UnitPath' else []
print(json.dumps({'type':'as','data':value}))
`

const backupCLIManager = `if action=='show-environment': sys.exit(0)
if action=='show' and 'deploy-notification-worker.service' in args and '--all' in args:
 loaded={}
 for line in state['LoadedUnit'].splitlines():
  if '=' in line and not line.startswith('#'):
   k,v=line.split('=',1);loaded[k]=v
 props=dict(state)
 props.update(MainPID=str(state['PID']),SubState='running' if state['PID'] else 'dead',ControlPID='0',NeedDaemonReload='no' if unit.read_text()==state['LoadedUnit'] else 'yes',TimeoutStopUSec=loaded.get('TimeoutStopSec','35').removesuffix('s')+'s',RemainAfterExit='no',Delegate='no')
 for key in ('Type','ProtectSystem','ProtectHome','NoNewPrivileges','PrivateUsers','PrivateDevices','ReadWritePaths','RestrictAddressFamilies','KillMode','SendSIGKILL','CapabilityBoundingSet','AmbientCapabilities'):
  props[key]=loaded.get(key,{'SendSIGKILL':'yes','KillMode':'control-group'}.get(key,''))
  if props[key] in ('true','false'): props[key]={'true':'yes','false':'no'}[props[key]]
  if key=='ReadWritePaths': props[key]=' '.join(shlex.split(props[key]))
 for key in next(a for a in args if a.startswith('--property=')).split('=',1)[1].split(','): print(key+'='+str(props.get(key,'')))
 sys.exit(0)
if any(a.startswith('deploy-backup') for a in args):
 names=[a for a in args if a.startswith('deploy-backup')]
 name=names[0]
 history=root/'backup-services.json'
 services=json.loads(history.read_text()) if history.exists() else {}
 ordinary=root/'.config/systemd/user'/name
 quadlet=root/'.config/containers/systemd'/(name.removesuffix('.service')+'.container')
 if action=='show':
  if '--value' in args:
   print('loaded' if ordinary.exists() or quadlet.exists() else 'not-found');sys.exit(0)
  generated=quadlet.exists()
  present=generated or ordinary.exists()
  props={'LoadState':'loaded' if present else 'not-found','ActiveState':services.get(name,{}).get('active','inactive'),'UnitFileState':'generated' if generated else services.get(name,{}).get('enabled','disabled'),'FragmentPath':('/run/user/%d/systemd/generator/'%os.getuid()+name) if generated else str(ordinary) if present else '', 'SourcePath':str(quadlet) if generated else '', 'DropInPaths':'','Names':name,'MainPID':'0','ControlPID':'0','NeedDaemonReload':'no','KillMode':'control-group','SendSIGKILL':'yes'}
  requested=next((a.split('=',1)[1].split(',') for a in args if a.startswith('--property=')),list(props))
  for k in requested: print(k+'='+props.get(k,''))
 elif action=='is-enabled': sys.exit(0 if services.get(name,{}).get('enabled')=='enabled' else 1)
 elif action in ('enable','disable','stop','start'):
  for n in names:
   current=services.setdefault(n,{'active':'inactive','enabled':'disabled'})
   if action in ('enable','disable'): current['enabled']='enabled' if action=='enable' else 'disabled'
   if action in ('start','stop') or '--now' in args: current['active']='active' if action in ('start','enable') else 'inactive'
  history.write_text(json.dumps(services))
 else: raise RuntimeError('unexpected backup manager action: '+repr(args))
 with open(root/'backup-manager.log','a') as log: log.write(json.dumps(args)+'\n')
 sys.exit(0)
`

func TestBackupCLISourceBrokerLongPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("checkpoint-", 12))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	startBackupCLISourceBroker(t, dir)
	info, err := os.Stat(filepath.Join(dir, "source.sock"))
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("broker socket not published in retained directory", err)
	}
}
