package backup

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	notifyinstall "github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/led"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type backupLEDRecoveryOptions struct {
	Config, State, NotificationConfig, NotificationState              string
	Inventory, InventoryHash, Binding, Fault, EvidenceHash, EpochHash string
}

type backupLEDCleanupOutput struct {
	Result     notify.RecoveryLEDClearResult `json:"result"`
	ResultHash string                        `json:"result_hash"`
	Import     *worker.RecoveryLEDImportView `json:"import,omitempty"`
}

func backupRecoveryPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}

func backupRecoveryFault(id string) bool {
	if !strings.HasPrefix(id, "deploy.f.") || len(id) != 57 {
		return false
	}
	raw, err := hex.DecodeString(id[9:])
	return err == nil && hex.EncodeToString(raw) == id[9:]
}

func runBackupLEDRecoveryCLI(args []string, out io.Writer) error {
	op := args[0]
	f := flag.NewFlagSet("backup-worker "+op, flag.ContinueOnError)
	local := f.Bool("local", false, "Operate on existing local backup and quarantined notification state")
	var o backupLEDRecoveryOptions
	f.StringVar(&o.Config, "config", "", "Private installed backup worker JSON")
	f.StringVar(&o.State, "state-dir", "", "Existing backup state directory")
	f.StringVar(&o.NotificationConfig, "notification-config", "", "Private notification runtime JSON")
	f.StringVar(&o.NotificationState, "notification-state", "", "Existing quarantined notification state directory")
	f.StringVar(&o.Inventory, "inventory", "", "Exact reviewed LED inventory ID")
	f.StringVar(&o.InventoryHash, "inventory-hash", "", "Exact reviewed LED inventory digest")
	f.StringVar(&o.Binding, "binding", "", "Exact backup LED binding")
	f.StringVar(&o.Fault, "fault", "", "Exact reviewed inherited fault ID")
	f.StringVar(&o.EvidenceHash, "evidence-hash", "", "Reviewed current backup domain evidence digest")
	f.StringVar(&o.EpochHash, "epoch-hash", "", "Committed LED history handoff digest")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || (op != "recovery-led-evidence" && op != "recovery-led-clear") {
		return fmt.Errorf("backup LED recovery requires an explicit local operation and no positional arguments")
	}
	for _, path := range []string{o.Config, o.State, o.NotificationConfig, o.NotificationState} {
		if !backupRecoveryPath(path) {
			return fmt.Errorf("backup LED recovery requires clean absolute configuration/state paths")
		}
	}
	if o.State == o.NotificationState || strings.HasPrefix(o.State, o.NotificationState+"/") || strings.HasPrefix(o.NotificationState, o.State+"/") {
		return fmt.Errorf("backup and notification state directories must be separate")
	}
	if !safefs.ValidID(o.Inventory, "n") || !safefs.DigestPattern.MatchString(o.InventoryHash) {
		return fmt.Errorf("exact reviewed --inventory and --inventory-hash are required")
	}
	if op == "recovery-led-evidence" {
		bad := false
		f.Visit(func(v *flag.Flag) {
			if v.Name == "binding" || v.Name == "fault" || v.Name == "evidence-hash" || v.Name == "epoch-hash" {
				bad = true
			}
		})
		if bad {
			return fmt.Errorf("evidence exchange does not accept cleanup options")
		}
	} else if !notify.ValidToken(o.Binding) || !backupRecoveryFault(o.Fault) || !safefs.DigestPattern.MatchString(o.EvidenceHash) || !safefs.DigestPattern.MatchString(o.EpochHash) {
		return fmt.Errorf("cleanup requires exact --binding, --fault, --evidence-hash and --epoch-hash")
	}
	// Use private-file checks and the common installation validator. No
	// credentials, installation files or state are created here.
	c, err := readBackupRecoveryInstallation(o.Config)
	if err != nil {
		return err
	}
	runtime, err := notifyinstall.ReadRuntime(o.NotificationConfig)
	if err != nil {
		return err
	}
	if !backupRecoveryPath(runtime.LEDSocket) {
		return fmt.Errorf("notification runtime requires a pinned LED socket")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] backup LED recovery arguments/configurations validated; no state opened, evidence renewed, daemon contacted or services changed")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runBackupLEDRecovery(ctx, op, o, c, runtime.Worker, runtime.LEDSocket, led.Client{Socket: runtime.LEDSocket, Budget: 2 * time.Second}, out)
}

