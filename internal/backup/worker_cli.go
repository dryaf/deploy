package backup

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/inventory"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/ui"
)

func RunCLI(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: deploy backup-worker <setup|update|status|run|list|pull|recover|verify|prune|restore-check|qualify|activate|rotate-key|uninstall|delivery-review|notification-cutover|recovery-notifications|recovery-handoff|recovery-led-evidence|recovery-led-clear> [options] [worker]")
	}
	if args[0] == "__notification-cutover-probe" {
		if len(args) != 1 {
			return fmt.Errorf("cutover probe accepts no arguments")
		}
		fmt.Println("backup-notification-cutover-v1")
		return nil
	}
	if args[0] == "notification-cutover" {
		return runBackupCutoverCLI(args[1:], os.Stdout)
	}
	if args[0] == "delivery-review" {
		return runBackupDeliveryReviewCLI(args[1:], os.Stdout)
	}
	if args[0] == "recovery-led-evidence" || args[0] == "recovery-led-clear" {
		return runBackupLEDRecoveryCLI(args, os.Stdout)
	}
	if args[0] == "recovery-notifications" {
		return runBackupReconciliationCLI(args[1:], os.Stdout)
	}
	if args[0] == "recovery-handoff" {
		return runBackupRecoveryCoordinatorCLI(args[0], args[1:], os.Stdout)
	}
	if args[0] == "commands-manifest" || args[0] == "commands-init" || args[0] == "commands-step" {
		return runBackupCommandCLI(args)
	}
	if args[0] == "__recover_setup" {
		if len(args) != 1 {
			return fmt.Errorf("setup recovery accepts no arguments")
		}
		home, err := deploysource.TrustedSourceHome()
		if err != nil {
			return err
		}
		return recoverWorkerSetup(home)
	}
	if args[0] == "rotate-key" || args[0] == "uninstall" {
		return lifecycleWorkerCLI(args[0], args[1:])
	}
	if args[0] == "__lifecycle" {
		return runWorkerLifecycle()
	}
	if args[0] == "source-reconcile" {
		return deploysource.ReconcileSourceCLI(args[1:])
	}
	if args[0] == "qualify" {
		return qualifyWorkerCLI(args[1:])
	}
	if args[0] == "__qualify" {
		return internalQualification()
	}
	if args[0] == "setup" || args[0] == "update" {
		return setupBackupWorkerOperation(args[0], args[1:])
	}
	if args[0] == "__signal_relay" {
		return notification.RunSignalRelay()
	}
	if runtimeWorkerOperation(args[0]) {
		return runWorkerOperation(args)
	}
	operation := args[0]
	flags := flag.NewFlagSet("backup-worker "+operation, flag.ContinueOnError)
	local := flags.Bool("local", false, "Use an attached archive without inventory or SSH")
	archivePath := flags.String("archive", "", "Archive directory on the selected host")
	stateDir := flags.String("state-dir", "", "New separate recovery-state directory")
	jsonOutput := flags.Bool("json", false, "Versioned JSON output")
	source := flags.String("source", "", "Source alias or UUID")
	instance := flags.String("instance", "", "Service name")
	generation := flags.String("generation", "", "Database generation")
	limit := flags.Int("limit", 50, "Maximum list rows (1–500)")
	cursor := flags.String("cursor", "", "Continuation cursor")
	id := flags.String("backup", "", "Exact archive-wide backup ID")
	latest := flags.Bool("latest", false, "Select the latest backup of an explicit instance")
	output := flags.String("output", "", "New local destination directory")
	scan := flags.Bool("scan", false, "Read-only archive recovery scan")
	apply := flags.Bool("apply", false, "Create a separate recovery catalog")
	nonce := flags.String("nonce", "", "Internal archive export request nonce")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateWorkerFlags(flags, operation, map[string]string{"list": "local archive json source instance generation limit cursor", "pull": "local archive source instance generation backup latest output", "recover": "local archive json scan apply state-dir", "__export": "local archive source instance generation backup latest nonce"}); err != nil {
		return err
	}
	if operation != "list" && operation != "pull" && operation != "recover" && operation != "__export" {
		return fmt.Errorf("unknown backup-worker operation %q", operation)
	}
	if *local && flags.NArg() != 0 {
		return fmt.Errorf("--local cannot be combined with a worker name")
	}
	if !*local && flags.NArg() != 1 {
		return fmt.Errorf("select a worker name or use --local --archive <path>")
	}
	if operation == "recover" && *scan == *apply {
		return fmt.Errorf("recover requires exactly one of --scan and --apply")
	}
	if operation == "pull" || operation == "__export" {
		if (*id == "") == !*latest {
			return fmt.Errorf("select exactly one of --backup and --latest")
		}
		if *latest && (*source == "" || *instance == "") {
			return fmt.Errorf("--latest requires --source and --instance")
		}
	}
	filter := ArchiveFilter{*source, *instance, *generation}
	var server config.ServerConfig
	if !*local {
		selected, err := config.SelectServer(ui.InventoryPath(""), flags.Arg(0))
		if err != nil {
			return err
		}
		server = selected
		if *archivePath == "" && server.BackupWorker != nil {
			*archivePath = server.BackupWorker.Storage.Path
		}
	}
	if *archivePath == "" {
		return fmt.Errorf("archive path is required (worker storage.path or --archive)")
	}
	if operation == "recover" && *apply && *stateDir == "" {
		return fmt.Errorf("recover --apply requires a new --state-dir on writable storage")
	}
	if ui.DryRun {
		fmt.Printf("[DRY] %s archive %s; source connections, backup creation and notifications are not involved\n", operation, *archivePath)
		return nil
	}
	if !*local {
		if operation == "pull" {
			destination, err := pullRemoteArchive(server, *archivePath, *id, *latest, filter, *output)
			if err == nil {
				printDownloadedBackup(os.Stdout, destination)
			}
			return err
		}
		remoteArgs := []string{operation, "--local", "--archive", *archivePath}
		if *jsonOutput {
			remoteArgs = append(remoteArgs, "--json")
		}
		if *source != "" {
			remoteArgs = append(remoteArgs, "--source", *source)
		}
		if *instance != "" {
			remoteArgs = append(remoteArgs, "--instance", *instance)
		}
		if *generation != "" {
			remoteArgs = append(remoteArgs, "--generation", *generation)
		}
		if operation == "list" {
			remoteArgs = append(remoteArgs, "--limit", fmt.Sprint(*limit))
			if *cursor != "" {
				remoteArgs = append(remoteArgs, "--cursor", *cursor)
			}
		}
		if *scan {
			remoteArgs = append(remoteArgs, "--scan")
		}
		if *apply {
			remoteArgs = append(remoteArgs, "--apply", "--state-dir", *stateDir)
		}
		cmd := workerAdminCommand(server, remoteArgs)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return process.Run(cmd, 2*time.Minute, 10*time.Second)
	}
	archive, err := openBackupArchive(*archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	switch operation {
	case "list":
		list, err := archive.list(filter, *limit, *cursor)
		if err != nil {
			return err
		}
		if err = printArchiveListing(os.Stdout, list, *jsonOutput); err != nil {
			return err
		}
		if !list.Complete {
			return fmt.Errorf("archive listing is incomplete; see diagnostics")
		}
		return nil
	case "pull", "__export":
		receipt, err := archive.selectReceipt(*id, *latest, filter)
		if err != nil {
			return err
		}
		if operation == "__export" {
			return archive.export(receipt, *nonce, os.Stdout)
		}
		destination, err := archive.pull(receipt, *output, func(file *os.File) (protocol.SnapshotValidation, error) {
			return isolatedSnapshotValidation(file, false, 5*time.Minute)
		})
		if err == nil {
			printDownloadedBackup(os.Stdout, destination)
		}
		return err
	case "recover":
		list, err := archive.scan(100000)
		if err != nil {
			return err
		}
		if *apply {
			absoluteState, _ := filepath.Abs(*stateDir)
			absoluteArchive, _ := filepath.Abs(*archivePath)
			if config.PathsOverlap(absoluteState, absoluteArchive) {
				return fmt.Errorf("recovery state must be separate from the archive")
			}
			state, err := openBackupState(*stateDir, true)
			if err != nil {
				return err
			}
			defer state.Close()
			if err = state.put("mode", "recovery"); err != nil {
				return err
			}
			if control, err := archive.checkpoint(); err == nil {
				if err = state.put("recovered_control", control); err != nil {
					return err
				}
			} else {
				list.Complete = false
				list.Diagnostics = append(list.Diagnostics, "recovery control/pins unavailable; writes and retention remain disabled")
			}
			for _, receipt := range list.Entries {
				data, _ := json.Marshal(receipt)
				if _, err = state.db.Exec("INSERT INTO bundles(id,path,record) VALUES(?,?,?)", receipt.BackupID, receiptPath(receipt), string(data)); err != nil {
					return err
				}
			}
			if err = archive.syncVerificationStatuses(state); err != nil {
				return err
			}
			if err = state.put("recovery_report", list); err != nil {
				return err
			}
			if !*jsonOutput {
				absolute, _ := filepath.Abs(*stateDir)
				fmt.Printf("Recovery catalog: %s (read mode; stored validation is historical)\n", absolute)
			}
		}
		if err = printArchiveListing(os.Stdout, list, *jsonOutput); err != nil {
			return err
		}
		if !list.Complete {
			return fmt.Errorf("recovery completed with incomplete evidence; writes remain disabled")
		}
		return nil
	}
	return nil
}

