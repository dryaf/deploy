package main

import (
	"flag"
	"fmt"
	"os"
)

// --- Global Flags ---
var (
	dryRun  bool
	verbose bool
)

func main() {
	flag.BoolVar(&dryRun, "dry-run", false, "Print commands without executing")
	flag.BoolVar(&verbose, "v", false, "Verbose output")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		printUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "init":
		doInit()
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
			logFatal("Usage: deploy release [version] <env>")
		}
		doRelease(version, envName)
	case "maintenance":
		// Syntax: deploy maintenance <enable|disable> <env>
		if len(args) < 3 {
			logFatal("Usage: deploy maintenance <enable|disable> <env>")
		}
		action := args[1]
		envName := args[2]

		if action == "enable" {
			doMaintenanceEnable(envName)
		} else if action == "disable" {
			doMaintenanceDisable(envName)
		} else {
			logFatal("Invalid maintenance action '%s'. Use 'enable' or 'disable'.", action)
		}
	case "server":
		if len(args) < 2 {
			logFatal("Usage: deploy server <init|provision>")
		}
		switch args[1] {
		case "init":
			doServerInit()
		case "provision":
			doServerProvision()
		default:
			logFatal("Invalid server command: %s", args[1])
		}
	case "logs":
		logsCmd := flag.NewFlagSet("logs", flag.ExitOnError)
		usePodman := logsCmd.Bool("podman", false, "Stream 'podman logs'")
		logsCmd.Parse(args[1:])
		if logsCmd.NArg() < 1 {
			logFatal("Usage: deploy logs [--podman] <env>")
		}
		doLogs(logsCmd.Arg(0), *usePodman)
	case "status":
		env := ""
		if len(args) > 1 {
			env = args[1]
		}
		doStatus(env)
	case "system-stats":
		// Alias for backward compatibility or explicit single env use
		if len(args) < 2 {
			logFatal("Usage: deploy system-stats <env>")
		}
		doSystemStats(args[1])
	case "system-updates":
		// Syntax: deploy system-updates <status|enable|disable> <env>
		if len(args) < 3 {
			logFatal("Usage: deploy system-updates <status|enable|disable> <env>")
		}
		doSystemUpdates(args[2], args[1])
	case "stop":
		if len(args) < 2 {
			logFatal("Usage: deploy stop <env>")
		}
		doServiceAction(args[1], "stop")
	case "start":
		if len(args) < 2 {
			logFatal("Usage: deploy start <env>")
		}
		doServiceAction(args[1], "start")
	case "restart":
		if len(args) < 2 {
			logFatal("Usage: deploy restart <env>")
		}
		doServiceAction(args[1], "restart")
	case "enable":
		if len(args) < 2 {
			logFatal("Usage: deploy enable <env>")
		}
		doServiceAction(args[1], "enable")
	case "disable":
		if len(args) < 2 {
			logFatal("Usage: deploy disable <env>")
		}
		doServiceAction(args[1], "disable")
	case "db":
		if len(args) < 3 {
			logFatal("Usage: deploy db <pull|push> <env>")
		}
		if args[1] == "pull" {
			doDBPull(args[2])
		} else if args[1] == "push" {
			doDBPush(args[2])
		} else {
			logFatal("Invalid db action: %s", args[1])
		}
	case "gen-auth":
		if len(args) < 3 {
			logFatal("Usage: deploy gen-auth <user> <password>")
		}
		doGenAuth(args[1], args[2])
	case "rights":
		if len(args) < 3 {
			logFatal("Usage: deploy rights <env> <target>")
		}
		doRights(args[1], args[2])
	case "prune":
		if len(args) < 2 {
			logFatal("Usage: deploy prune <env>")
		}
		doPrune(args[1])
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: deploy <command> [args]")
	fmt.Println("Commands:")
	fmt.Println("  init                     Generate deploy.yaml")
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
	fmt.Println("  server <init|provision>  Manage Server Infrastructure (Traefik/Auth)")
	fmt.Println("  logs <env>               Stream logs")
	fmt.Println("  db pull <env>            Sync DB (Remote -> Local)")
	fmt.Println("  db push <env>            Overwrite Remote DB (Service MUST be stopped first)")
	fmt.Println("  gen-auth <u?> <p?>       Generate Basic Auth string")
	fmt.Println("  rights <env> <target>    Manual permission fix (target: 'user' or 'container')")
}
