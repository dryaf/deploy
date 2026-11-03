package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func nativeSignalSpecFixture(t *testing.T) (NativeSignalServiceSpec, relay.Config) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	s := NativeSignalServiceSpec{UnitFile: filepath.Join(root, "config/systemd/user", nativeSignalUnit), Executable: filepath.Join(root, "deploy"), RelayConfig: filepath.Join(root, "relay.json")}
	for _, dir := range []string{filepath.Dir(s.UnitFile), filepath.Join(root, "secrets")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := relay.Config{StateDir: filepath.Join(root, "account-state"), Socket: filepath.Join(root, "socket/relay.sock"), AccountFile: filepath.Join(root, "secrets/account"), RecipientFile: filepath.Join(root, "secrets/recipient"), Interactive: &relay.InteractiveConfig{Version: "0.14.9", Account: "11111111-1111-4111-8111-111111111111", InboxDir: filepath.Join(root, "inbox")}}
	write := func(p string, b []byte, mode os.FileMode) string {
		t.Helper()
		if err := os.WriteFile(p, b, mode); err != nil {
			t.Fatal(err)
		}
		return managedDigest(b)
	}
	c.Interactive.Destinations = []interaction.Destination{{ID: "private", Generation: "one", Provider: "signal", Account: c.Interactive.Account, NativeID: "22222222-2222-4222-8222-222222222222"}}
	write(c.AccountFile, []byte("private-account-fixture"), 0600)
	write(c.RecipientFile, []byte("private-recipient-fixture"), 0600)
	b, _ := json.Marshal(c)
	s.RelaySHA256 = write(s.RelayConfig, b, 0600)
	s.UnitSHA256 = write(s.UnitFile, []byte("[Service]\nType=exec\nExecStart="+systemdArgument(s.Executable)+" notification-worker __signal-relay --local --config "+systemdArgument(s.RelayConfig)+"\nKillMode=control-group\nTimeoutStopSec=30s\n"), 0600)
	path, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.ExecutableSHA256 = write(s.Executable, b, 0700)
	if _, _, err := s.files(); err != nil {
		t.Fatal("invalid baseline fixture", err)
	}
	return s, c
}

func nativeSignalManagerProperties(s NativeSignalServiceSpec, active bool) map[string]string {
	m := map[string]string{}
	for _, key := range nativeSignalEmptyProperties {
		m[key] = ""
	}
	for k, v := range nativeSignalFixedProperties {
		m[k] = v
	}
	m["FragmentPath"], m["UnitFileState"], m["Type"], m["TimeoutStopUSec"] = s.UnitFile, "enabled", "exec", "30s"
	m["ActiveState"], m["SubState"], m["MainPID"] = "inactive", "dead", "0"
	if active {
		m["ActiveState"], m["SubState"], m["MainPID"] = "active", "running", "123"
	}
	return m
}

func nativeSignalManagerOutput(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, m[key])
	}
	return []byte(b.String())
}

func nativeSignalExecOutput(s NativeSignalServiceSpec) []byte {
	b, _ := json.Marshal(map[string]any{"type": "a(sasasttttuii)", "data": [][]any{{s.Executable, s.argv(), []string{}, 0, 0, 0, 0, 0, 0, 0}}})
	return b
}

func nativeSignalManagerFixture(t *testing.T, s NativeSignalServiceSpec, m map[string]string) *nativeSignalRecoveryService {
	t.Helper()
	v := newNativeSignalRecoveryService(s)
	v.processCheck = func(pid uint32, got NativeSignalServiceSpec) error {
		if pid != 123 || got != s {
			t.Fatal("wrong process inspection", pid, got)
		}
		return nil
	}
	v.run = func(_ context.Context, cmd string, args ...string) ([]byte, error) {
		switch cmd {
		case "systemctl":
			if len(args) != 6 || !reflect.DeepEqual(args[:5], []string{"--user", "--no-pager", "show", nativeSignalUnit, "--all"}) || !strings.HasPrefix(args[5], "--property=") {
				t.Fatal("unexpected manager call", args)
			}
			return nativeSignalManagerOutput(m), nil
		case "busctl":
			if !reflect.DeepEqual(args, []string{"--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2eservice", "org.freedesktop.systemd1.Service", "ExecStartEx"}) {
				t.Fatal("unexpected bus call", args)
			}
			return nativeSignalExecOutput(s), nil
		}
		t.Fatal("unexpected executable", cmd)
		return nil, errors.New("unexpected command")
	}
	return v
}

