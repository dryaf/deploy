package install

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func freshSignalManagerProperties(m FreshSignalManifest, state signalServiceState) map[string]string {
	p := signalServiceManagerProperties(SignalServiceManifest{Spec: SignalServiceSpec{UnitDirectory: m.Spec.UnitDirectory}}, state)
	p["Names"] = freshSignalUnit
	if state.Loaded {
		p["FragmentPath"] = filepath.Join(m.Spec.UnitDirectory, freshSignalUnit)
	}
	return p
}
func freshSignalManagerOutput(m FreshSignalManifest, p map[string]string, program string, args ...string) ([]byte, error) {
	switch program {
	case "systemctl":
		if len(args) != 6 || args[3] != freshSignalUnit || args[2] != "show" {
			return nil, fmt.Errorf("unexpected synthetic manager request")
		}
		return nativeSignalManagerOutput(p), nil
	case "busctl":
		if len(args) != 7 || args[4] != "/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2eservice" {
			return nil, fmt.Errorf("wrong fresh Signal object")
		}
		return json.Marshal(map[string]any{"type": "a(sasasttttuii)", "data": [][]any{{m.executable(), m.commandArgs(), []string{}, 0, 0, 0, 0, 0, 0, 0}}})
	}
	return nil, fmt.Errorf("unexpected synthetic command")
}
func TestFreshSignalManagerRefusesForeignExecution(t *testing.T) {
	m := FreshSignalManifest{Spec: FreshSignalSpec{Root: "/private/fresh", UnitDirectory: "/private/units", Operation: "n_01234567890123456789012345678901"}}
	p := freshSignalManagerProperties(m, signalServiceState{Loaded: true})
	u := userFreshSignalManager{userSignalServiceManager{run: func(_ context.Context, program string, args ...string) ([]byte, error) {
		return freshSignalManagerOutput(m, p, program, args...)
	}}}
	if _, err := u.Inspect(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{{"Names", freshSignalUnit + " alias.service"}, {"DropInPaths", "/foreign"}, {"FragmentPath", "/foreign"}, {"NeedDaemonReload", "yes"}, {"Environment", "SECRET=fixture"}, {"KillMode", "process"}, {"ExecStartPre", "/foreign"}, {"ControlPID", "14"}} {
		old := p[change[0]]
		p[change[0]] = change[1]
		if _, err := u.Inspect(context.Background(), m); err == nil {
			t.Fatal("foreign execution admitted", change[0])
		}
		p[change[0]] = old
	}
	u.run = func(_ context.Context, program string, args ...string) ([]byte, error) {
		if program == "busctl" {
			other := m
			other.Spec.Operation = strings.ReplaceAll(m.Spec.Operation, "0", "1")
			return freshSignalManagerOutput(other, p, program, args...)
		}
		return freshSignalManagerOutput(m, p, program, args...)
	}
	if _, err := u.Inspect(context.Background(), m); err == nil {
		t.Fatal("foreign operation admitted")
	}
}
