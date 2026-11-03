package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func signalServiceManagerProperties(m SignalServiceManifest, state signalServiceState) map[string]string {
	p := map[string]string{"LoadState": "not-found", "ActiveState": "inactive", "SubState": "dead", "FragmentPath": "", "UnitFileState": "", "Names": signalServiceUnit, "NeedDaemonReload": "no", "Type": "exec", "MainPID": "0"}
	for _, key := range signalServiceEmptyProperties {
		p[key] = ""
	}
	for key, value := range signalServiceFixedProperties {
		p[key] = value
	}
	p["ControlPID"] = "0"
	if state.Loaded {
		p["LoadState"] = "loaded"
		p["FragmentPath"] = filepath.Join(m.Spec.UnitDirectory, signalServiceUnit)
		p["UnitFileState"] = "disabled"
	}
	if state.Enabled {
		p["UnitFileState"] = "enabled"
	}
	if state.Active {
		p["ActiveState"] = "active"
		p["SubState"] = "running"
		p["MainPID"] = strconv.FormatUint(uint64(state.PID), 10)
	}
	return p
}
func signalServiceExecOutput(m SignalServiceManifest) []byte {
	b, _ := json.Marshal(map[string]any{"type": "a(sasasttttuii)", "data": [][]any{{m.executable(), m.commandArgs(), []string{}, 0, 0, 0, 0, 0, 0, 0}}})
	return b
}
func signalServiceManagerOutput(t *testing.T, m SignalServiceManifest, p map[string]string, program string, args ...string) ([]byte, error) {
	t.Helper()
	switch program {
	case "systemctl":
		if len(args) != 6 || !reflect.DeepEqual(args[:5], []string{"--user", "--no-pager", "show", signalServiceUnit, "--all"}) || !strings.HasPrefix(args[5], "--property=") {
			t.Fatalf("unexpected manager call: %q", args)
		}
		return nativeSignalManagerOutput(p), nil
	case "busctl":
		if !reflect.DeepEqual(args, []string{"--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2dverification_2eservice", "org.freedesktop.systemd1.Service", "ExecStartEx"}) {
			t.Fatalf("unexpected bus call: %q", args)
		}
		return signalServiceExecOutput(m), nil
	}
	t.Fatalf("unexpected command: %s", program)
	return nil, errors.New("unexpected command")
}
func TestSignalServiceManagerProof(t *testing.T) {
	m := SignalServiceManifest{Spec: SignalServiceSpec{Root: "/private/service", UnitDirectory: "/private/units", Operation: "n_01234567890123456789012345678901"}}
	state := signalServiceState{Loaded: true}
	p := signalServiceManagerProperties(m, state)
	u := userSignalServiceManager{run: func(ctx context.Context, program string, args ...string) ([]byte, error) {
		return signalServiceManagerOutput(t, m, p, program, args...)
	}}
	if got, err := u.Inspect(context.Background(), m); err != nil || got != state {
		t.Fatal(got, err)
	}
	for _, change := range [][2]string{{"Names", signalServiceUnit + " alias.service"}, {"DropInPaths", "/foreign"}, {"FragmentPath", "/foreign"}, {"NeedDaemonReload", "yes"}, {"UnitFileState", "masked"}, {"Job", "3"}, {"ControlPID", "3"}, {"Environment", "SECRET=x"}, {"BindPaths", "/foreign"}, {"Type", "forking"}, {"MainPID", "01"}, {"KillMode", "process"}, {"SendSIGKILL", "no"}, {"TimeoutStopUSec", "infinity"}, {"RemainAfterExit", "yes"}, {"Delegate", "yes"}} {
		t.Run(change[0], func(t *testing.T) {
			old := p[change[0]]
			p[change[0]] = change[1]
			defer func() { p[change[0]] = old }()
			if _, err := u.Inspect(context.Background(), m); err == nil {
				t.Fatal("unsafe property accepted")
			}
		})
	}
	p = signalServiceManagerProperties(m, signalServiceState{})
	if got, err := u.Inspect(context.Background(), m); err != nil || got.Loaded {
		t.Fatal(got, err)
	}
	p["UnitFileState"] = "enabled"
	if _, err := u.Inspect(context.Background(), m); err == nil {
		t.Fatal("enabled orphan accepted")
	}
	p = signalServiceManagerProperties(m, state)
	u.run = func(_ context.Context, program string, args ...string) ([]byte, error) {
		if program == "busctl" {
			wrong := m
			wrong.Spec.Root = "/foreign"
			return signalServiceExecOutput(wrong), nil
		}
		return signalServiceManagerOutput(t, m, p, program, args...)
	}
	if _, err := u.Inspect(context.Background(), m); err == nil {
		t.Fatal("wrong typed ExecStart accepted")
	}
}

func TestSignalServiceCommandOutputHelper(t *testing.T) {
	if os.Getenv("DEPLOY_SIGNAL_MANAGER_OUTPUT_HELPER") != "1" {
		t.Skip("subprocess only")
	}
	os.Stdout.WriteString("retained stdout")
	os.Stderr.WriteString("private diagnostic")
	os.Exit(3)
}
func TestSignalServiceCommandPreservesFailureOutput(t *testing.T) {
	t.Setenv("DEPLOY_SIGNAL_MANAGER_OUTPUT_HELPER", "1")
	raw, err := (userSignalServiceManager{}).command(context.Background(), os.Args[0], "-test.run=^TestSignalServiceCommandOutputHelper$")
	if err == nil || string(raw) != "retained stdout" {
		t.Fatal("failed command lost its inspectable output", err, string(raw))
	}
}