func TestNativeSignalServiceBindingAndStop(t *testing.T) {
	s, c := nativeSignalSpecFixture(t)
	m := nativeSignalManagerProperties(s, true)
	svc := nativeSignalManagerFixture(t, s, m)
	ctx := context.Background()
	before, err := svc.Inspect(ctx)
	if err != nil || !before.Active {
		t.Fatal(before, err)
	}
	svc.binding = before.Binding
	run := svc.run
	stops := 0
	svc.run = func(ctx context.Context, cmd string, args ...string) ([]byte, error) {
		if cmd == "systemctl" && reflect.DeepEqual(args, []string{"--user", "--no-pager", "stop", nativeSignalUnit}) {
			stops++
			m["ActiveState"], m["SubState"], m["MainPID"] = "inactive", "dead", "0"
			return nil, nil
		}
		return run(ctx, cmd, args...)
	}
	if err = svc.Stop(ctx); err != nil || stops != 1 {
		t.Fatal(stops, err)
	}
	after, err := svc.Inspect(ctx)
	if err != nil || after.Active || after.Binding != before.Binding {
		t.Fatal(after, err)
	}
	if err = svc.Stop(ctx); err != nil || stops != 1 {
		t.Fatal("repeated inactive stop", stops, err)
	}
	if err = svc.Start(ctx); err == nil {
		t.Fatal("receiver start allowed")
	}
	if err = os.WriteFile(c.AccountFile, []byte("rotated-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Inspect(ctx); err == nil {
		t.Fatal("credential drift accepted")
	}
	if err = svc.Stop(ctx); err == nil || stops != 1 {
		t.Fatal("stop with drift", stops, err)
	}
}

func TestNativeSignalServiceRejectsManagerDrift(t *testing.T) {
	s, _ := nativeSignalSpecFixture(t)
	for _, tc := range []struct{ key, value string }{
		{"FragmentPath", "/other/unit"}, {"Names", nativeSignalUnit + " alias.service"}, {"SourcePath", "/quadlet/container"},
		{"DropInPaths", "/override.conf"}, {"NeedDaemonReload", "yes"}, {"Job", "7"}, {"ControlPID", "7"},
		{"ExecStartPre", "unexpected"}, {"ExecStop", "unexpected"}, {"RootDirectory", "/chroot"},
		{"BindPaths", "/source:/account"}, {"EnvironmentFiles", "/credentials"}, {"PrivateTmp", "yes"},
		{"User", "another-user"}, {"Type", "forking"}, {"UnitFileState", "generated"}, {"KillMode", "process"},
		{"SendSIGKILL", "no"}, {"TimeoutStopUSec", "infinity"}, {"RuntimeDirectory", "deletable"},
		{"MainPID", "0"}, {"ActiveState", "activating"}, {"SubState", "auto-restart"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			m := nativeSignalManagerProperties(s, true)
			m[tc.key] = tc.value
			svc := nativeSignalManagerFixture(t, s, m)
			if _, err := svc.Inspect(context.Background()); err == nil {
				t.Fatal("unsafe manager state accepted")
			}
		})
	}
	for _, name := range []string{"missing", "duplicate", "malformed", "extra"} {
		t.Run(name, func(t *testing.T) {
			m := nativeSignalManagerProperties(s, false)
			if name == "missing" {
				delete(m, "SendSIGKILL")
			}
			b := nativeSignalManagerOutput(m)
			switch name {
			case "duplicate":
				b = append(b, []byte("MainPID=0\n")...)
			case "malformed":
				b = append(b, []byte("oops\n")...)
			case "extra":
				b = append(b, []byte("Unexpected=yes\n")...)
			}
			svc := newNativeSignalRecoveryService(s)
			svc.run = func(context.Context, string, ...string) ([]byte, error) { return b, nil }
			if _, err := svc.Inspect(context.Background()); err == nil {
				t.Fatal("invalid property set accepted")
			}
		})
	}
}

