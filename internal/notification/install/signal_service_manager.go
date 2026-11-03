package install

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

type signalServiceState struct {
	Loaded, Active bool
	PID            uint32
	Enabled        bool
}
type signalServiceManager interface {
	Inspect(context.Context, SignalServiceManifest) (signalServiceState, error)
	Reload(context.Context) error
	Start(context.Context) error
	Stop(context.Context) error
	Disable(context.Context) error
	Verify(context.Context, SignalServiceManifest) error
}
type userSignalServiceManager struct {
	run func(context.Context, string, ...string) ([]byte, error)
}

func (u userSignalServiceManager) command(ctx context.Context, program string, args ...string) ([]byte, error) {
	if u.run != nil {
		return u.run(ctx, program, args...)
	}
	cmd := exec.CommandContext(ctx, program, args...)
	var stdout, stderr process.Buffer
	stdout.Limit = 128 << 10
	stderr.Limit = 4096
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := process.Run(cmd, 45*time.Second, 10*time.Second)
	// systemctl show may exit nonzero for a nonexistent unit while still
	// returning the complete property set needed to prove that it is unowned.
	return stdout.Bytes(), err
}
func (u userSignalServiceManager) ctl(ctx context.Context, args ...string) error {
	_, err := u.command(ctx, "systemctl", append([]string{"--user", "--no-pager"}, args...)...)
	if err != nil {
		return fmt.Errorf("Signal verifier manager command failed")
	}
	return nil
}
func (u userSignalServiceManager) Reload(ctx context.Context) error {
	return u.ctl(ctx, "daemon-reload")
}
func (u userSignalServiceManager) Start(ctx context.Context) error {
	return u.ctl(ctx, "enable", "--now", signalServiceUnit)
}
func (u userSignalServiceManager) Stop(ctx context.Context) error {
	return u.ctl(ctx, "stop", signalServiceUnit)
}
func (u userSignalServiceManager) Disable(ctx context.Context) error {
	return u.ctl(ctx, "disable", signalServiceUnit)
}

var signalServiceEmptyProperties = []string{"DropInPaths", "SourcePath", "ControlPID", "Job", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload", "ExecCondition", "Environment", "EnvironmentFiles", "RootDirectory", "RootImage", "User", "Group", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories", "PassEnvironment", "UnsetEnvironment", "RuntimeDirectory", "StateDirectory", "CacheDirectory", "LogsDirectory", "ConfigurationDirectory"}

var signalServiceFixedProperties = map[string]string{
	"KillMode": "control-group", "SendSIGKILL": "yes", "RemainAfterExit": "no", "Delegate": "no", "TimeoutStopUSec": "35s",
}

