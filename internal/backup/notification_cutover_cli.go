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
	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runBackupCutoverCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup-worker notification-cutover", flag.ContinueOnError)
	f.SetOutput(out)
	local := f.Bool("local", false, "Use the canonical installation of this user")
	review := f.Bool("review", false, "Print legacy evidence and conservative held decisions")
	apply := f.Bool("apply", false, "Apply or resume an exact reviewed cutover")
	revoke := f.Bool("revoke", false, "Drain/revoke shared generation; retain legacy fence")
	specFile := f.String("spec", "", "Private cutover specification JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	modes := 0
	for _, v := range []bool{*review, *apply, *revoke} {
		if v {
			modes++
		}
	}
	if !*local || f.NArg() != 0 || modes != 1 || *review && *specFile != "" || !*review && !backupRecoveryPath(*specFile) {
		return fmt.Errorf("use --local and exactly one of --review, --apply --spec ABS, or --revoke --spec ABS")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] cutover arguments validated; no installation, manager, state or provider accessed")
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
		s, err := openBackupState(backupCutoverPaths(home, c).State, false)
		if err != nil {
			return err
		}
		defer s.Close()
		var evidence backupCutoverEvidence
		if err = backupTransferTx(ctx, s, func(tx *sql.Tx) error { var e error; evidence, e = readBackupCutoverEvidence(tx); return e }); err != nil {
			return err
		}
		decisions := map[string]backupCutoverDecision{}
		for _, r := range evidence.Rows {
			disposition := "held"
			if r.Accepted.Valid {
				disposition = "accepted"
			}
			decisions[r.ID] = backupCutoverDecision{notify.Hash(r), disposition, "Review legacy history; never infer unsent from zero attempts"}
		}
		return json.NewEncoder(out).Encode(struct {
			ConfigHash   string                           `json:"config_hash"`
			EvidenceHash string                           `json:"evidence_hash"`
			Evidence     backupCutoverEvidence            `json:"evidence"`
			Decisions    map[string]backupCutoverDecision `json:"decisions"`
		}{notify.Hash(c), notify.Hash(evidence), evidence, decisions})
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
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("cutover spec must be a private regular file")
	}
	var spec backupCutoverSpec
	if err = safefs.ReadJSON(file, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	deps := backupCutoverDependencies{Services: &backupCutoverManager{}, Managed: install.WithManagedProducer, Verify: verifyBackupCutoverGateway}
	if err = runBackupCutover(ctx, home, c, spec, *revoke, deps); err != nil {
		return err
	}
	status := "shared report service active"
	if *revoke {
		status = "shared generation revoked; legacy remains fenced pending outcome reconciliation"
	}
	return json.NewEncoder(out).Encode(map[string]any{"version": 1, "status": status, "generation": spec.Managed.Generation})
}
func verifyBackupCutoverGateway(ctx context.Context, owner backupNotificationOwner) error {
	var status notify.OwnershipStatus
	api := client.Client{Socket: owner.Socket, Identity: owner.Binding.Identity}
	identity, err := api.Call(ctx, notify.Request{Operation: "producer.ownership", Producer: owner.Binding.Producer, Generation: owner.Binding.Generation}, &status)
	if err != nil {
		return err
	}
	if identity != owner.Binding.Identity || status.Producer != owner.Binding.Producer || status.Generation != owner.Binding.Generation || !status.Active || status.State != "active" {
		return fmt.Errorf("shared gateway did not confirm exact active generation")
	}
	return nil
}
