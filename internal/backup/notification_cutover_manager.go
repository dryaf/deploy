package backup

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupCutoverManager struct {
	freshShared  bool
	home         string
	paths        WorkerUnitPaths
	legacy       map[string]string
	legacyNative map[string]string
	run          func(context.Context, string, ...string) ([]byte, error)
}

func cutoverCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return process.LimitedOutput(exec.CommandContext(ctx, name, args...), 45*time.Second, 16384)
}
func cutoverSystemctl(ctx context.Context, args ...string) error {
	_, err := cutoverCommand(ctx, "systemctl", append([]string{"--user", "--no-pager"}, args...)...)
	return err
}
func (u *backupCutoverManager) inspect(ctx context.Context, name, source string, stopped, enabled bool) error {
	return u.inspectReload(ctx, name, source, stopped, enabled, false)
}

// A retained report-unit replacement may have reached disk just before a crash.
// Only that caller may inspect its exact fragment while a reload is still due.
func (u *backupCutoverManager) inspectReload(ctx context.Context, name, source string, stopped, enabled, pendingReload bool) error {
	properties := "LoadState,ActiveState,FragmentPath,SourcePath,DropInPaths,Names,MainPID,ControlPID,NeedDaemonReload,UnitFileState"
	service := strings.HasSuffix(name, ".service")
	if service {
		properties += ",KillMode,SendSIGKILL"
	}
	run := u.run
	if run == nil {
		run = cutoverCommand
	}
	body, err := run(ctx, "systemctl", "--user", "--no-pager", "show", name, "--property="+properties)
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("invalid service inspection")
		}
		if _, ok = values[key]; ok {
			return fmt.Errorf("duplicate service inspection")
		}
		values[key] = value
	}
	reloadOK := values["NeedDaemonReload"] == "no" || pendingReload && values["NeedDaemonReload"] == "yes"
	if len(values) != len(strings.Split(properties, ",")) || values["LoadState"] != "loaded" || values["DropInPaths"] != "" || values["Names"] != name || !reloadOK {
		return fmt.Errorf("service %s is not an exact loaded unit", name)
	}
	if service && (values["KillMode"] != "control-group" || values["SendSIGKILL"] != "yes") {
		return fmt.Errorf("service %s does not drain its process group", name)
	}
	if strings.HasSuffix(source, ".container") {
		generated := filepath.Join(fmt.Sprintf("/run/user/%d/systemd/generator", os.Getuid()), name)
		if values["SourcePath"] != source || values["FragmentPath"] != generated {
			return fmt.Errorf("foreign generated sender unit %s", name)
		}
	} else if values["FragmentPath"] != source {
		return fmt.Errorf("foreign service fragment %s", name)
	}
	if stopped && (values["ActiveState"] != "inactive" && values["ActiveState"] != "failed" || values["MainPID"] != "0" || values["ControlPID"] != "0") {
		return fmt.Errorf("legacy sender %s is not drained", name)
	}
	if enabled && (values["ActiveState"] != "active" || values["UnitFileState"] != "enabled") {
		return fmt.Errorf("report trigger %s is not active and enabled", name)
	}
	return nil
}
func (u *backupCutoverManager) Prepare(ctx context.Context, c WorkerInstallation, p WorkerUnitPaths, helperHash string) error {
	u.home, u.paths = p.Home, p
	u.freshShared = c.Policy.Notifications.Mode == "shared"
	if u.freshShared {
		if err := u.requireNoDirectSenders(ctx); err != nil {
			return err
		}
	}
	for _, name := range []string{"deploy-backup-report.service", "deploy-backup-report.timer", "deploy-backup-report.path"} {
		if err := u.reportSlot(ctx, name); err != nil {
			return err
		}
	}
	units, err := renderWorkerUnits(c, p, c.Image, c.RelayImage)
	if err != nil {
		return err
	}
	root, err := safefs.Open(p.Home)
	if err != nil {
		return err
	}
	defer root.Close()
	u.legacy = map[string]string{}
	names := []string{"deploy-backup-notifier.container", "deploy-backup-notifier.timer", "deploy-backup-notifier.path"}
	for _, provider := range []string{c.Policy.Notifications.Primary, c.Policy.Notifications.Fallback} {
		if provider != "" {
			names = append(names, "deploy-backup-test-"+provider+".container")
		}
	}
	if c.Policy.Notifications.Signal != nil {
		names = append(names, "deploy-backup-signal.container")
	}
	if u.freshShared {
		names = nil
	}
	for _, name := range names {
		base := p.unitDirectory()
		if strings.HasSuffix(name, ".container") {
			base = p.quadletDirectory()
		}
		raw, err := readBackupOwnedFile(root, filepath.Join(base, name), 1<<20)
		if err != nil {
			return err
		}
		if string(raw) != units[name] || units[name] == "" {
			return fmt.Errorf("legacy unit %s changed", name)
		}
		service := strings.TrimSuffix(name, ".container")
		if strings.HasSuffix(name, ".container") {
			service += ".service"
		}
		source := filepath.Join(base, name)
		if err = u.inspect(ctx, service, source, false, false); err != nil {
			return err
		}
		u.legacy[service] = source
	}
	helper, err := openBackupOwnedFile(root, p.helperFile(), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer helper.Close()
	stat, err := helper.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("unsafe backup helper")
	}
	digest := sha256.New()
	if _, err = io.Copy(digest, io.LimitReader(helper, (256<<20)+1)); err != nil {
		return err
	}
	if stat.Size() > 256<<20 || fmt.Sprintf("%x", digest.Sum(nil)) != helperHash {
		return fmt.Errorf("backup helper hash changed")
	}
	// Probe the already opened file, not a PATH-selected or replaceable pathname.
	probe, expected := "__notification-cutover-probe", "backup-notification-cutover-v1"
	if u.freshShared {
		probe, expected = "__shared-reporting-probe", "backup-shared-reporting-v1"
	}
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "backup-worker", probe)
	cmd.ExtraFiles = []*os.File{helper}
	output, err := process.LimitedOutput(cmd, 15*time.Second, 1024)
	if err != nil || strings.TrimSpace(string(output)) != expected {
		return fmt.Errorf("installed backup helper lacks ownership fencing/report support")
	}
	output, err = cutoverCommand(ctx, "podman", "run", "--rm", "--pull=never", "--network=none", "--read-only", c.Image, "backup-worker", probe)
	if err != nil || strings.TrimSpace(string(output)) != expected {
		if u.freshShared {
			return fmt.Errorf("backup image lacks fresh shared reporting support; rebuild the matching image/helper bundle")
		}
		return fmt.Errorf("legacy image lacks notification cutover fencing")
	}
	if c.Policy.Notifications.Signal != nil {
		cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "backup-worker", "__notification-signal-migration-probe")
		cmd.ExtraFiles = []*os.File{helper}
		output, err := process.LimitedOutput(cmd, 15*time.Second, 1024)
		if err != nil || strings.TrimSpace(string(output)) != "backup-notification-signal-migration-v1" {
			return fmt.Errorf("backup helper lacks Signal notification migration support")
		}
	}
	return nil
}
func (u *backupCutoverManager) StopLegacy(ctx context.Context) error {
	if u.freshShared {
		return u.requireNoDirectSenders(ctx)
	}
	if err := cutoverSystemctl(ctx, "disable", "--now", "deploy-backup-notifier.timer", "deploy-backup-notifier.path"); err != nil {
		return err
	}
	services := []string{}
	for name := range u.legacy {
		if strings.HasSuffix(name, ".service") {
			services = append(services, name)
		}
	}
	slices.Sort(services)
	if err := cutoverSystemctl(ctx, append([]string{"stop"}, services...)...); err != nil {
		return err
	}
	for name, path := range u.legacy {
		if err := u.inspect(ctx, name, path, true, false); err != nil {
			return err
		}
	}
	// Completed systemd stop is insufficient if a detached old container survives.
	for _, service := range services {
		name := "systemd-" + strings.TrimSuffix(service, ".service")
		out, err := cutoverCommand(ctx, "podman", "ps", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "" {
			return fmt.Errorf("legacy sender container is still running")
		}
	}
	return nil
}
func (u *backupCutoverManager) InstallReports(ctx context.Context, units map[string]string) error {
	d, err := safefs.Open(u.unitDirectory())
	if err != nil {
		return err
	}
	defer d.Close()
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if sub, e := d.Sub(name + ".d"); e == nil {
			sub.Close()
			return fmt.Errorf("report unit has unowned drop-ins")
		} else if !os.IsNotExist(e) {
			return e
		}
		old, e := safefs.ReadFile(d, name, 1<<20)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil && string(old) != units[name] {
			return fmt.Errorf("report unit %s is foreign or changed", name)
		}
		if e = u.reportSlot(ctx, name); e != nil {
			return e
		}
		if e = safefs.ReplaceFile(d, name, old, []byte(units[name]), 0600); e != nil {
			return e
		}
	}
	return cutoverSystemctl(ctx, "daemon-reload")
}
func (u *backupCutoverManager) StartReports(ctx context.Context, units map[string]string) error {
	if err := u.InstallReports(ctx, units); err != nil {
		return err
	}
	if err := cutoverSystemctl(ctx, "enable", "--now", "deploy-backup-report.timer", "deploy-backup-report.path"); err != nil {
		return err
	}
	for _, name := range []string{"deploy-backup-report.timer", "deploy-backup-report.path"} {
		if err := u.inspect(ctx, name, filepath.Join(u.unitDirectory(), name), false, true); err != nil {
			return err
		}
	}
	if err := u.inspect(ctx, "deploy-backup-report.service", filepath.Join(u.unitDirectory(), "deploy-backup-report.service"), false, false); err != nil {
		return err
	}
	return cutoverSystemctl(ctx, "start", "deploy-backup-report.service")
}
func (u *backupCutoverManager) StopReports(ctx context.Context, units map[string]string) error {
	if err := u.InstallReports(ctx, units); err != nil {
		return err
	}
	if err := cutoverSystemctl(ctx, "disable", "--now", "deploy-backup-report.timer", "deploy-backup-report.path"); err != nil {
		return err
	}
	if err := cutoverSystemctl(ctx, "stop", "deploy-backup-report.service"); err != nil {
		return err
	}
	for name := range units {
		if err := u.inspect(ctx, name, filepath.Join(u.unitDirectory(), name), true, false); err != nil {
			return err
		}
	}
	return nil
}

