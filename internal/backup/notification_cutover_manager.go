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
	home   string
	legacy map[string]string
	run    func(context.Context, string, ...string) ([]byte, error)
}

func cutoverCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return process.LimitedOutput(exec.CommandContext(ctx, name, args...), 45*time.Second, 16384)
}
func cutoverSystemctl(ctx context.Context, args ...string) error {
	_, err := cutoverCommand(ctx, "systemctl", append([]string{"--user", "--no-pager"}, args...)...)
	return err
}
func (u *backupCutoverManager) inspect(ctx context.Context, name, source string, stopped, enabled bool) error {
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
	if len(values) != len(strings.Split(properties, ",")) || values["LoadState"] != "loaded" || values["DropInPaths"] != "" || values["Names"] != name || values["NeedDaemonReload"] != "no" {
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
	u.home = p.Home
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
	for _, name := range []string{"deploy-backup-notifier.container", "deploy-backup-test-telegram.container", "deploy-backup-notifier.timer", "deploy-backup-notifier.path"} {
		base := ".config/systemd/user/"
		if strings.HasSuffix(name, ".container") {
			base = ".config/containers/systemd/"
		}
		raw, err := safefs.ReadFile(root, base+name, 1<<20)
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
		source := filepath.Join(p.Home, base, name)
		if err = u.inspect(ctx, service, source, false, false); err != nil {
			return err
		}
		u.legacy[service] = source
	}
	helper, err := root.Open(".local/lib/deploy/backup-worker/deploy", os.O_RDONLY, 0)
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
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "backup-worker", "__notification-cutover-probe")
	cmd.ExtraFiles = []*os.File{helper}
	output, err := process.LimitedOutput(cmd, 15*time.Second, 1024)
	if err != nil || strings.TrimSpace(string(output)) != "backup-notification-cutover-v1" {
		return fmt.Errorf("installed backup helper lacks ownership fencing/report support")
	}
	output, err = cutoverCommand(ctx, "podman", "run", "--rm", "--pull=never", "--network=none", "--read-only", c.Image, "backup-worker", "__notification-cutover-probe")
	if err != nil || strings.TrimSpace(string(output)) != "backup-notification-cutover-v1" {
		return fmt.Errorf("legacy image lacks notification cutover fencing")
	}
	return nil
}
func (u *backupCutoverManager) StopLegacy(ctx context.Context) error {
	if err := cutoverSystemctl(ctx, "disable", "--now", "deploy-backup-notifier.timer", "deploy-backup-notifier.path"); err != nil {
		return err
	}
	if err := cutoverSystemctl(ctx, "stop", "deploy-backup-notifier.service", "deploy-backup-test-telegram.service"); err != nil {
		return err
	}
	for name, path := range u.legacy {
		if err := u.inspect(ctx, name, path, true, false); err != nil {
			return err
		}
	}
	// Completed systemd stop is insufficient if a detached old container survives.
	for _, name := range []string{"systemd-deploy-backup-notifier", "systemd-deploy-backup-test-telegram"} {
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
	d, err := safefs.Open(filepath.Join(u.home, ".config/systemd/user"))
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
		if err := u.inspect(ctx, name, filepath.Join(u.home, ".config/systemd/user", name), false, true); err != nil {
			return err
		}
	}
	if err := u.inspect(ctx, "deploy-backup-report.service", filepath.Join(u.home, ".config/systemd/user/deploy-backup-report.service"), false, false); err != nil {
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
		if err := u.inspect(ctx, name, filepath.Join(u.home, ".config/systemd/user", name), true, false); err != nil {
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
	if values["LoadState"] != "loaded" || values["FragmentPath"] != filepath.Join(u.home, ".config/systemd/user", name) {
		return fmt.Errorf("report unit name is owned by another fragment")
	}
	return nil
}
