package backup

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runBackupMigrationCycleCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup-worker notification-cycle", flag.ContinueOnError)
	f.SetOutput(out)
	local := f.Bool("local", false, "Use this user's canonical installation")
	review := f.Bool("review", false, "Read current notification owner and installation fingerprints")
	file := f.String("spec", "", "Private pinned migration-cycle specification")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || *review && *file != "" || !*review && !backupRecoveryPath(*file) {
		return fmt.Errorf("use --local --review or --local --spec ABS")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] migration cycle arguments validated; no state, service or provider accessed")
		return err
	}
	home, err := deploysource.TrustedSourceHome()
	if err != nil {
		return err
	}
	c, err := loadInstalledWorker(home)
	if err != nil {
		return err
	}
	if *review {
		state, err := openBackupState(backupCutoverPaths(home, c).State, false)
		if err != nil {
			return err
		}
		defer state.Close()
		owner, err := currentSharedBackupOwner(state)
		if err != nil {
			return err
		}
		if owner == nil {
			return fmt.Errorf("no shared notification owner")
		}
		return json.NewEncoder(out).Encode(map[string]any{"version": 1, "owner": owner, "owner_hash": notify.Hash(*owner), "config_hash": notify.Hash(c)})
	}
	d, err := safefs.Open(filepath.Dir(*file))
	if err != nil {
		return err
	}
	defer d.Close()
	input, err := d.Open(filepath.Base(*file), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("cycle specification must be a private regular file")
	}
	var spec backupMigrationCycleSpec
	if err = safefs.ReadJSON(input, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	record, err := runBackupMigrationCycle(ctx, home, c, spec, backupCutoverDependencies{Services: &backupCutoverManager{}, Managed: install.WithManagedProducer, Verify: verifyBackupCutoverGateway})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"version": 1, "cycle_hash": notify.Hash(record), "status": "shared generation revoked; prior cycle retained; legacy delivery remains fenced"})
}
