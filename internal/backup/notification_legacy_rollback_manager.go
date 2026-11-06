package backup

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func renderBackupLegacyUnits(p WorkerUnitPaths, c WorkerInstallation) (map[string]string, error) {
	policy := c.Policy
	if policy.Notifications.Telegram != nil {
		if err := requireBackupLegacyReadablePaths(p, policy.Notifications.Telegram.TokenFile, policy.Notifications.Telegram.ChatIDFile); err != nil {
			return nil, err
		}
	}
	// The legacy provider adapter needs its enrolled messenger credentials.
	// Archive, source keys and Signal account storage remain inaccessible.
	policy.Notifications = config.WorkerNotifications{}
	units, err := renderBackupReportUnits(p, policy, nil)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for name, body := range units {
		body = strings.ReplaceAll(body, "deploy-backup-report", "deploy-backup-legacy")
		body = strings.ReplaceAll(body, "backup-worker __report", "backup-worker __notify")
		body = strings.ReplaceAll(body, "Evaluate and export backup reports", "Deliver backup notifications under canonical legacy ownership")
		body = strings.ReplaceAll(body, "Periodic backup report evaluation", "Periodic legacy backup notification delivery")
		body = strings.ReplaceAll(body, "Wake backup report exporter for committed events", "Wake legacy backup notifier for committed events")
		result[strings.ReplaceAll(name, "deploy-backup-report", "deploy-backup-legacy")] = body
	}
	return result, nil
}

func requireBackupLegacyReadablePaths(p WorkerUnitPaths, required ...string) error {
	for _, denied := range []string{p.Keys, p.RelayState, p.Archive} {
		if denied == "" {
			continue
		}
		for _, path := range required {
			if path != "" && (denied == "/" || path == denied || strings.HasPrefix(path, denied+"/")) {
				return fmt.Errorf("native legacy credentials or canonical state overlap an inaccessible data root")
			}
		}
	}
	return nil
}

func (u *backupCutoverManager) PrepareLegacy(ctx context.Context, c WorkerInstallation, p WorkerUnitPaths, hash string) error {
	d, err := safefs.Open(p.Home)
	if err != nil {
		return err
	}
	defer d.Close()
	f, err := d.Open(".local/lib/deploy/backup-worker/deploy", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	digest := sha256.New()
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 256<<20 {
		return fmt.Errorf("unsafe legacy helper")
	}
	if _, err = io.Copy(digest, io.LimitReader(f, (256<<20)+1)); err != nil {
		return err
	}
	if fmt.Sprintf("%x", digest.Sum(nil)) != hash {
		return fmt.Errorf("legacy helper changed")
	}
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "backup-worker", "__notification-legacy-probe")
	cmd.ExtraFiles = []*os.File{f}
	out, err := process.LimitedOutput(cmd, 15*time.Second, 1024)
	if err != nil || strings.TrimSpace(string(out)) != "backup-notification-legacy-v1" {
		return fmt.Errorf("native helper lacks canonical legacy generation support")
	}
	units, err := renderBackupLegacyUnits(p, c)
	if err != nil {
		return err
	}
	u.legacyNative = units
	if err = u.InstallReports(ctx, units); err != nil {
		return err
	}
	if err = cutoverSystemctl(ctx, "disable", "--now", "deploy-backup-legacy.timer", "deploy-backup-legacy.path"); err != nil {
		return err
	}
	if err = cutoverSystemctl(ctx, "stop", "deploy-backup-legacy.service"); err != nil {
		return err
	}
	for name := range units {
		if err = u.inspect(ctx, name, filepath.Join(u.home, ".config/systemd/user", name), true, false); err != nil {
			return err
		}
	}
	return nil
}

func (u *backupCutoverManager) StartLegacy(ctx context.Context) error {
	if len(u.legacyNative) != 3 {
		return fmt.Errorf("legacy service was not prepared")
	}
	if err := u.InstallReports(ctx, u.legacyNative); err != nil {
		return err
	}
	if err := u.inspect(ctx, "deploy-backup-legacy.service", filepath.Join(u.home, ".config/systemd/user/deploy-backup-legacy.service"), false, false); err != nil {
		return err
	}
	if err := cutoverSystemctl(ctx, "enable", "--now", "deploy-backup-legacy.timer", "deploy-backup-legacy.path"); err != nil {
		return err
	}
	for _, name := range []string{"deploy-backup-legacy.timer", "deploy-backup-legacy.path"} {
		if err := u.inspect(ctx, name, filepath.Join(u.home, ".config/systemd/user", name), false, true); err != nil {
			return err
		}
	}
	return cutoverSystemctl(ctx, "start", "deploy-backup-legacy.service")
}
