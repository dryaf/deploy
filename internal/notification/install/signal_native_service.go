package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// Only a directly executed host relay is supported. Container/namespace and
// shell wrappers need their own account-mount and image proofs.
var nativeSignalEmptyProperties = []string{
	"SourcePath", "DropInPaths", "Job", "User", "Group", "RootDirectory", "RootImage",
	"BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages",
	"ExtensionDirectories", "Environment", "EnvironmentFiles", "PassEnvironment", "UnsetEnvironment",
	"RuntimeDirectory", "StateDirectory", "CacheDirectory", "LogsDirectory", "ConfigurationDirectory",
	"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload",
}

var nativeSignalFixedProperties = map[string]string{
	"LoadState": "loaded", "Names": nativeSignalUnit, "NeedDaemonReload": "no",
	"ControlPID": "0", "KillMode": "control-group", "SendSIGKILL": "yes",
	"PrivateTmp": "no", "PrivateMounts": "no", "ProtectHome": "no",
	"DynamicUser": "no", "RemainAfterExit": "no", "Delegate": "no",
}

type nativeSignalRecoveryService struct {
	spec         NativeSignalServiceSpec
	binding      string
	run          func(context.Context, string, ...string) ([]byte, error)
	processCheck func(uint32, NativeSignalServiceSpec) error
}

func newNativeSignalRecoveryService(s NativeSignalServiceSpec) *nativeSignalRecoveryService {
	return &nativeSignalRecoveryService{spec: s, run: nativeSignalCommand, processCheck: checkNativeSignalProcess}
}

func nativeSignalCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	b, err := process.LimitedOutput(cmd, 45*time.Second, 128<<10)
	if err != nil {
		// Manager output can contain environment or command arguments. Never
		// include it in diagnostics, including a failed stop's stderr.
		return nil, fmt.Errorf("native Signal manager command failed (%s)", command)
	}
	return b, nil
}

func nativeSignalProperties() []string {
	v := append([]string{}, nativeSignalEmptyProperties...)
	for k := range nativeSignalFixedProperties {
		v = append(v, k)
	}
	v = append(v, "FragmentPath", "UnitFileState", "ActiveState", "SubState", "MainPID", "Type", "TimeoutStopUSec")
	sort.Strings(v)
	return v
}

func (s *nativeSignalRecoveryService) snapshot(ctx context.Context) (map[string]string, bool, uint32, error) {
	keys := nativeSignalProperties()
	b, err := s.run(ctx, "systemctl", "--user", "--no-pager", "show", nativeSignalUnit, "--all", "--property="+strings.Join(keys, ","))
	if err != nil {
		return nil, false, 0, err
	}
	m := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if _, duplicate := m[k]; !ok || duplicate {
			return nil, false, 0, fmt.Errorf("malformed native Signal manager properties")
		}
		m[k] = v
	}
	if len(m) != len(keys) {
		return nil, false, 0, fmt.Errorf("incomplete native Signal manager properties")
	}
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			return nil, false, 0, fmt.Errorf("missing native Signal manager property %s", key)
		}
	}
	for _, k := range nativeSignalEmptyProperties {
		if m[k] != "" {
			return nil, false, 0, fmt.Errorf("unsupported native Signal service property %s", k)
		}
	}
	for k, v := range nativeSignalFixedProperties {
		if m[k] != v {
			return nil, false, 0, fmt.Errorf("native Signal service property %s differs", k)
		}
	}
	if m["FragmentPath"] != s.spec.UnitFile || (m["UnitFileState"] != "enabled" && m["UnitFileState"] != "disabled") || (m["Type"] != "exec" && m["Type"] != "simple") {
		return nil, false, 0, fmt.Errorf("native Signal loaded unit differs from requested host service")
	}
	// Bound stop time and reject disabled timeouts. systemctl uses systemd's
	// human duration representation; deliberately accept only this small profile.
	if m["TimeoutStopUSec"] != "30s" {
		return nil, false, 0, fmt.Errorf("native Signal service requires TimeoutStopSec=30s")
	}
	pid, err := strconv.ParseUint(m["MainPID"], 10, 32)
	if err != nil || strconv.FormatUint(pid, 10) != m["MainPID"] {
		return nil, false, 0, fmt.Errorf("invalid native Signal service PID")
	}
	active := m["ActiveState"] == "active" && m["SubState"] == "running" && pid > 0
	inactive := pid == 0 && ((m["ActiveState"] == "inactive" && m["SubState"] == "dead") || (m["ActiveState"] == "failed" && m["SubState"] == "failed"))
	if !active && !inactive {
		return nil, false, 0, fmt.Errorf("native Signal service is not in a stable state")
	}
	return m, active, uint32(pid), nil
}