func printArchiveListing(w io.Writer, list ArchiveListing, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(list)
	}
	writer := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "BACKUP ID\tSOURCE\tINSTANCE\tENV\tGENERATION\tREQUEST TIME (UTC)\tBYTES\tRECORDED VALIDATION\tLATER VERIFICATION")
	for _, r := range list.Entries {
		current := "not rechecked"
		if status, ok := list.Verification[r.BackupID]; ok {
			current = status.Last.Validation.Result
			if status.Last.ErrorKind != "" {
				current = "inconclusive: " + status.Last.ErrorKind
			}
			if status.Definitive != nil && status.Definitive.ErrorKind != "" {
				current = "FAILED: " + status.Definitive.ErrorKind
			}
			current += " at " + status.Last.FinishedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s %s\t%s\n", r.BackupID, inventory.DisplayInventoryText(r.SourceAlias), r.Header.Attempt.Service, inventory.DisplayInventoryText(r.Header.Metadata.Environment), r.Header.Generation, r.Header.Attempt.RequestedAt.UTC().Format(time.RFC3339), r.Header.Size, r.Validation.Check, r.Validation.Result, current)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	for _, d := range list.Diagnostics {
		fmt.Fprintln(w, "  "+inventory.DisplayInventoryText(d))
	}
	if list.Incoming > 0 || list.Trash > 0 {
		fmt.Fprintf(w, "Pending incoming: %d; managed trash: %d\n", list.Incoming, list.Trash)
	}
	if list.NextCursor != "" {
		fmt.Fprintf(w, "Next cursor: %s\n", list.NextCursor)
	}
	return nil
}

func printDownloadedBackup(w io.Writer, directory string) {
	fmt.Fprintf(w, "Downloaded and verified: %s\nReceipt: %s\nPoint an isolated local test application at database.db. Application startup has not been run.\n", filepath.Join(directory, "database.db"), filepath.Join(directory, "receipt.json"))
}
