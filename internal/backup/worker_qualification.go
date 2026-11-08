package backup

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	deployconfig "github.com/dryaf/deploy/internal/config"
	notificationprotocol "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type QualificationCheck struct {
	Passed   bool   `json:"passed"`
	Evidence string `json:"evidence"`
}

type WorkerQualification struct {
	Schema       int                           `json:"schema"`
	WorkerID     string                        `json:"worker_id"`
	ArchiveID    string                        `json:"archive_id"`
	ConfigHash   string                        `json:"config_hash"`
	Architecture string                        `json:"architecture"`
	WorkerImage  string                        `json:"worker_image"`
	CheckedAt    time.Time                     `json:"checked_at"`
	Checks       map[string]QualificationCheck `json:"checks"`
}

func qualifyWorkerCLI(args []string) error {
	flags := flag.NewFlagSet("backup-worker qualify", flag.ContinueOnError)
	report := flags.String("report", "", "Completed live qualification report JSON")
	local := flags.Bool("local", false, "Record on this worker host")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *report == "" || *local && flags.NArg() != 0 || !*local && flags.NArg() != 1 {
		return fmt.Errorf("qualify requires --report <file> and a worker name or --local")
	}
	file, err := os.Open(*report)
	if err != nil {
		return err
	}
	defer file.Close()
	var proof WorkerQualification
	if err = safefs.ReadJSON(file, &proof, protocol.MaxBackupHeader); err != nil {
		return err
	}
	if ui.DryRun {
		fmt.Println("[DRY] Validate and record explicit qualification evidence; no services started.")
		return nil
	}
	if *local {
		return recordWorkerQualification(proof)
	}
	server, err := deployconfig.SelectServer(ui.InventoryPath(""), flags.Arg(0))
	if err != nil {
		return err
	}
	body, _ := json.Marshal(proof)
	cmd := workerAdminCommand(server, []string{"__qualify"})
	cmd.Stdin = strings.NewReader(string(body))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return process.Run(cmd, 2*time.Minute, 10*time.Second)
}

