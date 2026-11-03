package install

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

type podmanSignalRecoveryService struct {
	spec    PodmanSignalServiceSpec
	binding string
	run     signalPodmanRun
}

func newPodmanSignalRecoveryService(s PodmanSignalServiceSpec) *podmanSignalRecoveryService {
	return &podmanSignalRecoveryService{spec: s, run: signalPodmanLocalCommand}
}

func (s PodmanSignalServiceSpec) managerProperties() map[string]string {
	want := map[string]string{}
	for _, k := range nativeSignalEmptyProperties {
		if k != "ExecStop" && k != "ExecStopPost" {
			want[k] = ""
		}
	}
	for k, v := range nativeSignalFixedProperties {
		want[k] = v
	}
	want["SourcePath"], want["FragmentPath"], want["UnitFileState"] = s.Quadlet, s.Fragment, "generated"
	want["Environment"], want["Delegate"], want["KillMode"] = "PODMAN_SYSTEMD_UNIT="+nativeSignalUnit, "yes", "mixed"
	want["Type"], want["NotifyAccess"], want["TimeoutStopUSec"], want["Restart"] = "notify", "all", "30s", "on-failure"
	return want
}

func (s *podmanSignalRecoveryService) snapshot(ctx context.Context) (map[string]string, bool, uint32, error) {
	want := s.spec.managerProperties()
	keys := []string{"ActiveState", "SubState", "MainPID"}
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, err := s.run(ctx, "systemctl", "--user", "--no-pager", "show", nativeSignalUnit, "--all", "--property="+strings.Join(keys, ","))
	if err != nil {
		return nil, false, 0, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if _, dup := m[k]; !ok || dup {
			return nil, false, 0, fmt.Errorf("invalid Signal Quadlet manager properties")
		}
		m[k] = v
	}
	if len(m) != len(keys) {
		return nil, false, 0, fmt.Errorf("incomplete Signal Quadlet manager properties")
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			return nil, false, 0, fmt.Errorf("Signal Quadlet manager property %s differs", k)
		}
	}
	pid, err := strconv.ParseUint(m["MainPID"], 10, 32)
	if err != nil || strconv.FormatUint(pid, 10) != m["MainPID"] {
		return nil, false, 0, fmt.Errorf("invalid Signal Quadlet PID")
	}
	active := m["ActiveState"] == "active" && m["SubState"] == "running" && pid > 0
	stopped := pid == 0 && ((m["ActiveState"] == "inactive" && m["SubState"] == "dead") || (m["ActiveState"] == "failed" && m["SubState"] == "failed"))
	if !active && !stopped {
		return nil, false, 0, fmt.Errorf("Signal Quadlet service is transitioning")
	}
	return m, active, uint32(pid), nil
}

func (s *podmanSignalRecoveryService) commands(ctx context.Context) error {
	for _, name := range []string{"ExecStartEx", "ExecStopEx", "ExecStopPostEx"} {
		b, err := s.run(ctx, "busctl", "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2eservice", "org.freedesktop.systemd1.Service", name)
		if err != nil {
			return err
		}
		args := s.spec.startArgv()
		var flags []string
		if name == "ExecStartEx" {
			var v struct {
				Type string
				Data [][]json.RawMessage
			}
			if safefs.DecodeJSON(b, &v, 128<<10) != nil || len(v.Data) != 1 || len(v.Data[0]) != 10 || json.Unmarshal(v.Data[0][1], &args) != nil {
				return fmt.Errorf("invalid Signal Podman typed command")
			}
			if err = s.spec.checkStartArgv(args); err != nil {
				return err
			}
		}
		if name != "ExecStartEx" {
			args = []string{"/usr/bin/podman", "--remote=false", "rm", "-v", "-f", "-i", podmanSignalName}
		}
		if name == "ExecStopPostEx" {
			flags = []string{"ignore-failure"}
		}
		if err = parseSignalExec(b, "/usr/bin/podman", args, flags); err != nil {
			return err
		}
	}
	return nil
}