func (u userSignalServiceManager) snapshot(ctx context.Context, m SignalServiceManifest) (map[string]string, signalServiceState, error) {
	var out signalServiceState
	keys := append([]string{"LoadState", "ActiveState", "SubState", "FragmentPath", "UnitFileState", "Names", "NeedDaemonReload", "Type", "MainPID"}, signalServiceEmptyProperties...)
	for key := range signalServiceFixedProperties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	raw, err := u.command(ctx, "systemctl", "--user", "--no-pager", "show", signalServiceUnit, "--all", "--property="+strings.Join(keys, ","))
	if err != nil && len(raw) == 0 {
		return nil, out, fmt.Errorf("Signal verifier manager inspection failed")
	}
	commandErr := err
	props := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, out, fmt.Errorf("malformed Signal verifier manager state")
		}
		if _, exists := props[k]; exists {
			return nil, out, fmt.Errorf("duplicate Signal verifier manager property")
		}
		props[k] = v
	}
	if len(props) != len(keys) {
		return nil, out, fmt.Errorf("incomplete Signal verifier manager state")
	}
	for _, k := range keys {
		if _, ok := props[k]; !ok {
			return nil, out, fmt.Errorf("missing Signal verifier property")
		}
	}
	for _, k := range signalServiceEmptyProperties {
		want := ""
		if k == "ControlPID" {
			want = "0"
		}
		if props[k] != want {
			return nil, out, fmt.Errorf("unsupported Signal verifier property %s", k)
		}
	}
	if props["Names"] != "" && props["Names"] != signalServiceUnit {
		return nil, out, fmt.Errorf("Signal verifier has aliases")
	}
	if props["LoadState"] == "not-found" && props["ActiveState"] == "inactive" && props["FragmentPath"] == "" && props["MainPID"] == "0" && props["UnitFileState"] == "" && props["SubState"] == "dead" && props["NeedDaemonReload"] == "no" {
		return props, out, nil
	}
	if commandErr != nil {
		return nil, out, fmt.Errorf("Signal verifier manager inspection failed")
	}
	if props["LoadState"] != "loaded" || props["FragmentPath"] != filepath.Join(m.Spec.UnitDirectory, signalServiceUnit) || props["Names"] != signalServiceUnit || props["NeedDaemonReload"] != "no" || props["Type"] != "exec" || (props["UnitFileState"] != "enabled" && props["UnitFileState"] != "disabled") {
		return nil, out, fmt.Errorf("Signal verifier loaded unit differs")
	}
	for key, want := range signalServiceFixedProperties {
		if props[key] != want {
			return nil, out, fmt.Errorf("Signal verifier property %s differs", key)
		}
	}
	pid, err := strconv.ParseUint(props["MainPID"], 10, 32)
	if err != nil || strconv.FormatUint(pid, 10) != props["MainPID"] {
		return nil, out, fmt.Errorf("invalid Signal verifier PID")
	}
	active := props["ActiveState"] == "active" && props["SubState"] == "running" && pid > 0
	inactive := pid == 0 && (props["ActiveState"] == "inactive" && props["SubState"] == "dead" || props["ActiveState"] == "failed" && props["SubState"] == "failed")
	if !active && !inactive {
		return nil, out, fmt.Errorf("Signal verifier is not in a stable state")
	}
	out = signalServiceState{Loaded: true, Active: active, PID: uint32(pid), Enabled: props["UnitFileState"] == "enabled"}
	return props, out, nil
}
func (u userSignalServiceManager) Inspect(ctx context.Context, m SignalServiceManifest) (signalServiceState, error) {
	before, state, err := u.snapshot(ctx, m)
	if err != nil || !state.Loaded {
		return state, err
	}
	check := func() error {
		raw, err := u.command(ctx, "busctl", "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2dverification_2eservice", "org.freedesktop.systemd1.Service", "ExecStartEx")
		if err != nil {
			return fmt.Errorf("Signal verifier manager command inspection failed")
		}
		return parseSignalExec(raw, m.executable(), m.commandArgs(), nil)
	}
	if err = check(); err != nil {
		return state, err
	}
	after, _, err := u.snapshot(ctx, m)
	if err != nil {
		return state, err
	}
	if !reflect.DeepEqual(before, after) {
		return state, fmt.Errorf("Signal verifier manager changed during inspection")
	}
	if err = check(); err != nil {
		return state, err
	}
	if state.Active {
		if err = checkSignalServiceProcess(state.PID, m); err != nil {
			return state, err
		}
	}
	return state, nil
}
func (u userSignalServiceManager) Verify(ctx context.Context, m SignalServiceManifest) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		err := u.verifyOnce(ctx, m)
		if err == nil {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("Signal verifier failed readiness check: %w", err)
		case <-timer.C:
		}
	}
}
func (u userSignalServiceManager) verifyOnce(ctx context.Context, m SignalServiceManifest) error {
	before, state, err := u.snapshot(ctx, m)
	if err != nil {
		return err
	}
	if !state.Active || !state.Enabled {
		return fmt.Errorf("Signal verifier is not enabled and running")
	}
	if _, err = u.Inspect(ctx, m); err != nil {
		return err
	}
	gateway := ManagedManifest{SignalVerification: &ManagedSignalBinding{SignalVerificationReference: SignalVerificationReference{Root: m.Spec.Endpoint.Root, ManifestHash: m.Spec.Endpoint.ManifestHash, ServiceRoot: m.Spec.Root, ServiceHash: m.ManifestHash}}}
	if err = checkManagedSignalHealthPID(ctx, gateway, state.PID); err != nil {
		return err
	}
	after, _, err := u.snapshot(ctx, m)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("Signal verifier changed during health check")
	}
	return nil
}

func checkSignalServiceProcess(pid uint32, m SignalServiceManifest) error {
	root := fmt.Sprintf("/proc/%d", pid)
	before, err := os.ReadFile(root + "/stat")
	if err != nil {
		return err
	}
	expected, err := os.Stat(m.executable())
	if err != nil {
		return err
	}
	actual, err := os.Stat(root + "/exe")
	if err != nil || !os.SameFile(expected, actual) {
		return fmt.Errorf("Signal verifier process executable differs")
	}
	owner, err := os.Stat(root)
	if err != nil || owner.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("Signal verifier process owner differs")
	}
	argv, err := os.ReadFile(root + "/cmdline")
	if err != nil || !bytes.Equal(argv, []byte(strings.Join(m.commandArgs(), "\x00")+"\x00")) {
		return fmt.Errorf("Signal verifier process arguments differ")
	}
	after, err := os.ReadFile(root + "/stat")
	if err != nil {
		return err
	}
	start := func(raw []byte) string {
		idx := bytes.LastIndexByte(raw, ')')
		if idx < 0 {
			return ""
		}
		fields := strings.Fields(string(raw[idx+1:]))
		if len(fields) < 20 {
			return ""
		}
		return fields[19]
	}
	if start(before) == "" || start(before) != start(after) {
		return fmt.Errorf("Signal verifier PID changed")
	}
	return nil
}