func recordWorkerQualification(proof WorkerQualification) error {
	home, err := deploysource.TrustedSourceHome()
	if err != nil {
		return err
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	layout, _, err := installedBackupPaths(root)
	if err != nil {
		return err
	}
	c, err := loadInstalledWorker(home)
	if err != nil {
		return err
	}
	dir, err := safefs.Open(layout.Config)
	if err != nil {
		return err
	}
	defer dir.Close()
	images := map[string]string{}
	if err = dir.ReadJSON("images.json", &images); err != nil {
		return err
	}
	if proof.Schema != 1 || proof.WorkerID != c.WorkerID || proof.ArchiveID != c.ArchiveID || proof.ConfigHash != c.ConfigHash || proof.Architecture != runtime.GOARCH || proof.WorkerImage != images["worker"] || proof.CheckedAt.After(time.Now().Add(time.Minute)) || time.Since(proof.CheckedAt) > 7*24*time.Hour {
		return fmt.Errorf("qualification report does not match this installation and recent test window")
	}
	for _, name := range workerQualificationChecks(c) {
		check := proof.Checks[name]
		if !check.Passed || len(strings.TrimSpace(check.Evidence)) < 8 || len(check.Evidence) > 4096 {
			return fmt.Errorf("missing passing qualification evidence: %s", name)
		}
	}
	state, err := openInstalledBackupState(home, false)
	if err != nil {
		return err
	}
	defer state.Close()
	if err = requireNoPendingDeclaredActivation(state); err != nil {
		return err
	}
	if err = state.put("qualification", proof); err != nil {
		return err
	}
	fmt.Println("Qualification evidence recorded for this installation. Use activate to enable the timers.")
	return nil
}

func applyWorkerQualification(c WorkerInstallation, state *backupState) WorkerInstallation {
	c.Qualification = copyQualification(c.Qualification)
	var proof WorkerQualification
	if state.get("qualification", &proof) == nil && proof.Schema == 1 && proof.ConfigHash == c.ConfigHash && proof.WorkerID == c.WorkerID && proof.ArchiveID == c.ArchiveID {
		c.Qualification["storage_durability"] = proof.Checks["storage_crash_recovery"].Passed
		c.Qualification["notification_delivery"] = proof.Checks["notification_receipt"].Passed
		c.Qualification["external_monitor"] = proof.Checks["missed_worker_route"].Passed && proof.Checks["daily_delivery_route"].Passed
		c.Qualification["application_restore"] = proof.Checks["isolated_application_restore"].Passed
	}
	return c
}

func copyQualification(original map[string]bool) map[string]bool {
	result := map[string]bool{}
	for key, value := range original {
		result[key] = value
	}
	return result
}

func activateWorker(c WorkerInstallation, state *backupState) error {
	return activateWorkerUsing(c, state, requireSharedBackupReportUnits, deploysource.QualifyRestrictedSource)
}
func activateWorkerUsing(c WorkerInstallation, state *backupState, verifyReports func(string, WorkerInstallation, *backupState, backupNotificationOwner) error, qualifySource func(deploysource.WorkerSource, deployconfig.WorkerTimeouts) error) error {
	if err := requireBackupActivityReady(state); err != nil {
		return err
	}
	home := strings.TrimSuffix(state.directory, "/.local/state/deploy/backup-worker/state")
	if c.Paths != nil {
		if c.Paths.Validate() != nil || c.Paths.State != state.directory {
			return fmt.Errorf("activation state differs from retained layout")
		}
		home = c.Paths.Home
	}
	if home != state.directory {
		root, e := safefs.Open(home)
		if e != nil {
			return e
		}
		defer root.Close()
		lock, e := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
		if e != nil {
			return e
		}
		defer lock.Close()
		var setup WorkerSetupJournal
		if e = root.ReadJSON(backupSetupFile, &setup); e != nil {
			return e
		}
		if c.Paths != nil {
			p, e := declarationPaths(home, setup.Declaration)
			if e != nil || notificationprotocol.Hash(p) != notificationprotocol.Hash(*c.Paths) {
				return fmt.Errorf("activation path authority changed")
			}
		}
		if setup.Activity != nil && (!setup.Activity.Complete || setup.Activity.Disabled) {
			return fmt.Errorf("finish or re-enable declared backup activity before activation")
		}
	}
	if err := requireNoPendingDeclaredActivation(state); err != nil {
		return err
	}
	units, err := validateWorkerActivation(c, state, verifyReports, qualifySource)
	if err != nil {
		return err
	}
	if _, err := process.LimitedOutput(exec.Command("systemctl", append([]string{"--user", "enable", "--now"}, units...)...), 30*time.Second, 4096); err != nil {
		_, _ = process.LimitedOutput(exec.Command("systemctl", append([]string{"--user", "disable", "--now"}, units...)...), 30*time.Second, 4096)
		return fmt.Errorf("worker timer activation failed; activation rolled back")
	}
	if err := state.put("installation", "active"); err != nil {
		return err
	}
	fmt.Println("Qualified backup, host-health and current reporting timers activated.")
	return nil
}

// Caller holds the installation setup lease. This validates all existing gates
// without enabling a unit or printing into a framed controller response.
func validateWorkerActivation(c WorkerInstallation, state *backupState, verifyReports func(string, WorkerInstallation, *backupState, backupNotificationOwner) error, qualifySource func(deploysource.WorkerSource, deployconfig.WorkerTimeouts) error) ([]string, error) {
	var control ArchiveControl
	if err := state.get("control", &control); err != nil || (control.ReportingDisabled != (c.Policy.Notifications.Mode == "disabled") || control.SharedRequired != (c.Policy.Notifications.Mode == "shared")) {
		return nil, fmt.Errorf("activation reporting policy differs from retained archive control")
	}
	owner, err := currentSharedBackupOwner(state)
	if err != nil {
		return nil, err
	}
	if c.Policy.Notifications.Mode == "shared" && owner == nil {
		return nil, fmt.Errorf("connect the qualified shared reporting owner before activation")
	}
	if owner != nil {
		if c.Policy.Notifications.Mode == "disabled" {
			return nil, fmt.Errorf("disabled reporting conflicts with shared notification ownership")
		}
		home := strings.TrimSuffix(state.directory, "/.local/state/deploy/backup-worker/state")
		if c.Paths != nil {
			if c.Paths.Validate() != nil || c.Paths.State != state.directory {
				return nil, fmt.Errorf("shared activation state differs from retained layout")
			}
			home = c.Paths.Home
		} else if state.directory != filepath.Join(home, ".local/state/deploy/backup-worker/state") {
			return nil, fmt.Errorf("shared activation requires canonical backup state")
		}
		if err = verifyReports(home, c, state, *owner); err != nil {
			return nil, err
		}
	}
	if err := notification.ValidateMonitoringChecks(c.Policy.Monitoring); err != nil {
		return nil, err
	}
	c = applyWorkerQualification(c, state)
	for _, gate := range workerQualificationGates(c, true) {
		if !c.Qualification[gate] {
			return nil, fmt.Errorf("activation requires qualification: %s", gate)
		}
	}
	if c.Policy.Notifications.Mode != "disabled" && (c.Policy.Monitoring.BackupPingURLFile == "" || c.Policy.Monitoring.DailyDeliveryPingURLFile == "") {
		return nil, fmt.Errorf("activation requires separate qualified backup and daily-delivery monitoring checks")
	}
	var run WorkerRun
	if err := state.get("run", &run); err != nil || !run.Complete || run.ConfigHash != c.ConfigHash || run.FinishedAt.IsZero() || run.FinishedAt.After(time.Now()) || time.Since(run.FinishedAt) > time.Hour || len(run.Failures) != 0 {
		return nil, fmt.Errorf("activation requires a recent successful manual backup pass")
	}
	if err := requireSharedActivityRun(state, run); err != nil {
		return nil, err
	}
	for _, source := range c.Sources {
		if err := qualifySource(source, c.Policy.Timeouts); err != nil {
			return nil, err
		}
	}
	return workerPolicyActivationUnits(c, owner != nil), nil
}

func signalLinkWorker(home, configuration, state, explicitOperation string, newLink bool) error {
	// The notifier/schema leases held by the caller must belong to the same
	// installed worker as the fixed managed relay. An alternate state directory
	// must never bypass that installation's notification ownership fence.
	layout, err := lookupInstalledBackupPaths(home)
	if err != nil {
		return err
	}
	if configuration != layout.Runtime || state != layout.State {
		return fmt.Errorf("Signal service maintenance requires the installed worker configuration and state paths")
	}
	c, err := loadInstalledWorker(home)
	if err != nil {
		return err
	}
	if c.Policy.Notifications.Signal == nil {
		return fmt.Errorf("Signal is not configured")
	}
	dir, err := safefs.Open(layout.Config)
	if err != nil {
		return err
	}
	defer dir.Close()
	raw, err := safefs.ReadFile(dir, "images.json", 8192)
	if err != nil {
		return err
	}
	images := map[string]string{}
	if err = safefs.DecodeJSON(raw, &images, 8192); err != nil {
		return err
	}
	if err = deployconfig.PinnedWorkerImage(images["relay"]); err != nil {
		return fmt.Errorf("no pinned Signal relay image installed")
	}
	paths, err := layout.UnitPaths(c.Policy.Storage.Path)
	if err != nil {
		return err
	}
	units, err := renderWorkerUnits(c, paths, images["worker"], images["relay"])
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	svc := &signalService{home: home, configuration: configuration, image: images["relay"], unitText: units["deploy-backup-signal.container"], imagesHash: notificationprotocol.Hash(raw), installation: c, run: signalSystemctl}
	view, err := svc.Inspect(ctx)
	if err != nil {
		return err
	}
	svc.pinned = view.Binding
	// Probe without account mounts or network before publishing a service gate.
	if err = signalAccountProbe(ctx, images["relay"]); err != nil {
		return err
	}
	status, err := signalaccount.InspectMaintenance(paths.RelayState)
	if err != nil {
		return err
	}
	operation, err := selectSignalOperation(status, explicitOperation, newLink)
	if err != nil {
		return err
	}

	request := signalaccount.ServiceRequest{Operation: operation, Name: "deploy-backup", Binding: view.Binding}
	result, err := signalaccount.MaintainLink(ctx, paths.RelayState, request, svc, func(ctx context.Context) error {
		cmd := signalLinkCommand(paths.RelayState, images["relay"], operation)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return process.RunContext(ctx, cmd, 6*time.Minute, 10*time.Second)
	})
	if result.Operation != "" {
		if e := json.NewEncoder(os.Stdout).Encode(result); e != nil {
			return e
		}
	}
	return err
}

// The container entry point owns the account lease independently of Podman/SSH.
// Older images without this entry point fail closed; never fall back to invoking
// signal-cli directly, which bypasses both the lease and uncertain-link journal.
func signalLinkCommand(statePath, image, operation string) *exec.Cmd {
	return exec.Command("podman", "run", "--rm", "--pull=never", "--read-only", "--cap-drop=all", "--security-opt=no-new-privileges", "--memory=768m", "--pids-limit=256", "--tmpfs=/tmp:rw,size=256m", "--env=JAVA_TOOL_OPTIONS=-Xmx256m", "--volume", statePath+":/signal-state:rw", "--entrypoint=/usr/local/bin/deploy", image, "notification-worker", "__signal-link", "--local", "--state-dir", "/signal-state", "--operation", operation, "--name", "deploy-backup")
}

func internalQualification() error {
	var proof WorkerQualification
	if err := safefs.ReadJSON(io.LimitReader(os.Stdin, protocol.MaxBackupHeader+1), &proof, protocol.MaxBackupHeader); err != nil {
		return err
	}
	return recordWorkerQualification(proof)
}
