package backup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"database/sql"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runBackupRecoveryActivationCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup-worker notification-recovery-activate", flag.ContinueOnError)
	f.SetOutput(out)
	local := f.Bool("local", false, "Use this user's installed backup worker")
	review := f.Bool("review", false, "Print the retained source commit and recovered producer binding")
	file := f.String("spec", "", "Private specification with managed installation and source commit pins")
	revoke := f.Bool("revoke", false, "Permanently revoke this recovered report generation")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || *review && (*file != "" || *revoke) || !*review && !backupRecoveryPath(*file) {
		return fmt.Errorf("use --local --review or --local --spec ABS [--revoke]")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] arguments validated; no installation, services or provider accessed")
		return err
	}
	home, err := deploysource.TrustedSourceHome()
	if err != nil {
		return err
	}
	c, err := loadWorkerInstallation(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"))
	if err != nil {
		return err
	}
	if *review {
		state, err := openBackupState(backupCutoverPaths(home, c).State, false)
		if err != nil {
			return err
		}
		defer state.Close()
		var record backupRecoveryCommitRecord
		err = backupTransferTx(context.Background(), state, func(tx *sql.Tx) error {
			j, e := readBackupRecoveryAuthority(tx, c)
			if e != nil {
				return e
			}
			if e = backupNotificationMeta(tx, "notification_recovery_commit", &record); e != nil {
				return e
			}
			return record.validate(j)
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"version": 1, "config_hash": notify.Hash(c), "commit_hash": notify.Hash(record), "delivery": record})
	}
	d, err := safefs.Open(filepath.Dir(*file))
	if err != nil {
		return err
	}
	defer d.Close()
	var spec backupRecoveryActivationSpec
	fh, err := d.Open(filepath.Base(*file), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("recovery activation spec must be a private regular file")
	}
	if err = safefs.ReadJSON(fh, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	deps := backupCutoverDependencies{Services: &backupCutoverManager{}, Managed: install.WithManagedProducer, Verify: verifyBackupCutoverGateway}
	if err = runBackupRecoveryActivation(ctx, home, c, spec, *revoke, deps); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"version": 1, "generation": spec.Managed.Generation, "reporting_enabled": !*revoke})
}

func (u *backupCutoverManager) PrepareRecovery(ctx context.Context, p WorkerUnitPaths, hash string) error {
	return u.prepareNotificationCapability(ctx, p, hash, "__notification-recovery-activation-probe", "backup-notification-recovery-activation-v1")
}
func (u *backupCutoverManager) PrepareNextRecovery(ctx context.Context, p WorkerUnitPaths, hash string) error {
	return u.prepareNotificationCapability(ctx, p, hash, "__notification-next-recovery-probe", "backup-notification-next-recovery-v1")
}
func (u *backupCutoverManager) PrepareRemigration(ctx context.Context, p WorkerUnitPaths, hash string) error {
	return u.prepareNotificationCapability(ctx, p, hash, "__notification-remigration-probe", "backup-notification-remigration-v1")
}
func (u *backupCutoverManager) PrepareMigrationCycle(ctx context.Context, p WorkerUnitPaths, hash string) error {
	return u.prepareNotificationCapability(ctx, p, hash, "__notification-cycle-probe", "backup-notification-cycle-v1")
}
func (u *backupCutoverManager) prepareNotificationCapability(ctx context.Context, p WorkerUnitPaths, hash, probe, response string) error {
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
	if !info.Mode().IsRegular() || info.Size() > 256<<20 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("unsafe backup helper")
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, (256<<20)+1)); err != nil {
		return err
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != hash {
		return fmt.Errorf("recovered report helper hash changed")
	}
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "backup-worker", probe)
	cmd.ExtraFiles = []*os.File{f}
	out, err := process.LimitedOutput(cmd, 15*time.Second, 1024)
	if err != nil || strings.TrimSpace(string(out)) != response {
		return fmt.Errorf("backup helper lacks the requested notification lifecycle capability")
	}
	return nil
}