func (s *podmanSignalRecoveryService) Inspect(ctx context.Context) (signalaccount.ServiceState, error) {
	var zero signalaccount.ServiceState
	_, files, err := s.spec.files()
	if err != nil {
		return zero, err
	}
	store, err := signalPodmanStore(ctx, s.run)
	if err != nil {
		return zero, err
	}
	if err = s.spec.image(ctx, s.run); err != nil {
		return zero, err
	}
	before, active, pid, err := s.snapshot(ctx)
	if err != nil {
		return zero, err
	}
	if err = s.commands(ctx); err != nil {
		return zero, err
	}
	c, err := s.spec.container(ctx, s.run, active, pid)
	if err != nil {
		return zero, err
	}
	if active {
		if err = signalPodmanProof(ctx, s.run, s.spec.proof(*c)); err != nil {
			return zero, err
		}
	}
	after, againActive, againPID, err := s.snapshot(ctx)
	if err != nil {
		return zero, err
	}
	if !reflect.DeepEqual(before, after) {
		return zero, fmt.Errorf("Signal Quadlet service changed during inspection")
	}
	again, err := s.spec.container(ctx, s.run, againActive, againPID)
	if err != nil {
		return zero, err
	}
	if !reflect.DeepEqual(c, again) {
		return zero, fmt.Errorf("Signal container changed during inspection")
	}
	if err = s.commands(ctx); err != nil {
		return zero, err
	}
	_, againFiles, err := s.spec.files()
	if err != nil {
		return zero, err
	}
	againStore, err := signalPodmanStore(ctx, s.run)
	if err != nil {
		return zero, err
	}
	if files != againFiles || store != againStore {
		return zero, fmt.Errorf("Signal Podman files/store changed")
	}
	for _, k := range []string{"MainPID", "ActiveState", "SubState"} {
		delete(after, k)
	}
	binding := protocol.Hash(struct {
		Files, Store string
		Properties   map[string]string
	}{files, store, after})
	if s.binding != "" && s.binding != binding {
		return zero, fmt.Errorf("Signal Podman recovery binding changed")
	}
	return signalaccount.ServiceState{Binding: binding, Active: active}, nil
}

func (s *podmanSignalRecoveryService) Stop(ctx context.Context) error {
	if s.binding == "" {
		return fmt.Errorf("Signal Podman service binding not frozen")
	}
	v, err := s.Inspect(ctx)
	if err != nil {
		return err
	}
	if !v.Active {
		return nil
	}
	_, err = s.run(ctx, "systemctl", "--user", "--no-pager", "stop", nativeSignalUnit)
	return err
}
func (s *podmanSignalRecoveryService) Start(context.Context) error {
	return fmt.Errorf("Signal recovery cannot restart a receiver")
}

func (s *podmanSignalRecoveryService) VerifyRecoverySupport(ctx context.Context) error {
	if _, err := s.Inspect(ctx); err != nil {
		return err
	}
	// A separate disposable container has no account/configuration mounts and
	// cannot pull, contact a provider or initialize image-declared volumes.
	b, err := signalPodmanCommand(ctx, s.run, "run", "--rm", "--pull=never", "--network=none", "--read-only", "--image-volume=ignore", "--cap-drop=all", "--security-opt=no-new-privileges", "--user=0", "--memory=128m", "--pids-limit=64", "--entrypoint="+podmanSignalExecutable, "sha256:"+s.spec.ImageID, "notification-worker", "__signal-podman-recovery-probe")
	if err != nil {
		return fmt.Errorf("Signal container recovery capability probe failed")
	}
	var v struct {
		Version          int    `json:"version"`
		ExecutableSHA256 string `json:"executable_sha256"`
	}
	if safefs.DecodeJSON(b, &v, 4096) != nil || v.Version != signalaccount.RecoveryVersion || v.ExecutableSHA256 != s.spec.ExecutableSHA256 {
		return fmt.Errorf("Signal image lacks recovery fence support")
	}
	_, err = s.Inspect(ctx)
	return err
}
