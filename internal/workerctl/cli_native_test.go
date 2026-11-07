package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// The CLI and both SSH Python programs are real. Only SSH's destination and the
// user manager are adapters; the installed worker is a real child with its actual
// Unix API, database and file leases. No host unit or provider is touched.
func TestWorkerCLIThroughSSHNativeGateway(t *testing.T) {
	testWorkerCLIThroughSSHNativeGateway(t, false)
}
func TestWorkerCLIAdoptsNativeGateway(t *testing.T) { testWorkerCLIThroughSSHNativeGateway(t, true) }
func testWorkerCLIThroughSSHNativeGateway(t *testing.T, adopting bool) {
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Skip("rootless Linux native helper")
	}
	dir, err := os.MkdirTemp("", "worker-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(dir, "deploy")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "../../cmd/deploy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, e := process.LimitedOutput(build, time.Minute, 8192); e != nil {
		t.Fatal(e, string(out))
	}
	if err = os.Chmod(binary, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	catalog := Catalog{Version: 1, Release: "cli-fixture", Linux: map[string]ArchitectureArtifacts{runtime.GOARCH: {Helper: Artifact{File: "deploy", SHA256: inputDigest(raw)}}}}
	raw, err = json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	write("catalog.json", string(raw), 0600)
	write("token", "123:synthetic-e2e-token", 0600)
	write("chat", "456", 0600)
	write("ssh", "#!/bin/sh\nfor argument do selected=$argument; done\nexec /bin/sh -c \"$selected\"\n", 0700)
	write("loginctl", "#!/bin/sh\nprintf 'yes\\n'\n", 0700)
	write("systemctl", nativeCLIManager, 0700)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_WORKER_E2E_ROOT", dir)
	for _, v := range []struct{ k, sub string }{{"XDG_CONFIG_HOME", "config"}, {"XDG_STATE_HOME", "state"}, {"XDG_DATA_HOME", "data"}, {"XDG_RUNTIME_DIR", "run"}} {
		p := filepath.Join(dir, v.sub)
		if err = os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(v.k, p)
	}
	t.Cleanup(func() {
		cmd := exec.Command(filepath.Join(dir, "systemctl"), "--user", "stop", "deploy-notification-worker.service")
		if out, e := process.LimitedOutput(cmd, 45*time.Second, 4096); e != nil {
			t.Errorf("fixture cleanup: %v %s", e, out)
		}
	})
	extraRoute := false
	importedPolicy := false
	policy := func(enabled bool, limit int) {
		if adopting && importedPolicy {
			e, err := OpenInventoryEdit(filepath.Join(dir, "server.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range []struct {
				path  []string
				value any
			}{{[]string{"notification_worker", "enabled"}, enabled}, {[]string{"notification_worker", "limits", "max_messages"}, limit}} {
				if err = e.Set("manager", v.path, v.value); err != nil {
					t.Fatal(err)
				}
			}
			if extraRoute {
				if err = e.Set("manager", []string{"notification_worker", "routes", "audit"}, config.NotificationRoute{Primary: "telegram"}); err != nil {
					t.Fatal(err)
				}
			}
			if err = e.Save(); err != nil {
				t.Fatal(err)
			}
			return
		}
		routes := ""
		if extraRoute {
			routes = "      routes: {alerts: {primary: telegram}, audit: {primary: telegram}}\n"
		}
		body := fmt.Sprintf(`version: 3
servers:
  manager:
    host: offline-fixture.invalid
    user: %s
    notification_worker:
      enabled: %t
      providers:
        telegram:
          token_input: %s/token
          chat_id_input: %s/chat
%s      limits: {max_messages: %d}
      artifacts: {catalog: %s/catalog.json}
`, u.Username, enabled, dir, dir, routes, limit, dir)
		if adopting {
			body = strings.ReplaceAll(strings.ReplaceAll(body, "token_input:", "token_file:"), "chat_id_input:", "chat_id_file:")
		}
		write("server.yaml", body, 0600)
	}
	type reply struct {
		Version                      int
		State, Code, Next, Candidate string
	}
	run := func(action string, success bool) reply {
		t.Helper()
		args := []string{"--servers", filepath.Join(dir, "server.yaml"), "--non-interactive", "notification-worker"}
		if action != "apply" {
			args = append(args, action)
		}
		args = append(args, "manager", "--json")
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		var stdout, stderr process.Buffer
		stdout.Limit = 1 << 20
		stderr.Limit = 8192
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		e := process.RunContext(ctx, cmd, 45*time.Second, time.Second)
		if (e == nil) != success {
			runtimeLog, _ := os.ReadFile(filepath.Join(dir, "runtime.log"))
			managerErrors, _ := os.ReadFile(filepath.Join(dir, "manager-errors.log"))
			t.Fatalf("%s success=%t: %v\n%s\n%s\nRuntime: %s\nManager: %s", action, success, e, stdout.Bytes(), stderr.Bytes(), runtimeLog, managerErrors)
		}
		var result reply
		if e = json.Unmarshal(stdout.Bytes(), &result); e != nil {
			t.Fatalf("%s response: %s %s: %v", action, stdout.Bytes(), stderr.Bytes(), e)
		}
		if result.Version != 1 {
			t.Fatal("invalid response", result)
		}
		if strings.Contains(string(stdout.Bytes()), "synthetic-e2e-token") || strings.Contains(string(stderr.Bytes()), "synthetic-e2e-token") {
			t.Fatal("credential leaked")
		}
		return result
	}
	manager := func() (int, int) {
		t.Helper()
		raw, e := os.ReadFile(filepath.Join(dir, "manager.json"))
		if e != nil {
			t.Fatal(e)
		}
		var v struct{ PID, Starts int }
		if e = json.Unmarshal(raw, &v); e != nil {
			t.Fatal(e)
		}
		return v.PID, v.Starts
	}
	policy(true, 100)
	identityPath := filepath.Join(dir, "state/deploy/notification-worker/state/identity.json")
	if adopting {
		inv, e := config.ReadInventory(filepath.Join(dir, "server.yaml"))
		if e != nil {
			t.Fatal(e)
		}
		facts, e := (SSHInspector{}).Inspect(context.Background(), inv.Servers["manager"])
		if e != nil {
			t.Fatal(e)
		}
		paths, e := ResolvePaths("notification-worker", inv.Servers["manager"].Desired.Notification.Paths, facts)
		if e != nil {
			t.Fatal(e)
		}
		input, e := CompileUnboundNotification(*inv.Servers["manager"].Desired, paths, facts.Home, map[string][]byte{"token": []byte("123:synthetic-e2e-token"), "chat_id": []byte("456")})
		if e != nil {
			t.Fatal(e)
		}
		for name, route := range input.Worker.Routes {
			route.Revision = "manual-e2e-revision"
			input.Worker.Routes[name] = route
		}
		body, e := json.MarshalIndent(input, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		manualRuntime := filepath.Join(dir, "manual-runtime.json")
		if e = publishPrivateInput(manualRuntime, body); e != nil {
			t.Fatal(e)
		}
		for _, path := range []string{filepath.Dir(paths.State), filepath.Dir(paths.Installation), paths.UnitDirectory} {
			d, e := ensureConfigDirectory(path)
			if e != nil {
				t.Fatal(e)
			}
			d.Close()
		}
		c, e := input.Resolve()
		if e != nil {
			t.Fatal(e)
		}
		op, e := safefs.NewID("n")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = worker.BootstrapGateway(context.Background(), paths.State, op, c.Worker); e != nil {
			t.Fatal(e)
		}
		if _, e = install.EnsureVerification(context.Background(), install.ManagedSpec{Version: 1, Root: paths.Installation, State: paths.State, UnitDirectory: paths.UnitDirectory, RuntimeFile: manualRuntime, BinaryFile: binary, BinarySHA256: catalog.Linux[runtime.GOARCH].Helper.SHA256, DeniedPaths: paths.DeniedPaths}); e != nil {
			t.Fatal(e)
		}
		beforePID, beforeStarts := manager()
		candidate := run("import", true)
		if candidate.Code != "review_required" {
			t.Fatal(candidate)
		}
		write("server.yaml", candidate.Candidate, 0600)
		importedPolicy = true
		if got := run("adopt", true); got.State != "waiting_for_producers" {
			t.Fatal("adoption", got)
		}
		for _, action := range []string{"adopt", "apply", "resume", "status", "plan"} {
			if got := run(action, true); got.Code != "ok" {
				t.Fatal(action, got)
			}
		}
		afterPID, afterStarts := manager()
		if afterPID != beforePID || afterStarts != beforeStarts {
			t.Fatal("adoption restarted existing gateway")
		}
		current, e := os.ReadFile(manualRuntime)
		if e != nil || string(current) != string(body) {
			t.Fatal("adoption changed installed input", e)
		}
	} else {
		write("lose-start-reply", "once", 0600)
		if got := run("apply", false); got.Code == "ok" {
			t.Fatal("lost manager reply was hidden")
		}
	}
	reservedIdentity, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := run("resume", true); got.State != "waiting_for_producers" || got.Code != "ok" {
		t.Fatal(got)
	}
	pid, starts := manager()
	if pid == 0 || starts < 1 {
		t.Fatal("native worker missing", pid, starts)
	}
	recoveredIdentity, e := os.ReadFile(identityPath)
	if e != nil || string(recoveredIdentity) != string(reservedIdentity) {
		t.Fatal("resume replaced identity", e)
	}
	for _, action := range []string{"apply", "resume", "status", "plan"} {
		got := run(action, true)
		if got.Code != "ok" {
			t.Fatal(action, got)
		}
		p, n := manager()
		if p != pid || n != starts {
			t.Fatal("unchanged action restarted native service", action)
		}
	}
	policy(true, 101)
	if got := run("apply", true); got.Code != "ok" {
		t.Fatal("limits", got)
	}
	newPID, newStarts := manager()
	if newPID == pid || newStarts != starts+1 {
		t.Fatal("limits did not replace runtime")
	}
	run("apply", true)
	p, n := manager()
	if p != newPID || n != newStarts {
		t.Fatal("limits retry restarted service")
	}
	extraRoute = true
	policy(true, 101)
	if got := run("apply", true); got.State != "waiting_for_producers" {
		t.Fatal("routes", got)
	}
	routePID, routeStarts := manager()
	if routePID == newPID || routeStarts != newStarts+1 {
		t.Fatal("route change did not replace runtime")
	}
	for _, action := range []string{"apply", "resume", "status", "plan"} {
		if got := run(action, true); got.Code != "ok" {
			t.Fatal(action, got)
		}
		p, n := manager()
		if p != routePID || n != routeStarts {
			t.Fatal("completed routes restarted service", action)
		}
	}
	currentIdentity, e := os.ReadFile(identityPath)
	if e != nil || string(currentIdentity) != string(reservedIdentity) {
		t.Fatal("routes changed identity", e)
	}
	newPID, newStarts = routePID, routeStarts
	policy(false, 101)
	if got := run("apply", true); got.State != "disabled" {
		t.Fatal("disable", got)
	}
	p, n = manager()
	if p != 0 || n != newStarts {
		t.Fatal("disable left worker running")
	}
	policy(true, 101)
	if got := run("apply", true); got.State != "waiting_for_producers" {
		t.Fatal("restore", got)
	}
	pid, starts = manager()
	if pid == 0 || starts != newStarts+1 {
		t.Fatal("restore did not start retained worker")
	}
	run("apply", true)
	p, n = manager()
	if p != pid || n != starts {
		t.Fatal("restore retry restarted service")
	}
	// New YAML references rotate credentials and then the audience. The native
	// gateway restarts once for each reviewed revision; no provider send occurs.
	for round := 0; round < 2; round++ {
		write("token-next", "123:rotated-e2e-token", 0600)
		chatFile := "chat"
		if round == 1 {
			chatFile = "chat-next"
			write(chatFile, "789", 0600)
		}
		e, err := OpenInventoryEdit(filepath.Join(dir, "server.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Set("manager", []string{"notification_worker", "providers", "telegram"}, config.ProviderDeclaration{TokenFile: filepath.Join(dir, "token-next"), ChatIDFile: filepath.Join(dir, chatFile)}); err != nil {
			t.Fatal(err)
		}
		if err = e.Save(); err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			write("lose-start-reply", "once", 0600)
			run("apply", false)
			run("resume", true)
		} else {
			run("apply", true)
		}
		providerPID, providerStarts := manager()
		if providerPID == 0 || providerPID == pid || providerStarts != starts+1 {
			t.Fatal("provider migration service lifetime", providerStarts, starts)
		}
		for _, action := range []string{"apply", "resume", "status", "plan"} {
			if got := run(action, true); got.Code != "ok" {
				t.Fatal(action, got)
			}
			p, n := manager()
			if p != providerPID || n != providerStarts {
				t.Fatal("provider replay restarted service")
			}
		}
		current, err := os.ReadFile(identityPath)
		if err != nil || string(current) != string(reservedIdentity) {
			t.Fatal("provider migration changed identity", err)
		}
		pid, starts = providerPID, providerStarts
	}
	// Same bot identity with changed credential bytes is a rotation, never a no-op.
	write("token-next", "123:changed-synthetic-token", 0600)
	got := run("apply", false)
	if got.Code == "ok" {
		t.Fatal("credential drift accepted")
	}
	p, n = manager()
	if p != pid || n != starts {
		t.Fatal("credential refusal changed service")
	}
	write("token-next", "123:rotated-e2e-token", 0600)
	journal := filepath.Join(dir, "state/deploy/worker-control/notification-worker/operation.json")
	if err = os.Rename(journal, journal+".retained-test"); err != nil {
		t.Fatal(err)
	}
	if got = run("apply", false); got.Code == "ok" {
		t.Fatal("missing journal adopted installation")
	}
	if got = run("adopt", false); got.Code == "ok" {
		t.Fatal("adopt recreated missing authority")
	}
	sourceYAML, e := os.ReadFile(filepath.Join(dir, "server.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	imported := run("import", true)
	if imported.State != "candidate" || imported.Code != "review_required" || !strings.Contains(imported.Candidate, "token_file:") || strings.Contains(imported.Candidate, "synthetic") {
		t.Fatal("import did not return private-reference candidate", imported.State, imported.Code)
	}
	afterYAML, e := os.ReadFile(filepath.Join(dir, "server.yaml"))
	if e != nil || string(afterYAML) != string(sourceYAML) {
		t.Fatal("import changed source YAML", e)
	}
	if _, err = os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("recreated lost coordinator authority", err)
	}
	p, n = manager()
	if p != pid || n != starts {
		t.Fatal("missing-journal refusal changed service")
	}
}

const nativeCLIManager = `#!/usr/bin/python3
import json,os,pathlib,shlex,signal,subprocess,sys,time
root=pathlib.Path(os.environ['DEPLOY_WORKER_E2E_ROOT'])
def manager_error(kind,error,trace):
 import traceback
 with open(root/'manager-errors.log','a') as log: traceback.print_exception(kind,error,trace,file=log)
 sys.__excepthook__(kind,error,trace)
sys.excepthook=manager_error
path=root/'manager.json'
def save_state():
 temporary=root/'manager-next.json'
 temporary.write_text(json.dumps(state));temporary.replace(path)
state=json.loads(path.read_text()) if path.exists() else {'PID':0,'Starts':0,'LoadState':'not-found','ActiveState':'inactive','FragmentPath':'','DropInPaths':'','UnitFileState':'','Names':''}
args=[a for a in sys.argv[1:] if a not in ('--user','--no-pager')]
unit=root/'config/systemd/user/deploy-notification-worker.service'
action=args[0]
if action=='show-environment': sys.exit(0)
if action=='show':
 for key in ('LoadState','ActiveState','FragmentPath','DropInPaths','UnitFileState','Names'): print(key+'='+state[key])
 sys.exit(0)
if action=='daemon-reload':
 if unit.exists():
  state.update(LoadState='loaded',FragmentPath=str(unit),Names=unit.name)
  if not state['UnitFileState']: state['UnitFileState']='disabled'
 else: state.update(LoadState='not-found',FragmentPath='',Names='',UnitFileState='')
elif action=='enable':
 assert args[1:]==['--now',unit.name]
 if state['PID']:
  state['UnitFileState']='enabled'
  save_state();sys.exit(0)
 command=next(line[len('ExecStart='):] for line in unit.read_text().splitlines() if line.startswith('ExecStart='))
 argv=shlex.split(command)
 assert argv[0].startswith(str(root)+'/')
 assert argv[1:3]==['notification-worker','__managed-serve']
 with open(root/'runtime.log','ab',buffering=0) as log:
  p=subprocess.Popen(argv,stdin=subprocess.DEVNULL,stdout=log,stderr=log,start_new_session=True)
 state.update(PID=p.pid,Starts=state['Starts']+1,ActiveState='active',UnitFileState='enabled')
elif action=='stop':
 assert args[1:]==[unit.name]
 if state['PID']:
  pid=state['PID']
  try:
   assert os.readlink('/proc/%d/exe'%pid).startswith(str(root)+'/')
   os.kill(pid,signal.SIGTERM)
   # Honor the generated unit's shutdown budget instead of racing a live
   # worker against an unrelated five-second fixture timeout.
   budget=next((line.split('=',1)[1] for line in unit.read_text().splitlines() if line.startswith('TimeoutStopSec=')),'35')
   deadline=time.monotonic()+float(budget.removesuffix('s'))
   while time.monotonic()<deadline:
    try:
     # A dead adopted child may remain a zombie until the container reaps it.
     stat=pathlib.Path('/proc/%d/stat'%pid).read_text().split(') ',1)[1]
     if stat.startswith('Z '): break
    except (FileNotFoundError, ProcessLookupError): break
    time.sleep(.01)
   else: raise RuntimeError('fixture worker did not stop')
  except (FileNotFoundError, ProcessLookupError): pass
 state.update(PID=0,ActiveState='inactive')
elif action=='disable':
 assert args[1:]==[unit.name]
 state['UnitFileState']='disabled'
else: raise RuntimeError('unexpected manager action: '+repr(args))
save_state()
if action=='enable' and (root/'lose-start-reply').exists():
 (root/'lose-start-reply').unlink();sys.exit(73)
`