func runBackupLEDRecovery(ctx context.Context, op string, o backupLEDRecoveryOptions, c WorkerInstallation, config worker.Config, socket string, api worker.LEDRecoveryCleaner, out io.Writer) error {
	return runBackupLEDRecoveryWith(ctx, op, o, c, config, socket, api, out, nil)
}

func runBackupLEDRecoveryWith(ctx context.Context, op string, o backupLEDRecoveryOptions, c WorkerInstallation, config worker.Config, socket string, api worker.LEDRecoveryCleaner, out io.Writer, fault func(string) error) error {
	boundary := func(at string) error {
		if fault != nil {
			return fault(at)
		}
		return nil
	}
	state, err := openBackupState(o.State, false)
	if err != nil {
		return err
	}
	defer state.Close()
	// The original recovery binding must already be established by the producer
	// reconciliation coordinator. This command never creates or bypasses it.
	var producer notify.Producer
	err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		j, _, e := backupLEDRecoveryAuthority(tx, c)
		if e != nil {
			return e
		}
		producer = j.Binding.Producer
		return nil
	})
	if err != nil {
		return err
	}
	found := false
	for _, p := range config.Producers {
		if p.Producer == producer {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("backup producer is not enrolled in the notification configuration")
	}
	if op == "recovery-led-clear" {
		found = false
		for _, b := range config.LEDBindings {
			if b.ID == o.Binding && b.Producer == producer {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("cleanup binding does not belong to this backup producer")
		}
	}
	review, err := worker.ReviewRecoveryLED(ctx, o.NotificationState, config, socket, nil, false)
	if err != nil {
		return err
	}
	if review.InventoryID != o.Inventory || review.InventoryHash != o.InventoryHash || !review.SourcesMatch || !review.MappingsMatch || !review.RuntimeUIDMatches {
		return fmt.Errorf("reviewed backup LED inventory changed")
	}
	if op == "recovery-led-evidence" {
		session, err := worker.OpenRecoveryLEDEvidenceSession(ctx, o.NotificationState, config)
		if err != nil {
			return err
		}
		defer session.Close()
		evidence, err := reconcileBackupLEDRecovery(ctx, state, c, session, o.Inventory)
		if err != nil {
			return err
		}
		if err = boundary("backup_led_evidence_result"); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(evidence)
	}
	if review.EpochHandoff == nil || review.EpochHandoff.State != "committed" || review.EpochHandoff.RecordHash != o.EpochHash {
		return fmt.Errorf("cleanup requires the reviewed committed LED epoch handoff")
	}
	session, err := worker.OpenRecoveryLEDCleanupSession(ctx, o.NotificationState, config, socket, api)
	if err != nil {
		return err
	}
	result, clearErr := clearBackupRecoveryLEDFault(ctx, state, c, session, o.Inventory, o.Binding, o.Fault, o.EvidenceHash)
	closeErr := session.Close()
	response := backupLEDCleanupOutput{Result: result}
	if result.FaultID != "" {
		response.ResultHash = notify.Hash(result)
	}
	err = errors.Join(clearErr, closeErr)
	if err == nil {
		err = boundary("backup_led_cleanup_result")
	}
	if err == nil && (result.Outcome == "acknowledged" || result.Outcome == "observed_absent") {
		imported, e := worker.ImportRecoveryLEDCleanup(ctx, o.NotificationState, config, socket, o.EpochHash, response.ResultHash)
		if e == nil {
			response.Import = &imported
		}
		err = e
		if err == nil {
			err = boundary("backup_led_cleanup_import")
		}
	}
	if result.FaultID != "" {
		err = errors.Join(err, json.NewEncoder(out).Encode(response))
	}
	return err
}