func (s NativeSignalServiceSpec) argv() []string {
	return []string{s.Executable, "notification-worker", "__signal-relay", "--local", "--config", s.RelayConfig}
}

// ExecStartEx is a typed D-Bus property. Parsing systemctl's display string would
// lose argument boundaries and the execution-prefix flags.
func parseNativeSignalExec(b []byte, spec NativeSignalServiceSpec) error {
	return parseSignalExec(b, spec.Executable, spec.argv(), nil)
}

func parseSignalExec(b []byte, executable string, argv, expectedFlags []string) error {
	var v struct {
		Type string              `json:"type"`
		Data [][]json.RawMessage `json:"data"`
	}
	if safefs.DecodeJSON(b, &v, 128<<10) != nil || v.Type != "a(sasasttttuii)" || len(v.Data) != 1 || len(v.Data[0]) != 10 {
		return fmt.Errorf("invalid native Signal ExecStartEx property")
	}
	row := v.Data[0]
	var path string
	var args, flags []string
	if json.Unmarshal(row[0], &path) != nil || json.Unmarshal(row[1], &args) != nil || json.Unmarshal(row[2], &flags) != nil || path != executable || !reflect.DeepEqual(args, argv) || strings.Join(flags, "\x00") != strings.Join(expectedFlags, "\x00") || len(flags) != len(expectedFlags) || string(row[2]) == "null" {
		return fmt.Errorf("native Signal loaded executable, arguments or flags differ")
	}
	for i := 3; i < 10; i++ {
		var target any
		switch {
		case i < 7:
			target = new(uint64)
		case i == 7:
			target = new(uint32)
		default:
			target = new(int32)
		}
		if string(row[i]) == "null" || json.Unmarshal(row[i], target) != nil {
			return fmt.Errorf("invalid native Signal execution metadata")
		}
	}
	return nil
}

func (s *nativeSignalRecoveryService) execCheck(ctx context.Context) error {
	b, err := s.run(ctx, "busctl", "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2eservice", "org.freedesktop.systemd1.Service", "ExecStartEx")
	if err != nil {
		return err
	}
	return parseNativeSignalExec(b, s.spec)
}

func (s *nativeSignalRecoveryService) Inspect(ctx context.Context) (signalaccount.ServiceState, error) {
	var zero signalaccount.ServiceState
	_, files, err := s.spec.files()
	if err != nil {
		return zero, err
	}
	before, active, pid, err := s.snapshot(ctx)
	if err != nil {
		return zero, err
	}
	if err = s.execCheck(ctx); err != nil {
		return zero, err
	}
	if active {
		if err = s.processCheck(pid, s.spec); err != nil {
			return zero, err
		}
	}
	after, _, _, err := s.snapshot(ctx)
	if err != nil {
		return zero, err
	}
	if !reflect.DeepEqual(before, after) {
		return zero, fmt.Errorf("native Signal manager changed during inspection")
	}
	if err = s.execCheck(ctx); err != nil {
		return zero, err
	}
	_, again, err := s.spec.files()
	if err != nil {
		return zero, err
	}
	if files != again {
		return zero, fmt.Errorf("native Signal inputs changed during inspection")
	}
	for _, key := range []string{"MainPID", "ActiveState", "SubState"} {
		delete(after, key)
	}
	binding := protocol.Hash(struct {
		Files      string
		Properties map[string]string
	}{files, after})
	if s.binding != "" && binding != s.binding {
		return zero, fmt.Errorf("native Signal service binding changed")
	}
	return signalaccount.ServiceState{Binding: binding, Active: active}, nil
}

