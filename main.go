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
	case "run":
		if len(args) < 2 {
			logFatal("Usage: deploy [flags] run <env>")
		}
		doRun(args[1])
	case "traefik":
		if len(args) < 2 {
			logFatal("Usage: deploy traefik <env>")
		}
		doTraefikSetup(args[1])
	case "logs":
		logsCmd := flag.NewFlagSet("logs", flag.ExitOnError)
		usePodman := logsCmd.Bool("podman", false, "Stream 'podman logs'")
		logsCmd.Parse(args[1:])
		if logsCmd.NArg() < 1 {
			logFatal("Usage: deploy logs [--podman] <env>")
		}
		doLogs(logsCmd.Arg(0), *usePodman)
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
	fmt.Println("  init                  Generate deploy.yaml")
	fmt.Println("  run <env>             Deploy app")
	fmt.Println("  start <env>           Start service")
	fmt.Println("  stop <env>            Stop service")
	fmt.Println("  restart <env>         Restart service")
	fmt.Println("  enable <env>          Enable service at boot")
	fmt.Println("  disable <env>         Disable service at boot")
	fmt.Println("  prune <env>           Clean up unused images/builder cache")
	fmt.Println("  traefik <env>         Setup Traefik infrastructure")
	fmt.Println("  logs <env>            Stream logs")
	fmt.Println("  db pull <env>         Sync DB (Remote -> Local)")
	fmt.Println("  db push <env>         Sync DB (Local -> Remote)")
	fmt.Println("  gen-auth <u?> <p?>    Generate Basic Auth string")
	fmt.Println("  rights <env> <target> Manual permission fix (target: 'user' or 'container')")
}
