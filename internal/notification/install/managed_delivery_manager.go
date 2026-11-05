package install

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type userDeliveryInspector struct {
	run func(context.Context, string, ...string) ([]byte, error)
}

// Before daemon-reload the manager can still hold the journal's prior command,
// including after the replacement unit has been published. Accept only those
// two retained revisions; readiness after reload requires After alone.
func (i managedInstaller) inspectDeliveryTransition(ctx context.Context, j managedJournal) error {
	m := j.Intent.After
	if err := i.inspectDelivery(ctx, m, true); err != nil {
		if j.Intent.Before == nil {
			return err
		}
		before := *j.Intent.Before
		before.DeliveryOwnerHash = m.DeliveryOwnerHash
		return i.inspectDelivery(ctx, before, true)
	}
	return nil
}

func managedGatewayArgs(m ManagedManifest) []string {
	return []string{m.Paths.Executable, "notification-worker", "__managed-serve", "--local", "--root", m.Root, "--revision", m.Revision}
}

func (u userDeliveryInspector) snapshot(ctx context.Context, m ManagedManifest, allowStale bool) (map[string]string, uint32, error) {
	fixed := map[string]string{"Type": "simple", "ProtectSystem": "strict", "ProtectHome": "read-only", "NoNewPrivileges": "yes", "PrivateUsers": "yes", "PrivateDevices": "yes", "ReadWritePaths": m.Paths.State}
	for k, v := range signalServiceFixedProperties {
		fixed[k] = v
	}
	empty := append(slices.Clone(signalServiceEmptyProperties), "CapabilityBoundingSet", "AmbientCapabilities")
	keys := append([]string{"LoadState", "ActiveState", "SubState", "FragmentPath", "Names", "UnitFileState", "MainPID", "NeedDaemonReload", "RestrictAddressFamilies"}, empty...)
	for k := range fixed {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	raw, err := (userSignalServiceManager{run: u.run}).command(ctx, "systemctl", "--user", "--no-pager", "show", managedUnit, "--all", "--property="+strings.Join(keys, ","))
	if err != nil {
		return nil, 0, fmt.Errorf("delivery manager inspection failed")
	}
	p := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, 0, fmt.Errorf("invalid delivery manager property")
		}
		if _, seen := p[k]; seen {
			return nil, 0, fmt.Errorf("duplicate delivery manager property")
		}
		p[k] = v
	}
	if len(p) != len(keys) {
		return nil, 0, fmt.Errorf("incomplete delivery manager property set")
	}
	for _, key := range keys {
		if _, ok := p[key]; !ok {
			return nil, 0, fmt.Errorf("missing delivery manager property")
		}
	}
	for _, key := range empty {
		want := ""
		if key == "ControlPID" {
			want = "0"
		}
		if p[key] != want {
			return nil, 0, fmt.Errorf("delivery manager has unsupported %s", key)
		}
	}
	for k, want := range fixed {
		if p[k] != want {
			return nil, 0, fmt.Errorf("delivery manager profile %s differs", k)
		}
	}
	if p["LoadState"] != "loaded" || p["FragmentPath"] != filepath.Join(m.UnitDirectory, managedUnit) || p["Names"] != managedUnit || (p["UnitFileState"] != "enabled" && p["UnitFileState"] != "disabled") || (p["NeedDaemonReload"] != "no" && (!allowStale || p["NeedDaemonReload"] != "yes")) {
		return nil, 0, fmt.Errorf("delivery manager unit differs")
	}
	families := strings.Fields(p["RestrictAddressFamilies"])
	slices.Sort(families)
	unixOnly := reflect.DeepEqual(families, []string{"AF_UNIX"})
	delivery := reflect.DeepEqual(families, []string{"AF_INET", "AF_INET6", "AF_UNIX"})
	if !allowStale && ((m.Mode == "delivery" && !delivery) || (m.Mode != "delivery" && !unixOnly)) || allowStale && !unixOnly && !delivery {
		return nil, 0, fmt.Errorf("delivery networking profile differs")
	}
	pid, err := strconv.ParseUint(p["MainPID"], 10, 32)
	if err != nil || strconv.FormatUint(pid, 10) != p["MainPID"] {
		return nil, 0, fmt.Errorf("invalid delivery manager PID")
	}
	active := p["ActiveState"] == "active" && p["SubState"] == "running" && pid > 0
	inactive := pid == 0 && (p["ActiveState"] == "inactive" && p["SubState"] == "dead" || p["ActiveState"] == "failed" && p["SubState"] == "failed")
	if !active && !inactive {
		return nil, 0, fmt.Errorf("delivery manager process has not settled")
	}
	return p, uint32(pid), nil
}

func (u userDeliveryInspector) inspect(ctx context.Context, m ManagedManifest, allowStale bool) (uint32, error) {
	before, pid, err := u.snapshot(ctx, m, allowStale)
	if err != nil {
		return 0, err
	}
	check := func() error {
		raw, err := (userSignalServiceManager{run: u.run}).command(ctx, "busctl", "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/deploy_2dnotification_2dworker_2eservice", "org.freedesktop.systemd1.Service", "ExecStartEx")
		if err != nil {
			return fmt.Errorf("delivery loaded command unavailable")
		}
		return parseSignalExec(raw, m.Paths.Executable, managedGatewayArgs(m), nil)
	}
	if err = check(); err != nil {
		return 0, err
	}
	if pid > 0 {
		if err = checkManagedNativeProcess(pid, m.Paths.Executable, managedGatewayArgs(m)); err != nil {
			return 0, err
		}
	}
	after, _, err := u.snapshot(ctx, m, allowStale)
	if err != nil {
		return 0, err
	}
	if !reflect.DeepEqual(before, after) {
		return 0, fmt.Errorf("delivery manager changed during inspection")
	}
	return pid, check()
}
