package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	deploysource "github.com/dryaf/deploy/internal/backup/source"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runBackupLegacyResolutionCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup-worker notification-resolve", flag.ContinueOnError)
	f.SetOutput(out)
	local := f.Bool("local", false, "Use this user's canonical backup installation")
	review := f.Bool("review", false, "Review one held legacy source without releasing it")
	apply := f.Bool("apply", false, "Apply or resume an exact reviewed resolution")
	source := f.String("source", "", "Original held source event ID")
	specFile := f.String("spec", "", "Private resolution specification JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || *review == *apply ||
		*review && (!notify.ValidToken(*source) || *specFile != "") ||
		*apply && (*source != "" || !backupRecoveryPath(*specFile)) {
		return fmt.Errorf("use --local with --review --source ID or --apply --spec ABS")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] resolution arguments validated; no installation, state or provider accessed")
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
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	if err != nil {
		return err
	}
	defer state.Close()
	if *review {
		lease, err := backupNotifierLease(state)
		if err != nil {
			return err
		}
		defer lease.Close()
		var r backupLegacyReview
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			var err error
			r, err = readBackupLegacyReview(tx, c, *source, time.Now().UTC())
			return err
		})
		if err != nil {
			return err
		}
		if err = requireBackupCutoverOwner(state, r.Owner); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(backupLegacyResolution{Version: 1, Review: r, ReviewHash: notify.Hash(r), Action: "held", Reason: "Choose unsent with independent evidence, or retry with duplicate-risk acknowledgement"})
	}
	d, err := safefs.Open(filepath.Dir(*specFile))
	if err != nil {
		return err
	}
	defer d.Close()
	file, err := d.Open(filepath.Base(*specFile), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("legacy resolution spec must be a private regular file")
	}
	var spec backupLegacyResolution
	if err = safefs.ReadJSON(file, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	if err = resolveBackupLegacy(ctx, state, c, spec, time.Now().UTC(), nil); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"version": 1, "source": spec.Review.Original.ID, "decision_hash": notify.Hash(spec), "status": "resolution recorded; delivery and coverage still require eligible canonical acceptance"})
}