// A free local pathname does not establish ownership of a unit name: an admin
// unit, alias or generated unit may already occupy it in the user manager.
func (u *backupCutoverManager) reportSlot(ctx context.Context, name string) error {
	run := u.run
	if run == nil {
		run = func(ctx context.Context, program string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, program, args...)
			var out, stderr process.Buffer
			out.Limit = 16384
			stderr.Limit = 4096
			cmd.Stdout = &out
			cmd.Stderr = &stderr
			err := process.Run(cmd, 15*time.Second, 2*time.Second)
			return out.Bytes(), err
		}
	}
	body, commandErr := run(ctx, "systemctl", "--user", "--no-pager", "show", name, "--property=LoadState,ActiveState,FragmentPath,DropInPaths,Names")
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("report unit ownership inspection failed")
		}
		if _, exists := values[k]; exists {
			return fmt.Errorf("duplicate report unit property")
		}
		values[k] = v
	}
	if len(values) != 5 || values["DropInPaths"] != "" || values["Names"] != "" && values["Names"] != name {
		return fmt.Errorf("report unit name has foreign drop-ins or aliases")
	}
	if values["LoadState"] == "not-found" && values["ActiveState"] == "inactive" && values["FragmentPath"] == "" {
		return nil
	}
	if commandErr != nil {
		return commandErr
	}
	if values["LoadState"] != "loaded" || values["FragmentPath"] != filepath.Join(u.unitDirectory(), name) {
		return fmt.Errorf("report unit name is owned by another fragment")
	}
	return nil
}

func (u *backupCutoverManager) unitDirectory() string {
	p := u.paths
	p.Home = u.home
	return p.unitDirectory()
}

func (u *backupCutoverManager) quadletDirectory() string {
	p := u.paths
	p.Home = u.home
	return p.quadletDirectory()
}