func (s *nativeSignalRecoveryService) Stop(ctx context.Context) error {
	if s.binding == "" {
		return fmt.Errorf("native Signal service has no frozen binding")
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

func (s *nativeSignalRecoveryService) Start(context.Context) error {
	return fmt.Errorf("Signal recovery cannot restart a receiver")
}

func (s *nativeSignalRecoveryService) VerifyRecoverySupport(ctx context.Context) error {
	if _, err := s.Inspect(ctx); err != nil {
		return err
	}
	if err := s.spec.probe(ctx); err != nil {
		return err
	}
	_, err := s.Inspect(ctx)
	return err
}

func checkNativeSignalProcess(pid uint32, spec NativeSignalServiceSpec) error {
	root := "/proc/" + strconv.FormatUint(uint64(pid), 10)
	before, err := os.ReadFile(root + "/stat")
	if err != nil {
		return fmt.Errorf("cannot inspect native Signal process")
	}
	f, err := spec.binary()
	if err != nil {
		return err
	}
	defer f.Close()
	want, err := f.Stat()
	if err != nil {
		return err
	}
	got, err := os.Stat(root + "/exe")
	if err != nil || !os.SameFile(want, got) {
		return fmt.Errorf("running native Signal executable differs from pinned inode")
	}
	owner, err := os.Stat(root)
	if err != nil || owner.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("running native Signal process owner differs")
	}
	argv, err := os.ReadFile(root + "/cmdline")
	if err != nil || !bytes.Equal(argv, []byte(strings.Join(spec.argv(), "\x00")+"\x00")) {
		return fmt.Errorf("running native Signal arguments differ")
	}
	c, err := readRelayConfiguration(spec.RelayConfig)
	if err != nil {
		return err
	}
	if err = checkNativeSignalAccount(root, c.StateDir); err != nil {
		return err
	}
	if c.Interactive == nil {
		return fmt.Errorf("native Signal process requires an interactive inbox")
	}
	if err = checkNativeSignalLease(root, filepath.Join(c.Interactive.InboxDir, "inbox.lock")); err != nil {
		return fmt.Errorf("native Signal source inbox lease: %w", err)
	}
	after, err := os.ReadFile(root + "/stat")
	// The start-time field follows the parenthesized command name, which may
	// contain spaces and parentheses. Other stat fields change during execution.
	start := func(b []byte) string {
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			return ""
		}
		fields := strings.Fields(string(b[i+1:]))
		if len(fields) < 20 {
			return ""
		}
		return fields[19]
	}
	if err != nil || start(before) == "" || start(before) != start(after) {
		return fmt.Errorf("native Signal process changed during inspection")
	}
	return nil
}

// A matching argv alone does not establish which configuration an already
// running relay read. Require its actual account lock descriptor, including
// the exclusive flock, and the same account inode through its mount namespace.
func checkNativeSignalAccount(procRoot, account string) error {
	return checkNativeSignalLease(procRoot, filepath.Join(account, "deploy-account.lock"))
}

func checkNativeSignalLease(procRoot, path string) error {
	return checkSignalProcessLease(procRoot, path, path)
}

func checkSignalProcessLease(procRoot, path, visiblePath string) error {
	f, _, err := nativeSignalFile(path, 4096, true)
	if err != nil {
		return err
	}
	defer f.Close()
	want, err := f.Stat()
	if err != nil {
		return err
	}
	inProcess, err := os.Stat(procRoot + "/root" + visiblePath)
	if err != nil || !os.SameFile(want, inProcess) {
		return fmt.Errorf("native Signal process sees a different lease inode")
	}
	d, err := os.Open(procRoot + "/fd")
	if err != nil {
		return fmt.Errorf("cannot inspect native Signal lease descriptors")
	}
	defer d.Close()
	entries, err := d.ReadDir(4097)
	if err != nil && err != io.EOF || len(entries) > 4096 {
		return fmt.Errorf("native Signal descriptor inventory unavailable or too large")
	}
	for _, entry := range entries {
		got, e := os.Stat(procRoot + "/fd/" + entry.Name())
		if e != nil || !os.SameFile(want, got) {
			continue
		}
		b, e := os.ReadFile(procRoot + "/fdinfo/" + entry.Name())
		if e != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 9 && fields[0] == "lock:" && fields[2] == "FLOCK" && fields[3] == "ADVISORY" && fields[4] == "WRITE" && fields[7] == "0" && fields[8] == "EOF" {
				return nil
			}
		}
	}
	return fmt.Errorf("native Signal process does not hold the requested lease")
}
