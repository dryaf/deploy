package backup

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runtimeWorkerOperation(operation string) bool {
	switch operation {
	case "__report", "pin", "unpin", "ack-clock", "__maintenance", "accept-generation", "reactivate", "reconcile", "activate", "signal-link", "run", "status", "logs", "pause", "resume", "notify-test", "__notify_test", "__run", "__notify", "__health", "retire", "restore-check", "verify", "prune":
		return true
	}
	return false
}

func runWorkerOperation(args []string) error {
	operation := args[0]
	flags := flag.NewFlagSet("backup-worker "+operation, flag.ContinueOnError)
	local := flags.Bool("local", false, "Execute on the enrolled worker host")
	configFile := flags.String("config", "", "Installed runtime configuration")
	stateDir := flags.String("state-dir", "", "Worker state directory")
	archivePath := flags.String("archive", "", "Archive path")
	jsonOutput := flags.Bool("json", false, "Versioned JSON output")
	channel := flags.String("channel", "", "Configured notification channel")
	until := flags.String("until", "", "Pause expiration as RFC3339")
	pinLabel := flags.String("pin-label", "", "Exact manual or verification pin label")
	generation := flags.String("generation", "", "Explicit reconciled generation")
	source := flags.String("source", "", "Source alias or UUID")
	instance := flags.String("instance", "", "Service name")
	reason := flags.String("reason", "", "Required retirement reason")
	latest := flags.Bool("latest", false, "Select the latest snapshot for an explicit source and instance")
	backup := flags.String("backup", "", "Explicit archive-wide backup ID")
	apply := flags.Bool("apply", false, "Apply the configured retention policy")
	preview := flags.Bool("list", false, "Preview retention candidates")
	full := flags.Bool("full", false, "Run full SQLite integrity checking")
	linkOperation := flags.String("operation", "", "Exact Signal linking operation ID for retry or a deliberately new attempt")
	newLink := flags.Bool("new-link", false, "Start a new Signal link after completed maintenance; default resumes/reports the last operation")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	common := "local config state-dir "
	allowed := map[string]string{"pin": common + "archive backup reason pin-label", "unpin": common + "archive backup reason pin-label", "run": "local", "logs": "local", "status": common + "json", "pause": common + "until", "resume": common, "notify-test": common + "channel", "__notify_test": common + "channel", "__notify": common, "__report": common, "__health": common + "archive", "__run": common + "archive", "__maintenance": common + "archive", "activate": common, "signal-link": common + "operation new-link", "retire": common + "archive source instance reason", "reactivate": common + "archive source instance reason", "accept-generation": common + "archive source instance generation reason", "reconcile": common + "archive", "ack-clock": common + "reason", "verify": common + "archive backup full", "prune": common + "archive source instance apply list", "restore-check": common + "archive source instance backup latest"}
	if err := validateWorkerFlags(flags, operation, allowed); err != nil {
		return err
	}
	if *local && flags.NArg() != 0 {
		return fmt.Errorf("--local excludes a worker name")
	}
	if !*local && flags.NArg() != 1 {
		return fmt.Errorf("select a worker or use --local")
	}
	if operation == "pause" {
		expires, err := time.Parse(time.RFC3339, *until)
		if err != nil || !expires.After(time.Now()) || time.Until(expires) > 7*24*time.Hour {
			return fmt.Errorf("pause requires a future --until within seven days")
		}
	}
	if operation == "retire" && (*source == "" || strings.TrimSpace(*reason) == "") {
		return fmt.Errorf("retire requires --source and --reason; use --instance to retire one database")
	}
	if operation == "prune" && (*apply == *preview || *apply && (*source == "" || *instance == "")) {
		return fmt.Errorf("prune requires --list or --apply --source <source> --instance <service>")
	}
	if operation == "verify" && *backup == "" {
		return fmt.Errorf("verify requires --backup <id>")
	}
	if operation == "signal-link" && ((*newLink && *linkOperation != "") || (*linkOperation != "" && !safefs.ValidID(*linkOperation, "n"))) {
		return fmt.Errorf("Signal linking requires a valid --operation or --new-link, never both")
	}
	if ui.DryRun {
		fmt.Printf("[DRY] backup-worker %s; runtime host checks remain unknown\n", operation)
		return nil
	}
	if !*local {
		server, err := config.SelectServer(ui.InventoryPath(""), flags.Arg(0))
		if err != nil {
			return err
		}
		remote := []string{operation, "--local"}
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "local" {
				return
			}
			remote = append(remote, "--"+f.Name+"="+f.Value.String())
		})
		cmd := workerAdminCommand(server, remote)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return process.Run(cmd, 25*time.Minute, 10*time.Second)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if *configFile == "" || *stateDir == "" {
		paths, err := lookupInstalledBackupPaths(home)
		if err != nil {
			return err
		}
		if *configFile == "" {
			*configFile = paths.Runtime
		}
		if *stateDir == "" {
			*stateDir = paths.State
		}
	}
	if operation == "signal-link" {
		lease, e := signalInstallationLease(home)
		if e != nil {
			return e
		}
		defer lease.Close()
	}
	c, err := loadWorkerInstallation(*configFile)
	if err != nil {
		return fmt.Errorf("worker installation unavailable: %w", err)
	}
	if *archivePath == "" {
		*archivePath = c.Policy.Storage.Path
	}
	if operation == "run" {
		state, err := openBackupState(*stateDir, false)
		if err != nil {
			return err
		}
		err = requireBackupActivityReady(state)
		state.Close()
		if err != nil {
			return err
		}
		cmd := exec.Command("systemctl", "--user", "start", "--no-block", "deploy-backup.service")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return process.Run(cmd, 10*time.Second, time.Second)
	}
	if operation == "logs" {
		cmd := exec.Command("journalctl", "--user", "--no-pager", "-n", "200", "-u", "deploy-backup.service", "-u", "deploy-backup-notifier.service", "-u", "deploy-backup-health.service", "-u", "deploy-backup-maintenance.service")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return process.Run(cmd, 30*time.Second, time.Second)
	}
	state, err := openBackupState(*stateDir, false)
	if err != nil {
		if operation == "__health" && notification.ValidateMonitoringChecks(c.Policy.Monitoring) == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
			defer cancel()
			_ = notification.Heartbeat(ctx, c.Policy.Monitoring.BackupPingURLFile, true)
			_ = notification.Heartbeat(ctx, c.Policy.Monitoring.DailyDeliveryPingURLFile, true)
		}
		return err
	}
	defer state.Close()
	if operation != "status" {
		if err = requireBackupActivityReady(state); err != nil {
			return err
		}
	}
	c = applyWorkerQualification(c, state)
	switch operation {
	case "activate":
		return activateWorker(c, state)
	case "signal-link":
		lease, e := backupNotifierLease(state)
		if e != nil {
			return e
		}
		defer lease.Close()
		if err = requireLegacyNotificationOwner(state); err != nil {
			return err
		}
		return signalLinkWorker(home, *configFile, *stateDir, *linkOperation, *newLink)
	case "ack-clock":
		if strings.TrimSpace(*reason) == "" {
			return fmt.Errorf("ack-clock requires --reason after checking the host clock")
		}
		if err = state.put("clock_suspect", false); err != nil {
			return err
		}
		return state.resolveIncident("clock", "worker", time.Now().UTC(), c.Policy.Notifications)
	case "__health":
		probe := collectWorkerHealth(c, *archivePath, state)
		var previous WorkerHealthProbe
		_ = state.get("health_probe", &previous)
		var suspect bool
		_ = state.get("clock_suspect", &suspect)
		if previous.BootID == probe.BootID && !previous.ObservedAt.IsZero() && absFloat(probe.ObservedAt.Sub(previous.ObservedAt).Seconds()-(probe.Uptime-previous.Uptime)) > 30 {
			suspect = true
			if err = state.put("clock_suspect", true); err != nil {
				return err
			}
		}
		if suspect {
			probe.Healthy = false
			probe.BackupHealthy = false
			probe.Diagnostics = append(probe.Diagnostics, "host clock changed; reconcile time and use ack-clock --reason")
			if err = state.observeIncident("clock", "worker", "UTC and monotonic observations diverged", probe.ObservedAt, c.Policy.Notifications); err != nil {
				return err
			}
		}

		if catchupErr := consumeBackupCatchup(state, c, false, startBackupCatchup); catchupErr != nil {
			probe.Healthy = false
			probe.BackupHealthy = false
			probe.Diagnostics = append(probe.Diagnostics, "catch-up pending: "+catchupErr.Error())
		}
		shared, err := recordSharedBackupHealthProbe(state, c, probe)
		if err != nil {
			return err
		}
		if shared {
			return state.wakeNotifier()
		}
		if err = state.put("health_probe", probe); err != nil {
			return err
		}
		if !probe.Healthy {
			return state.observeIncident("host-health", "worker", strings.Join(probe.Diagnostics, "; "), probe.ObservedAt, c.Policy.Notifications)
		}
		return state.resolveIncident("host-health", "worker", probe.ObservedAt, c.Policy.Notifications)
	case "__report":
		return runSharedBackupReport(context.Background(), c, state)
	case "__notify":
		return runNotifier(context.Background(), c, state)
	case "status":
		report := struct {
			Schema       int                `json:"schema"`
			Installation WorkerInstallation `json:"installation"`
			Run          WorkerRun          `json:"run"`
			Control      ArchiveControl     `json:"control"`
			Host         WorkerHealthProbe  `json:"host"`
			Pending      int                `json:"pending_notifications"`
			Delivery     deliveryOutage     `json:"delivery"`
			Incidents    int                `json:"open_incidents"`
			PausedUntil  time.Time          `json:"paused_until"`
		}{Schema: 1, Installation: c}
		if err = state.get("control", &report.Control); err != nil {
			return err
		}
		_ = state.get("run", &report.Run)
		_ = state.get("health_probe", &report.Host)
		_ = state.get("paused_until", &report.PausedUntil)
		_ = state.get("delivery_outage", &report.Delivery)
		if err = state.db.QueryRow("SELECT count(*) FROM outbox WHERE accepted IS NULL AND covered_by IS NULL").Scan(&report.Pending); err != nil {
			return err
		}
		if err = state.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL").Scan(&report.Incidents); err != nil {
			return err
		}
		if *jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(report)
		}
		fmt.Printf("Worker %s; archive %s; revision %d\n", c.WorkerID, c.ArchiveID, report.Control.Revision)
		fmt.Printf("Latest run complete: %t; captures: %d; open incidents: %d; pending notifications: %d\n", report.Run.Complete, report.Run.Captured, report.Incidents, report.Pending)
		fmt.Printf("Fresh host evidence: %t; host checks healthy: %t\n", report.Host.Fresh(time.Now()), report.Host.Healthy)
		for _, name := range c.Policy.Sources {
			fmt.Println("Source: " + name)
		}
		for _, gate := range workerQualificationGates(c, false) {
			if !c.Qualification[gate] {
				fmt.Println("Qualification pending: " + gate)
			}
		}
		return nil
	case "pause":
		expires, _ := time.Parse(time.RFC3339, *until)
		if err = setWorkerPause(state, expires.UTC()); err != nil {
			return err
		}
		return state.observeIncident("maintenance", "worker", "backups intentionally paused until "+expires.Format(time.RFC3339), time.Now().UTC(), c.Policy.Notifications)
	case "resume":
		return consumeBackupCatchup(state, c, true, startBackupCatchup)
	case "notify-test":
		if err = requireBackupReporting(state, c); err != nil {
			return err
		}
		if err = requireLegacyNotificationOwner(state); err != nil {
			return err
		}
		if *channel == "" {
			*channel = c.Policy.Notifications.Primary
		}
		if *channel != c.Policy.Notifications.Primary && *channel != c.Policy.Notifications.Fallback {
			return fmt.Errorf("channel is not configured")
		}
		cmd := exec.Command("systemctl", "--user", "start", "deploy-backup-test-"+*channel+".service")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return process.Run(cmd, 40*time.Second, 5*time.Second)
	case "__notify_test":
		if err = requireBackupReporting(state, c); err != nil {
			return err
		}
		lease, err := backupNotifierLease(state)
		if err != nil {
			return err
		}
		defer lease.Close()
		if err = requireLegacyNotificationOwner(state); err != nil {
			return err
		}
		if *channel == "" {
			*channel = c.Policy.Notifications.Primary
		}
		if *channel != c.Policy.Notifications.Primary && *channel != c.Policy.Notifications.Fallback {
			return fmt.Errorf("channel is not configured in delivery policy")
		}
		id, err := safefs.NewID("n")
		if err != nil {
			return err
		}
		event := notification.NotificationEvent{ID: id, Kind: "test", Message: "deploy backup-worker delivery test " + id}
		attempt, err := notification.Send(context.Background(), *channel, c.Policy.Notifications, event)
		_ = state.put("notification_test/"+*channel, attempt)
		if err != nil {
			return err
		}
		fmt.Println("Provider accepted test " + id + "; confirm receipt at the configured destination before qualification.")
		return nil
	}
	archive, err := openBackupArchive(*archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	if operation == "verify" || operation == "restore-check" || operation == "prune" && *apply {
		writer, e := archive.dir.Lock(".locks/writer.lock", true, false)
		if e != nil {
			return e
		}
		defer writer.Close()
		required := 25 * time.Minute
		if operation == "restore-check" {
			required = 25 * time.Minute
		}
		if e = maintenanceAdmission(c, state, required, time.Now()); e != nil {
			return e
		}
	}
	switch operation {
	case "pin", "unpin":
		if !safefs.ValidID(*backup, "b") || strings.TrimSpace(*reason) == "" || *pinLabel == "" || len(*pinLabel) > 1024 || strings.ContainsAny(*pinLabel, "\r\n") {
			return fmt.Errorf("pin/unpin requires --backup --pin-label and --reason")
		}
		if operation == "pin" && !strings.HasPrefix(*pinLabel, "manual/") {
			return fmt.Errorf("operator pin labels must start with manual/")
		}
		receipt, e := archive.selectReceipt(*backup, false, ArchiveFilter{})
		if e != nil {
			return e
		}
		dir, lease, e := archive.openBundle(receipt)
		if e != nil {
			return e
		}
		defer dir.Close()
		defer lease.Close()
		return archive.updateControl(state, func(control *ArchiveControl) error {
			labels := []string{}
			found := false
			for _, label := range strings.Split(control.Pins[*backup], "\n") {
				if label == *pinLabel {
					found = true
					if operation == "unpin" {
						continue
					}
				}
				if label != "" {
					labels = append(labels, label)
				}
			}
			if operation == "unpin" && !found {
				return fmt.Errorf("pin label is not present")
			}
			if operation == "pin" && !found {
				labels = append(labels, *pinLabel)
			}
			id, e := safefs.NewID("n")
			if e != nil {
				return e
			}
			if e = archive.dir.Mkdir("control/pin-changes"); e != nil {
				return e
			}
			intent := struct {
				Schema    int    `json:"schema"`
				Backup    string `json:"backup"`
				Operation string `json:"operation"`
				Label     string `json:"label"`
				Reason    string `json:"reason"`
				Revision  uint64 `json:"proposed_revision"`
			}{1, *backup, operation, *pinLabel, *reason, control.Revision + 1}
			if e = archive.dir.WriteJSON("control/pin-changes/"+id+".json", intent, false); e != nil {
				return e
			}
			if len(labels) == 0 {
				delete(control.Pins, *backup)
			} else {
				control.Pins[*backup] = strings.Join(labels, "\n")
			}
			return nil
		})
	case "__maintenance":
		return runScheduledMaintenance(context.Background(), c, state, archive)
	case "__run":
		return runBackupPass(context.Background(), c, state, archive)
	case "accept-generation":
		if *source == "" || *instance == "" || !safefs.ValidID(*generation, "g") || strings.TrimSpace(*reason) == "" {
			return fmt.Errorf("accept-generation requires --source --instance --generation --reason")
		}
		var matched *deploysource.WorkerSource
		for _, candidate := range c.Sources {
			if candidate.SourceID == *source || candidate.Alias == *source {
				v := candidate
				matched = &v
			}
		}
		if matched == nil {
			return fmt.Errorf("source is not configured")
		}
		var discovered deploysource.SourceInventory
		if err = deploysource.QueryRestrictedSource(context.Background(), *matched, c.Policy.Timeouts, "inventory", &discovered); err != nil {
			return err
		}
		if !discovered.Complete || discovered.SourceID != matched.SourceID {
			return fmt.Errorf("source discovery is incomplete")
		}
		for _, item := range discovered.Instances {
			if item.Metadata.Service == *instance && item.Eligible && item.Generation == *generation {
				return archive.updateControl(state, func(control *ArchiveControl) error {
					key := instanceKey(matched.SourceID, *instance)
					value, ok := control.Instances[key]
					if !ok || value.Retired || control.RetiredSources[value.SourceID] != "" {
						return fmt.Errorf("instance is unknown or retired")
					}
					value.Generation = item.Generation
					value.BindingHash = item.BindingHash
					value.LastSuccess = time.Time{}
					control.Instances[key] = value
					return nil
				})
			}
		}
		return fmt.Errorf("source does not prove the requested generation")
	case "reactivate":
		if !safefs.ValidID(*source, "s") || strings.TrimSpace(*reason) == "" {
			return fmt.Errorf("reactivate requires an explicit --source UUID and --reason")
		}
		return archive.updateControl(state, func(control *ArchiveControl) error {
			found := false
			for key, value := range control.Instances {
				if value.SourceID == *source && (*instance == "" || value.Service == *instance) {
					value.Retired = false
					value.RetirementReason = ""
					value.LastSuccess = time.Time{}
					control.Instances[key] = value
					found = true
				}
			}
			if !found {
				return fmt.Errorf("no matching retired instance")
			}
			if *instance == "" {
				delete(control.RetiredSources, *source)
			} else if control.RetiredSources[*source] != "" {
				return fmt.Errorf("reactivate the retired source first")
			}
			return nil
		})
	case "retire":
		// Exclude in-flight delivery before committing retirement and replacing
		// obsolete queued alerts. A busy notifier requires retrying the command.
		dir, err := safefs.Open(state.directory)
		if err != nil {
			return err
		}
		defer dir.Close()
		lease, err := dir.Lock("notifier.lock", true, true)
		if err != nil {
			return err
		}
		defer lease.Close()
		err = archive.updateControl(state, func(control *ArchiveControl) error {
			matches := map[string]bool{}
			for _, value := range control.Instances {
				if value.SourceID == *source || value.Alias == *source {
					matches[value.SourceID] = true
				}
			}
			if len(matches) > 1 {
				return fmt.Errorf("ambiguous source alias; select its UUID")
			}
			found := false
			for key, value := range control.Instances {
				if (value.SourceID == *source || value.Alias == *source) && (*instance == "" || value.Service == *instance) {
					value.Retired = true
					value.RetirementReason = *reason
					control.Instances[key] = value
					found = true
					if *instance == "" {
						control.RetiredSources[value.SourceID] = *reason
					}
				}
			}
			if !found {
				return fmt.Errorf("no enrolled instance matches retirement selector")
			}
			return nil
		})
		if err != nil {
			return err
		}
		return state.reconcileRetiredIncidents(time.Now().UTC())
	case "restore-check":
		if *source == "" || *instance == "" || (*backup == "") == !*latest {
			return fmt.Errorf("restore-check requires --source, --instance and exactly one of --backup or --latest")
		}
		var check *config.WorkerRestoreCheck
		for _, candidate := range c.Policy.RestoreChecks {
			if candidate.Source == *source && candidate.Instance == *instance {
				copy := candidate
				check = &copy
				break
			}
		}
		if check == nil {
			return fmt.Errorf("no worker-configured restore checker for this scope")
		}
		receipt, err := archive.selectReceipt(*backup, *latest, ArchiveFilter{Source: *source, Instance: *instance})
		if err != nil {
			return err
		}
		record, err := archive.restoreCheck(state, receipt, *check)
		_ = json.NewEncoder(os.Stdout).Encode(record)
		return err
	case "verify":
		receipt, err := archive.selectReceipt(*backup, false, ArchiveFilter{})
		if err != nil {
			return err
		}
		record, err := archive.verify(state, receipt, *full)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(record)
	case "reconcile":
		writer, err := archive.dir.Lock(".locks/writer.lock", true, false)
		if err != nil {
			return err
		}
		defer writer.Close()
		return archive.reconcileWork(state)
	case "prune":
		result, err := archive.prune(state, ArchiveFilter{Source: *source, Instance: *instance}, *apply, time.Now().UTC())
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	return fmt.Errorf("unsupported worker operation")
}

func validateWorkerFlags(flags *flag.FlagSet, operation string, operations map[string]string) error {
	names, ok := operations[operation]
	if !ok {
		return fmt.Errorf("unsupported backup-worker operation %q", operation)
	}
	allowed := map[string]bool{}
	for _, name := range strings.Fields(names) {
		allowed[name] = true
	}
	var err error
	flags.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] && err == nil {
			err = fmt.Errorf("--%s does not apply to %s", f.Name, operation)
		}
	})
	return err
}
