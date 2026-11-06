package backup

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runBackupLegacyRollbackCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup-worker notification-rollback", flag.ContinueOnError)
	f.SetOutput(out)
	local := f.Bool("local", false, "Use this user's canonical installation")
	apply := f.Bool("apply", false, "Restore legacy delivery after shared revocation/reconciliation")
	revoke := f.Bool("revoke", false, "Stop and revoke restored legacy delivery; retain evidence")
	review := f.Bool("review", false, "Fill the current credential fingerprint in a rollback specification")
	specFile := f.String("spec", "", "Private reviewed rollback specification JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	modes := 0
	for _, mode := range []bool{*apply, *revoke, *review} {
		if mode {
			modes++
		}
	}
	if !*local || modes != 1 || f.NArg() != 0 || !backupRecoveryPath(*specFile) {
		return fmt.Errorf("use --local and exactly one of --review, --apply or --revoke with --spec ABS; restoration requires prior shared revocation")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] legacy rollback arguments validated; no state, service or provider accessed")
		return err
	}
	d, err := safefs.Open(filepath.Dir(*specFile))
	if err != nil {
		return err
	}
	defer d.Close()
	fh, err := d.Open(filepath.Base(*specFile), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("rollback spec must be a private regular file")
	}
	var spec backupLegacyRollbackSpec
	if err = safefs.ReadJSON(fh, &spec, safefs.MaxRecordBytes); err != nil {
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
		if notify.Hash(c) != spec.Cutover.ConfigHash {
			return fmt.Errorf("backup configuration changed")
		}
		spec.CredentialHash, err = notification.TelegramCredentialHash(c.Policy.Notifications)
		if err != nil {
			return err
		}
		if err = spec.validate(c); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(spec)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	deps := backupCutoverDependencies{Services: &backupCutoverManager{}, Managed: install.WithManagedProducer}
	if err = runBackupLegacyRollback(ctx, home, c, spec, *revoke, deps); err != nil {
		return err
	}
	status := "legacy delivery restored under a new canonical generation; shared generation remains revoked"
	if *revoke {
		status = "restored legacy generation revoked; all delivery remains fenced"
	}
	return json.NewEncoder(out).Encode(map[string]any{"version": 1, "generation": spec.Generation, "status": status})
}