func TestNativeSignalExecContract(t *testing.T) {
	s, _ := nativeSignalSpecFixture(t)
	if err := parseNativeSignalExec(nativeSignalExecOutput(s), s); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old-type", "wrapper", "arg-boundaries", "extra-arg", "flags", "null-flags", "second-command", "missing-field", "bad-number", "null-number", "extra-json"} {
		t.Run(name, func(t *testing.T) {
			row := []any{s.Executable, s.argv(), []string{}, 0, 0, 0, 0, 0, 0, 0}
			v := map[string]any{"type": "a(sasasttttuii)", "data": [][]any{row}}
			switch name {
			case "old-type":
				v["type"] = "a(sasbttttuii)"
			case "wrapper":
				row[0] = "/bin/sh"
			case "arg-boundaries":
				row[1] = []string{strings.Join(s.argv(), " ")}
			case "extra-arg":
				row[1] = append(s.argv(), "--other")
			case "flags":
				row[2] = []string{"ignore-failure"}
			case "null-flags":
				row[2] = nil
			case "second-command":
				v["data"] = [][]any{row, row}
			case "missing-field":
				v["data"] = [][]any{row[:9]}
			case "bad-number":
				row[7] = -1
			case "null-number":
				row[3] = nil
			}
			b, _ := json.Marshal(v)
			if name == "extra-json" {
				b = append(b, []byte("{}")...)
			}
			if err := parseNativeSignalExec(b, s); err == nil {
				t.Fatal("invalid command accepted")
			}
		})
	}
}

func TestNativeSignalServiceMidInspectionDrift(t *testing.T) {
	s, c := nativeSignalSpecFixture(t)
	m := nativeSignalManagerProperties(s, true)
	svc := nativeSignalManagerFixture(t, s, m)
	svc.processCheck = func(uint32, NativeSignalServiceSpec) error { m["MainPID"] = "124"; return nil }
	if _, err := svc.Inspect(context.Background()); err == nil {
		t.Fatal("process change accepted")
	}
	m["MainPID"] = "123"
	svc.processCheck = func(uint32, NativeSignalServiceSpec) error {
		return os.WriteFile(c.RecipientFile, []byte("changed"), 0600)
	}
	if _, err := svc.Inspect(context.Background()); err == nil {
		t.Fatal("mid-inspection credential change accepted")
	}
	svc.processCheck = func(uint32, NativeSignalServiceSpec) error { return errors.New("old executable") }
	if _, err := svc.Inspect(context.Background()); err == nil {
		t.Fatal("old running executable accepted")
	}
}

