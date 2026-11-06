package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func runBackupRemigrationCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup-worker notification-remigrate", flag.ContinueOnError)
	f.SetOutput(out)
	local := f.Bool("local", false, "Use this user's installed backup worker")
	review := f.Bool("review", false, "Read revoked legacy ownership and current delivery evidence")
	file := f.String("spec", "", "Private exact remigration specification")
	revoke := f.Bool("revoke", false, "Permanently revoke the remigrated shared generation")
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if *review {
		state, err := openBackupState(backupCutoverPaths(home, c).State, false)
		if err != nil {
			return err
		}
		defer state.Close()
		rollbackFile, err := currentMigrationArtifact(state, legacyRollbackFile)
		if err != nil {
			return err
		}
		check, release, err := lockRemigrationSources(ctx, state)
		if err != nil {
			return err
		}
		defer release()
		d, err := safefs.Open(state.directory)
		if err != nil {
			return err
		}
		defer d.Close()
		var rollback backupLegacyRollbackIntent
		if err = d.ReadJSON(rollbackFile, &rollback); err != nil {
			return err
		}
		var evidence backupCutoverEvidence
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			if err := check(); err != nil {
				return err
			}
			var revoked string
			if err := backupNotificationMeta(tx, "notification_legacy_rollback_revoked", &revoked); err != nil {
				return err
			}
			if revoked != notify.Hash(rollback) {
				return fmt.Errorf("legacy rollback is not durably revoked")
			}
			var err error
			evidence, err = readBackupCutoverEvidence(tx)
			return err
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"version": 1, "rollback_hash": notify.Hash(rollback), "config_hash": notify.Hash(c), "evidence_hash": notify.Hash(evidence), "evidence": evidence, "previous_generation": rollback.Owner.Binding.Generation, "legacy_generation": rollback.Spec.Generation})
	}
	d, err := safefs.Open(filepath.Dir(*file))
	if err != nil {
		return err
	}
	defer d.Close()
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
		return fmt.Errorf("remigration spec must be a private regular file")
	}
	var spec backupRemigrationSpec
	if err = safefs.ReadJSON(fh, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	deps := backupCutoverDependencies{Services: &backupCutoverManager{}, Managed: install.WithManagedProducer, Verify: verifyBackupCutoverGateway}
	if err = runBackupRemigration(ctx, home, c, spec, *revoke, deps); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"version": 1, "generation": spec.Cutover.Managed.Generation, "reporting_enabled": !*revoke})
}
