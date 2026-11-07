package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/deployment"
	"github.com/dryaf/deploy/internal/inventory"
	notificationinstall "github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/ui"
	"github.com/dryaf/deploy/internal/workerctl"
)

func Main() {
	flag.BoolVar(&ui.DryRun, "dry-run", false, "Print commands without executing")
	flag.BoolVar(&ui.Verbose, "v", false, "Verbose output")
	flag.StringVar(&ui.ServersFile, "servers", "", "Server inventory path (default: server.yaml beside deploy.yaml)")
	flag.BoolVar(&ui.NonInteractive, "non-interactive", false, "Never prompt for worker configuration")
	flag.BoolVar(&ui.JSON, "json", false, "Versioned structured worker output")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		printUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "led-host":
		if ui.DryRun {
			backupFatal("use led-host review for read-only prerequisite inspection")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := workerctl.RunLEDHostCLI(ctx, args[1:], os.Stdout); err != nil {
			backupFatal("LED host prerequisite: %v", err)
		}
	case "__worker_control", "__worker_control_stream":
		if len(args) != 1 || ui.DryRun {
			backupFatal("invalid worker control invocation")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		ctx, deadline := context.WithTimeout(ctx, 15*time.Minute)
		defer deadline()
		watchdog := time.AfterFunc(15*time.Minute, func() { os.Stdin.Close() })
		defer watchdog.Stop()
		handler := workerctl.ControlMain
		if args[0] == "__worker_control_stream" {
			handler = workerctl.ControlMainStream
		}
		if err := handler(ctx, os.Stdin, os.Stdout); err != nil {
			backupFatal("Worker control response failed")
		}
	case "__worker_setup":
		if err := backup.WorkerSetupCoordinator(); err != nil {
			_ = backup.SendSetupFrame(backup.WorkerSetupResponse{Schema: 1, Error: err.Error()})
			backupFatal("Worker setup: %v", err)
		}
	case "__worker_selftest":
		if err := backup.WorkerSelfTest(); err != nil {
			backupFatal("Worker qualification: %v", err)
		}
	case "__source_admin":
		if err := source.Admin(); err != nil {
			backupFatal("Source enrollment: %v", err)
		}
	case "source-serve":
		if len(args) != 5 || args[1] != "--worker-id" || args[3] != "--key-id" {
			backupFatal("invalid source dispatcher invocation")
		}
		if err := source.Serve(args[2], args[4]); err != nil {
			backupFatal("Source: %v", err)
		}
	case "__source_session":
		if len(args) != 3 {
			backupFatal("invalid source session")
		}
		if err := source.Session(args[1], args[2]); err != nil {
			backupFatal("Source: %v", err)
		}
	case "__source_job":
		if len(args) != 3 {
			backupFatal("invalid source capture job")
		}
		if err := source.CaptureJob(args[1], args[2]); err != nil {
			backupFatal("Source job: %v", err)
		}
	case "__source_capture":
		if len(args) != 4 {
			backupFatal("invalid source capture")
		}
		size, err := strconv.ParseInt(args[3], 10, 64)
		if err == nil {
			err = source.Capture(args[1], args[2], size)
		}
		if err != nil {
			backupFatal("Capture: %v", err)
		}
	case "backup-worker":
		if runDeclarativeWorker(args[0], args[1:]) {
			return
		}
		if err := backup.RunCLI(args[1:]); err != nil {
			backupFatal("Backup worker: %v", err)
		}
	case "notification-worker":
		if runDeclarativeWorker(args[0], args[1:]) {
			return
		}
		if err := notificationinstall.RunCLI(args[1:]); err != nil {
			backupFatal("Notification worker: %v", err)
		}
	case "__backup_validate":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--full") {
			backupFatal("invalid validator invocation")
		}
		if err := backup.InternalBackupValidate(len(args) == 2); err != nil {
			backupFatal("%v", err)
		}
	case "ls":
		lsCmd := flag.NewFlagSet("ls", flag.ExitOnError)
		jsonOutput := lsCmd.Bool("json", false, "Versioned JSON inventory")
		managed := lsCmd.Bool("managed", false, "Show managed entries and all diagnostics")
		lsCmd.Parse(args[1:])
		if lsCmd.NArg() > 1 {
			ui.LogFatal("Usage: deploy ls [--json] [--managed] [server]")
		}
		var remote *config.Environment
		if lsCmd.NArg() == 1 {
			s, err := config.SelectServer(ui.InventoryPath(""), lsCmd.Arg(0))
			if err != nil {
				ui.LogFatal("Inventory: %v", err)
			}
			e := config.ServerEnvironment(s)
			remote = &e
		}
		if ui.DryRun {
			fmt.Println("[DRY] Read Quadlet sources, ownership evidence, systemd and Podman state; no remote operations performed")
			return
		}
		inv, err := inventory.Collect(remote)
		if err == nil {
			err = inventory.Print(os.Stdout, inv, *managed, *jsonOutput)
		}
		if err != nil {
			ui.LogFatal("Inventory: %v", err)
		}
		if !inv.Complete {
			os.Exit(1)
		}
	case "validate":
		validateCmd := flag.NewFlagSet("validate", flag.ExitOnError)
		server := validateCmd.Bool("server", false, "Validate server.yaml")
		validateCmd.Parse(args[1:])
		if (*server && validateCmd.NArg() != 0) || (!*server && validateCmd.NArg() != 1) {
			ui.LogFatal("Usage: deploy validate <env> | deploy validate --server")
		}
		if err := deployment.ValidateConfiguration(validateCmd.Arg(0), *server); err != nil {
			ui.LogFatal("Validation: %v", err)
		}
		ui.LogSuccess("Configuration valid.")
	case "init":
		initCmd := flag.NewFlagSet("init", flag.ExitOnError)
		mode := initCmd.String("mode", "binary", "Deployment mode: binary or image")
		initCmd.Parse(args[1:])
		if initCmd.NArg() != 0 {
			ui.LogFatal("Usage: deploy init [--mode=binary|image]")
		}
		deployment.DoInitMode(config.DeploymentMode(*mode))
	case "release":
		// Syntax 1: deploy release <env> (Interactive/Auto)
		// Syntax 2: deploy release <version> <env> (Explicit)
		var envName, version string
		if len(args) == 2 {
			envName = args[1]
			version = "" // Trigger auto-detection
		} else if len(args) == 3 {
			version = args[1]
			envName = args[2]
		} else {
			ui.LogFatal("Usage: deploy release [version] <env>")
		}
		deployment.DoRelease(version, envName)
	case "maintenance":
		// Syntax: deploy maintenance <enable|disable> <env>
		if len(args) < 3 {
			ui.LogFatal("Usage: deploy maintenance <enable|disable> <env>")
		}
		action := args[1]
		envName := args[2]

		if action == "enable" {
			deployment.DoMaintenanceEnable(envName)
		} else if action == "disable" {
			deployment.DoMaintenanceDisable(envName)
		} else {
			ui.LogFatal("Invalid maintenance action '%s'. Use 'enable' or 'disable'.", action)
		}
	case "server":
		if len(args) < 2 {
			ui.LogFatal("Usage: deploy server <init|provision|migrate> [name]")
		}
		switch args[1] {
		case "init":
			if len(args) != 2 {
				ui.LogFatal("Usage: deploy server init")
			}
			deployment.DoServerInit()
		case "provision":
			if len(args) > 3 {
				ui.LogFatal("Usage: deploy server provision [server]")
			}
			name := ""
			if len(args) == 3 {
				name = args[2]
			}
			cfg, err := config.SelectServer(ui.InventoryPath(""), name)
			if err == nil {
				err = deployment.ProvisionServer(cfg, deployment.CommandRunner{})
			}
			if err != nil {
				ui.LogFatal("Server provisioning: %v", err)
			}
			ui.LogSuccess("Server provisioning complete.")
		case "migrate":
			if len(args) == 4 && args[2] == "--to-version" && args[3] == "3" || len(args) == 3 && args[2] == "--to-version=3" {
				if err := config.Version3Candidate(ui.InventoryPath(""), os.Stdout); err != nil {
					ui.LogFatal("Server migration: %v", err)
				}
				return
			}
			if len(args) != 3 {
				ui.LogFatal("Usage: deploy server migrate <name> (writes v2 YAML to stdout)")
			}
			if err := config.MigrateServer(ui.InventoryPath(""), args[2], os.Stdout); err != nil {
				ui.LogFatal("Server migration: %v", err)
			}
		default:
			ui.LogFatal("Invalid server command: %s", args[1])
		}
	case "logs":
		logsCmd := flag.NewFlagSet("logs", flag.ExitOnError)
		usePodman := logsCmd.Bool("podman", false, "Stream 'podman logs'")
		logsCmd.Parse(args[1:])
		if logsCmd.NArg() < 1 {
			ui.LogFatal("Usage: deploy logs [--podman] <env>")
		}
		deployment.DoLogs(logsCmd.Arg(0), *usePodman)
	case "status":
		env := ""
		if len(args) > 1 {
			env = args[1]
		}
		deployment.DoStatus(env)
	case "system-stats":
		// Alias for backward compatibility or explicit single env use
		if len(args) < 2 {
			ui.LogFatal("Usage: deploy system-stats <env>")
		}
		deployment.DoSystemStats(args[1])
	case "system-updates":
		// Syntax: deploy system-updates <status|enable|disable> <env>
		if len(args) < 3 {
			ui.LogFatal("Usage: deploy system-updates <status|enable|disable> <env>")
		}
		deployment.DoSystemUpdates(args[2], args[1])
	case "stop":
		if len(args) < 2 {
			ui.LogFatal("Usage: deploy stop <env>")
		}
		deployment.DoServiceAction(args[1], "stop")
	case "start":
		if len(args) < 2 {
			ui.LogFatal("Usage: deploy start <env>")
		}
		deployment.DoServiceAction(args[1], "start")
	case "restart":
		if len(args) < 2 {
			ui.LogFatal("Usage: deploy restart <env>")
		}
		deployment.DoServiceAction(args[1], "restart")
	case "enable":
		if len(args) < 2 {
			ui.LogFatal("Usage: deploy enable <env>")
		}
		deployment.DoServiceAction(args[1], "enable")
	case "disable":
		if len(args) < 2 {
			ui.LogFatal("Usage: deploy disable <env>")
		}
		deployment.DoServiceAction(args[1], "disable")
	case "db":
		if len(args) < 3 {
			ui.LogFatal("Usage: deploy db <pull|push> <env>")
		}
		if args[1] == "pull" {
			deployment.DoDBPull(args[2])
		} else if args[1] == "push" {
			deployment.DoDBPush(args[2])
		} else if args[1] == "prune-local" {
			pruneCmd := flag.NewFlagSet("db prune-local", flag.ExitOnError)
			list := pruneCmd.Bool("list", false, "Preview local database backup cleanup")
			pruneCmd.Parse(args[2:])
			if pruneCmd.NArg() != 1 {
				ui.LogFatal("Usage: deploy db prune-local [--list] <env>")
			}
			_, e := deployment.LoadEnv(pruneCmd.Arg(0))
			if err := deployment.PruneLocalDatabase(e, *list || ui.DryRun); err != nil {
				ui.LogFatal("Local backup prune: %v", err)
			}
		} else if args[1] == "backup" {
			_, e := deployment.LoadEnv(args[2])
			id, err := deployment.BackupOffHost(e)
			if err != nil {
				ui.LogFatal("Off-host backup: %v", err)
			}
			if !ui.DryRun {
				ui.LogSuccess("Verified off-host backup: %s", id)
			}
		} else if args[1] == "restore-check" && len(args) == 4 {
			_, e := deployment.LoadEnv(args[2])
			if err := deployment.VerifyOffHostRestore(e, args[3]); err != nil {
				ui.LogFatal("Restore check: %v", err)
			}
			if !ui.DryRun {
				ui.LogSuccess("SQLite integrity and application restore check passed.")
			}
		} else {
			ui.LogFatal("Invalid db action: %s", args[1])
		}
	case "gen-auth":
		if len(args) < 3 {
			ui.LogFatal("Usage: deploy gen-auth <user> <password>")
		}
		deployment.DoGenAuth(args[1], args[2])
	case "rights":
		if len(args) < 3 {
			ui.LogFatal("Usage: deploy rights <env> <target>")
		}
		deployment.DoRights(args[1], args[2])
	case "prune":
		pruneCmd := flag.NewFlagSet("prune", flag.ExitOnError)
		history := pruneCmd.Bool("history", false, "Prune configured release and database backup history")
		list := pruneCmd.Bool("list", false, "Preview history cleanup")
		pruneCmd.Parse(args[1:])
		if pruneCmd.NArg() != 1 || (*list && !*history) {
			ui.LogFatal("Usage: deploy prune [--history [--list]] <env>")
		}
		if *history {
			cfg, e := deployment.LoadEnv(pruneCmd.Arg(0))
			n, err := config.Normalize(cfg, e.Name)
			if err == nil {
				err = deployment.PruneHistory(n, *list || ui.DryRun)
			}
			if err != nil {
				ui.LogFatal("History prune: %v", err)
			}
		} else {
			deployment.DoPrune(pruneCmd.Arg(0))
		}
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: deploy [--servers <path>] [--dry-run] <command> [args]")
	fmt.Println("Commands:")
	fmt.Println("  <backup-worker|notification-worker> <plan|status|configure|import|adopt> SERVER  Version-3 policy tools")
	fmt.Println("  --dry-run <backup-worker|notification-worker> SERVER              Offline preview")
	fmt.Println("  server migrate --to-version 3                                    Print a migration candidate")
	fmt.Println("  notification-worker [apply|resume] SERVER                        Install/resume Telegram gateway; admit delivery after qualification")
	fmt.Println("  notification-worker qualify SERVER                             Record actual qualification observations through a resumable wizard")
	fmt.Println("  backup-worker qualify SERVER                                   Record actual backup qualification observations")
	fmt.Println("  backup-worker activate SERVER                                  Enable schedules after qualification and a successful manual backup")
	fmt.Println("  backup-worker run SERVER                                       Request one manual backup; inspect status/logs for completion")
	fmt.Println("  notification-worker test SERVER [--provider NAME]               Test one YAML-configured destination without automatic fallback")
	fmt.Println("  backup-worker [apply|resume|uninstall] SERVER                    Stage backup/dependency or remove standalone services")
	fmt.Println("  notification-worker <test|uninstall> SERVER                       Explicit diagnostic / unbound service removal")
	fmt.Println("  Signal/LED setup and general migrations remain in development.")
	fmt.Println("  backup-worker <setup|update|status|run|list|pull|recover|verify|prune|restore-check|recovery-notifications|recovery-handoff|recovery-led-evidence|recovery-led-clear> …")
	fmt.Println("  notification-worker <init|serve|status|notify-test> --local …")
	fmt.Println("  notification-worker <snapshot|snapshot-verify|recovery-prepare|recovery-status|recovery-checkpoint|recovery-checkpoint-verify|recovery-publication-prepare|recovery-publication-verify|recovery-publish|recovery-review|recovery-decide> --local …")
	fmt.Println("  notification-worker <setup|update> --local --spec <file> (native verification only)")
	fmt.Println("  notification-worker <resume|uninstall|installation-status> --local --root <path>")
	fmt.Println("  notification-worker <qualification-template|qualification-status> --local --root <path>")
	fmt.Println("  notification-worker qualify --local --root <path> --report <file> (records evidence; no activation)")
	fmt.Println("  notification-worker signal-link-status --local --state-dir <account-path>")
	fmt.Println("  notification-worker signal-link-resolve --local --state-dir <account-path> --operation <id> --accept-account-state --reason <text>")
	fmt.Println("  notification-worker <queue-inspect|receipts-inspect> --local …")
	fmt.Println("  notification-worker receipts-compact --local --config <file> --state-dir <path> --producer <id> [--apply]")
	fmt.Println("  notification-worker rollback --local --root <path> --from-revision <id> (same-schema executable only)")
	fmt.Println("  notification-worker <message-review|message-decide|message-decision-status> --local … (stopped worker)")
	fmt.Println("  ls [--json] [--managed] [server] Read local or named-server inventory")
	fmt.Println("  validate <env>|--server  Validate configuration locally")
	fmt.Println("  init [--mode=image]      Generate deploy.yaml")
	fmt.Println("  release [tag] <env>      Deploy to env. If tag omitted, auto-detects or prompts.")
	fmt.Println("  status [env]             Show detailed system health. If env omitted, shows all.")
	fmt.Println("  maintenance <ac> <env>   Manage maintenance page (ac: enable|disable)")
	fmt.Println("  system-updates <ac> <env> Manage unattended upgrades (status|enable|disable)")
	fmt.Println("  start <env>              Start service")
	fmt.Println("  stop <env>               Stop service")
	fmt.Println("  restart <env>            Restart service")
	fmt.Println("  enable <env>             Enable service at boot")
	fmt.Println("  disable <env>            Disable service at boot")
	fmt.Println("  prune <env>              Clean up unused images/builder cache")
	fmt.Println("  server <init|provision|migrate> [name] Manage server inventories/infrastructure")
	fmt.Println("  logs <env>               Stream logs")
	fmt.Println("  db pull <env>            Sync DB (Remote -> Local)")
	fmt.Println("  db push <env>            Overwrite Remote DB (Service MUST be stopped first)")
	fmt.Println("  db backup <env>          Create and verify an off-host snapshot")
	fmt.Println("  db restore-check <env> <id> Verify off-host backup with application check")
	fmt.Println("  gen-auth <u?> <p?>       Generate Basic Auth string")
	fmt.Println("  rights <env> <target>    Manual permission fix (target: 'user' or 'container')")
}

func backupFatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func runDeclarativeWorker(kind string, args []string) bool {
	if !workerctl.Matches(args) {
		return false
	}
	options := workerctl.Options{Inventory: ui.InventoryPath(""), DryRun: ui.DryRun, JSON: ui.JSON, NonInteractive: ui.NonInteractive}
	// Version-2 named status/uninstall retain their original worker lifecycle.
	// New operations require an explicit version-3 declaration.
	if call, err := workerctl.Parse(args, options); err == nil && (call.Operation == "status" || call.Operation == "uninstall" || call.Operation == "resume" || call.Operation == "activate" || call.Operation == "run") {
		if inv, err := config.ReadInventory(call.Options.Inventory); err == nil && inv.Version != 3 {
			return false
		}
	}
	controller := workerctl.CLI{Out: os.Stdout, Err: os.Stderr}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := controller.Run(ctx, kind, args, options); err != nil {
		backupFatal("Worker: %v", err)
	}
	return true
}