func TestNativeSignalFileAndCapabilityGuards(t *testing.T) {
	for _, name := range []string{"binary-pin", "unit-pin", "config-pin", "public-secret", "symlink-secret", "hardlink-secret", "writable-unit", "foreign-unit", "old-probe"} {
		t.Run(name, func(t *testing.T) {
			s, c := nativeSignalSpecFixture(t)
			switch name {
			case "binary-pin":
				s.ExecutableSHA256 = strings.Repeat("0", 64)
			case "unit-pin":
				s.UnitSHA256 = strings.Repeat("0", 64)
			case "config-pin":
				s.RelaySHA256 = strings.Repeat("0", 64)
			case "public-secret":
				if err := os.Chmod(c.AccountFile, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink-secret":
				if err := os.Remove(c.AccountFile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(c.RecipientFile, c.AccountFile); err != nil {
					t.Fatal(err)
				}
			case "hardlink-secret":
				if err := os.Link(c.AccountFile, c.AccountFile+".extra"); err != nil {
					t.Fatal(err)
				}
			case "writable-unit":
				if err := os.Chmod(s.UnitFile, 0666); err != nil {
					t.Fatal(err)
				}
			case "foreign-unit":
				s.UnitFile = filepath.Join(t.TempDir(), nativeSignalUnit)
			case "old-probe":
				svc := nativeSignalManagerFixture(t, s, nativeSignalManagerProperties(s, false))
				if err := svc.VerifyRecoverySupport(context.Background()); err == nil {
					t.Fatal("binary without capability accepted")
				}
				return
			}
			if _, _, err := s.files(); err == nil {
				t.Fatal("unsafe file accepted")
			}
		})
	}
}

func TestNativeSignalRunningAccountLease(t *testing.T) {
	account := t.TempDir()
	if err := os.Chmod(account, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := signalaccount.AcquireRuntime(account)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = checkNativeSignalAccount("/proc/self", account); err != nil {
		t.Fatal(err)
	}
	// An open descriptor without the flock is insufficient.
	other := t.TempDir()
	p := filepath.Join(other, "deploy-account.lock")
	if err = os.WriteFile(p, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = checkNativeSignalAccount("/proc/self", other); err == nil {
		t.Fatal("unlocked descriptor accepted")
	}
}

func TestNativeSignalUnsupportedCannotFenceOrStop(t *testing.T) {
	s, rc := nativeSignalSpecFixture(t)
	if err := os.Mkdir(rc.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := signalaccount.AcquireRuntime(rc.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	svc := nativeSignalManagerFixture(t, s, nativeSignalManagerProperties(s, true))
	state, err := svc.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	svc.binding = state.Binding
	id, _ := safefs.NewID("n")
	_, err = signalaccount.FenceRecovery(context.Background(), rc.StateDir, signalaccount.RecoveryRequest{Operation: id, Binding: state.Binding, Target: protocol.Hash("fixture")}, svc,
		func(context.Context, func() error) (string, error) { t.Fatal("unsupported apply"); return "", nil },
		func(context.Context, string, func() error) error { t.Fatal("unsupported verify"); return nil })
	if err == nil {
		t.Fatal("binary without probe fenced account")
	}
	status, err := signalaccount.InspectRecovery(rc.StateDir)
	if err != nil || status.Phase != "none" || !status.RuntimeAllowed {
		t.Fatal(status, err)
	}
	l, err = signalaccount.AcquireRuntime(rc.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

func TestNativeSignalHandoffDryRun(t *testing.T) {
	s, rc := nativeSignalSpecFixture(t)
	c, path := signalRuntimeCLIConfiguration(t)
	c.Signal.Socket = rc.Socket
	writeRuntime(t, path, c)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := safefs.NewID("n")
	base := t.TempDir()
	spec := NativeSignalRecoverySpec{Version: 1, Operation: op, GatewayConfig: path, GatewaySHA256: managedDigest(b), GatewayState: filepath.Join(base, "gateway"), CommitHash: strings.Repeat("a", 64), AccountState: rc.StateDir, RelaySource: rc.Interactive.InboxDir, RelayDestination: filepath.Join(base, "destination"), Service: s}
	b, _ = json.Marshal(spec)
	input := filepath.Join(base, "spec.json")
	if err = os.WriteFile(input, b, 0600); err != nil {
		t.Fatal(err)
	}
	// An accidental manager invocation must fail, never reach a real manager.
	t.Setenv("PATH", t.TempDir())
	old := ui.DryRun
	ui.DryRun = true
	t.Cleanup(func() { ui.DryRun = old })
	var out bytes.Buffer
	if err = runRecoverySignalNativeCLI([]string{"--local", "--spec", input}, &out); err != nil || !strings.Contains(out.String(), "[DRY]") {
		t.Fatal(err, out.String())
	}
	for _, p := range []string{spec.GatewayState, spec.AccountState, spec.RelaySource, spec.RelayDestination} {
		if _, err = os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("dry-run touched state", p, err)
		}
	}
	for _, args := range [][]string{{"--spec", input}, {"--local", "--spec", "relative"}, {"--local", "--spec", input, "extra"}} {
		if err = runRecoverySignalNativeCLI(args, &out); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
	spec.GatewaySHA256 = strings.Repeat("0", 64)
	if _, err = spec.prepare(); err == nil {
		t.Fatal("gateway hash drift accepted")
	}
}
