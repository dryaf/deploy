package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/crypto/bcrypt"
)

func doStatus(envName string) {
	if envName != "" {
		// Single env status
		doSystemStats(envName)
		return
	}

	// All envs status
	cfg := loadConfig()
	if len(cfg.Environments) == 0 {
		logWarn("No environments defined in deploy.yaml")
		return
	}

	// Sort keys for consistent output order
	keys := make([]string, 0, len(cfg.Environments))
	for k := range cfg.Environments {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		fmt.Printf("\n------------------------------------------------------------\n")
		fmt.Printf(" 🌍 ENVIRONMENT: %s\n", k)
		fmt.Printf("------------------------------------------------------------\n")
		doSystemStats(k)
	}
}

func doSystemStats(envName string) {
	cfg, env := loadEnv(envName)
	if cfg.Deployment.Mode == ModeImage || cfg.Deployment.Mode == ModeBinary {
		n, err := normalizeAndValidateConfig(cfg, envName)
		if err != nil {
			logFatal("Config: %v", err)
		}
		fmt.Printf("Configured image: %s\n", n.Env.Quadlet.Image)
		if err := runSSHStream(env, imageStatusScript(n)); err != nil {
			logWarn("Image state: %v", err)
		}
	}
	logInfo("📊 Fetching sophisticated stats from %s (%s)...", envName, env.Host)

	// We construct a robust shell script to run everything in one SSH session.
	// NOTE: We must use "%%" for literal % signs in the shell script because
	// this string is passed to fmt.Sprintf in Go.
	containerName := "systemd-" + env.Quadlet.ServiceName

	script := fmt.Sprintf(`
		# Colors
		BLUE='\033[1;34m'
		RED='\033[0;31m'
		GREEN='\033[0;32m'
		YELLOW='\033[0;33m'
		NC='\033[0m'

		# --- 1. HOST INFO ---
		echo -e "${BLUE}=== 🖥️  SYSTEM HEALTH ===${NC}"
		if [ -f /etc/os-release ]; then
			source /etc/os-release
			printf "OS:      ${PRETTY_NAME}\n"
		else
			printf "OS:      $(uname -s)\n"
		fi
		printf "Kernel:  $(uname -r) ($(uname -m))\n"
		printf "Uptime:  $(uptime -p)\n"

		# Load & Memory
		printf "Load:    $(cat /proc/loadavg | awk '{print $1, $2, $3}')\n"
		printf "Memory:  $(free -h | awk '/^Mem:/ {print $3 " / " $2}')\n"

		# Disk Usage (Target Dir Partition)
		# Use %%s in printf to safely handle string with %% sign
		DISK_INFO=$(df -h %s | awk 'NR==2 {print $3 " / " $2 " (" $5 ")"}')
		printf "Disk:    %%s\n" "${DISK_INFO}"

		# --- 2. MAINTENANCE ---
		echo ""
		echo -e "${BLUE}=== 📦 UPDATES ===${NC}"
		UPDATES="Unknown"
		if command -v apt &> /dev/null; then
			# Debian/Ubuntu
			CNT=$(apt list --upgradable 2>/dev/null | grep -v "Listing..." | wc -l)
			UPDATES="${CNT} packages pending"

			# Check Unattended Upgrades Status
			if systemctl is-active unattended-upgrades &>/dev/null; then
				# Check config file for explicit enable
				if grep -q 'APT::Periodic::Unattended-Upgrade "1"' /etc/apt/apt.conf.d/20auto-upgrades 2>/dev/null; then
					UU_STATUS="${GREEN}Enabled (Active)${NC}"
				else
					UU_STATUS="${YELLOW}Service Active / Config Disabled${NC}"
				fi
			else
				UU_STATUS="${RED}Disabled${NC}"
			fi
		elif command -v dnf &> /dev/null; then
			# Fedora/RHEL
			CNT=$(dnf check-update -q 2>/dev/null | wc -l)
			if [ "$CNT" -gt 0 ]; then UPDATES="~${CNT} packages pending"; else UPDATES="System up to date"; fi
			UU_STATUS="N/A (Not apt)"
		elif command -v apk &> /dev/null; then
			# Alpine
			CNT=$(apk version -l '<' 2>/dev/null | wc -l)
			UPDATES="${CNT} packages pending"
			UU_STATUS="N/A (Not apt)"
		fi

		if [[ "$UPDATES" != *"0 packages"* && "$UPDATES" != "System up to date" && "$UPDATES" != "Unknown" ]]; then
			printf "Status:      ${YELLOW}%%s${NC}\n" "${UPDATES}"
		else
			printf "Status:      ${GREEN}System up to date${NC}\n"
		fi
		if [ ! -z "$UU_STATUS" ]; then
			printf "Auto-Update: ${UU_STATUS}\n"
		fi

		# --- 3. SECURITY ---
		echo ""
		echo -e "${BLUE}=== 🛡️  SECURITY (24h) ===${NC}"

		# Failed SSH Attempts (parsing journalctl)
		if command -v journalctl &> /dev/null; then
			FAILURES=$(journalctl -u ssh -u sshd -q --since "24 hours ago" | grep -i "Failed password" | wc -l)
			if [ "$FAILURES" -gt 0 ]; then
				printf "Failed Logins: ${RED}%%s attempts${NC}\n" "${FAILURES}"
			else
				printf "Failed Logins: ${GREEN}0${NC}\n"
			fi
		else
			printf "Failed Logins: (Cannot read logs)\n"
		fi

		# Last 3 Logins
		printf "Last Logins:\n"
		# Use %%s for awk print formats to avoid Go fmt.Sprintf swallowing them
		last -n 3 -a -i | head -n 3 | awk '{printf "  - %%s (%%s %%s %%s) from %%s\n", $1, $4, $5, $6, $NF}'

		# --- 4. SERVICE ---
		echo ""
		echo -e "${BLUE}=== ⚙️  SERVICE (%s) ===${NC}"
		SYSTEMD_STATUS=$(systemctl --user is-active %s.service 2>/dev/null)
		if [ "$SYSTEMD_STATUS" == "active" ]; then
			printf "Status:  ${GREEN}Active (Running)${NC}\n"
			# Show runtime
			systemctl --user status %s.service --no-pager | grep "Active:" | sed 's/^[ \t]*//'
		else
			printf "Status:  ${RED}${SYSTEMD_STATUS:-Not Found}${NC}\n"
		fi

		# --- 5. CONTAINER ---
		echo ""
		echo -e "${BLUE}=== 🐳 CONTAINER ===${NC}"
		# Podman ps name filter
		if podman ps -q --filter name=%s | grep -q .; then
			# Podman stats
			podman stats --no-stream --format "table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}\t{{.BlockIO}}" %s
		else
			printf "${YELLOW}Container is NOT running.${NC}\n"
		fi

	`, shellQuote(env.Dir), env.Quadlet.ServiceName, env.Quadlet.ServiceName, env.Quadlet.ServiceName, containerName, containerName)

	if err := runSSHStream(env, script); err != nil {
		logError("Failed to retrieve stats: %v", err)
	}
}

func doSystemUpdates(envName, action string) {
	_, env := loadEnv(envName)
	logInfo("📦 Managing Unattended Upgrades on %s (%s)...", envName, env.Host)

	var script string
	switch action {
	case "status":
		script = `
			echo "Checking status..."
			if ! command -v apt-get &>/dev/null; then echo "Not a Debian/Ubuntu system."; exit 1; fi
			dpkg -l | grep unattended-upgrades >/dev/null || echo "Package: NOT INSTALLED"
			systemctl is-active unattended-upgrades >/dev/null && echo "Service: ACTIVE" || echo "Service: INACTIVE"
			if grep -q 'APT::Periodic::Unattended-Upgrade "1"' /etc/apt/apt.conf.d/20auto-upgrades 2>/dev/null; then
				echo "Config:  ENABLED"
			else
				echo "Config:  DISABLED"
			fi
		`
	case "enable":
		// Requires sudo
		script = `
			set -e
			echo "Installing/Enabling..."
			if ! command -v apt-get &>/dev/null; then echo "Not a Debian/Ubuntu system."; exit 1; fi
			sudo apt-get update >/dev/null
			sudo apt-get install -y unattended-upgrades >/dev/null
			# Enable in apt config
			echo 'APT::Periodic::Update-Package-Lists "1";' | sudo tee /etc/apt/apt.conf.d/20auto-upgrades
			echo 'APT::Periodic::Unattended-Upgrade "1";' | sudo tee -a /etc/apt/apt.conf.d/20auto-upgrades
			sudo systemctl enable unattended-upgrades
			sudo systemctl restart unattended-upgrades
			echo "✅ Unattended Upgrades ENABLED."
		`
	case "disable":
		script = `
			set -e
			echo "Disabling..."
			if ! command -v apt-get &>/dev/null; then echo "Not a Debian/Ubuntu system."; exit 1; fi
			# Disable in apt config
			echo 'APT::Periodic::Update-Package-Lists "0";' | sudo tee /etc/apt/apt.conf.d/20auto-upgrades
			echo 'APT::Periodic::Unattended-Upgrade "0";' | sudo tee -a /etc/apt/apt.conf.d/20auto-upgrades
			sudo systemctl stop unattended-upgrades
			sudo systemctl disable unattended-upgrades
			echo "❌ Unattended Upgrades DISABLED."
		`
	default:
		logFatal("Invalid action. Use 'status', 'enable', or 'disable'.")
	}

	if err := runSSH(env, script); err != nil {
		logFatal("Failed to perform upgrades action: %v", err)
	}
}

func doGenAuth(user, password string) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		logFatal("Hash generation failed: %v", err)
	}
	fmt.Printf("%s:%s\n", user, string(hash))
}

func doPrune(envName string) {
	_, env := loadEnv(envName)
	if err := pruneImages(env); err != nil {
		logFatal("Prune: %v", err)
	}
	if dryRun {
		logInfo("[DRY] Prune dangling images and build cache under the storage lock")
		return
	}
	logSuccess("Prune complete.")
}

func pruneImages(env Environment) error {
	limits, _ := json.Marshal(struct {
		Timeouts operationLimits `json:"timeouts"`
	}{limitsFor(env)})
	script := transactionCommonScript + "\nconfigure_timeouts <(printf '%s' " + shellQuote(string(limits)) + ")\nstore_lock -x\npodman image prune -f\npodman image prune --build-cache -f\n"
	_, err := runSSHOutput(env, script)
	return err
}

func doRights(envName, target string) {
	_, env := loadEnv(envName)
	if err := rights(env, target); err != nil {
		logFatal("Permissions: %v", err)
	}
	if dryRun {
		logInfo("[DRY] Require an inactive service and update configured ownership while preserving backups")
		return
	}
	logSuccess("Permissions updated.")
}

func rights(env Environment, target string) error {
	uid, gid := "0", "0"
	switch target {
	case "user":
	case "container":
		if env.Quadlet.ContainerUID <= 0 {
			return fmt.Errorf("container_uid not set in config")
		}
		uid, gid = fmt.Sprint(env.Quadlet.ContainerUID), fmt.Sprint(env.Quadlet.ContainerGID)
	default:
		return fmt.Errorf("invalid target; use user or container")
	}
	return changeOwnership(env, uid, gid)
}

func changeOwnership(env Environment, uid, gid string) error {
	if len(env.Quadlet.ChownVolumes) == 0 {
		return fmt.Errorf("no chown_volumes configured")
	}
	script := remoteOperationGuard(env) + `
if service_is_active; then fail 'Stop the service before changing ownership'
else [[ $? == 1 ]] || exit 1; fi
`
	backup := ""
	if env.Database.Driver == "sqlite" {
		backup = path.Join(env.Dir, path.Dir(env.Database.Source), "backup")
	}
	for _, owned := range env.Quadlet.ChownVolumes {
		if !path.IsAbs(owned) {
			owned = path.Join(env.Dir, owned)
		}
		owned = path.Clean(owned)
		if backup != "" && (owned == backup || strings.HasPrefix(owned, backup+"/")) {
			return fmt.Errorf("chown_volumes cannot target the database backup directory")
		}
		script += "check_path " + shellQuote(owned) + "\n"
		// find's default physical traversal and chown --no-dereference preserve links.
		script += "podman unshare find " + shellQuote(owned)
		if backup != "" {
			script += " -path " + shellQuote(backup) + " -prune -o"
		}
		script += " -exec chown --no-dereference " + shellQuote(uid+":"+gid) + " -- '{}' + || { echo " + shellQuote("Ownership change failed at "+owned+"; earlier paths may have changed; retry after correcting the error") + " >&2; exit 1; }\n"
	}
	_, err := runSSHOutput(env, script)
	return err
}

func doLogs(envName string, usePodman bool) {
	_, env := loadEnv(envName)
	cmd := fmt.Sprintf("journalctl --user -u %s.service -f", env.Quadlet.ServiceName)
	if usePodman {
		cmd = fmt.Sprintf("podman logs -f systemd-%s", env.Quadlet.ServiceName)
	}
	logInfo("Streaming logs...")
	if dryRun {
		logInfo("[DRY] %s", cmd)
		return
	}
	if err := prepareSSHControlDir(); err != nil {
		logFatal("SSH: %v", err)
	}

	sshArgs := getSSHBaseArgs(env)
	sshArgs = append(sshArgs, "-t", cmd)

	c := exec.Command("ssh", sshArgs...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	if err := c.Run(); err != nil {
		logFatal("Logs: %v", err)
	}
}

func doServiceAction(envName, action string) {
	cfg, err := readConfig("deploy.yaml")
	if err != nil {
		logFatal("Config: %v", err)
	}
	n, err := normalizeAndValidateConfig(cfg, envName)
	if err != nil {
		logFatal("Config: %v", err)
	}
	valid := map[string]bool{"start": true, "stop": true, "restart": true, "enable": true, "disable": true}
	if !valid[action] {
		logFatal("Invalid service action %q", action)
	}
	if dryRun {
		fmt.Printf("[DRY] %s %s.service (mode=%s; recover pending state under service lock for image mode)\n", action, n.Env.Quadlet.ServiceName, n.Mode)
		return
	}
	if n.Mode == ModeImage {
		err = executeImageServiceAction(n, action, commandRunner{})
	} else {
		err = executeDeploymentServiceAction(n, action, commandRunner{}, binaryTransactionScript)
	}
	if err != nil {
		logFatal("Service action: %v", err)
	}
	logSuccess("Service action %s completed.", action)
}

// --- INIT LOGIC ---

type InitContext struct {
	AppName    string
	BinaryName string
	User       string
}

func doInit() { doInitMode(ModeBinary) }

func doInitMode(mode DeploymentMode) {
	if mode != ModeBinary && mode != ModeImage {
		logFatal("mode must be binary or image")
	}
	if dryRun {
		fmt.Printf("[DRY] Create %s deploy.yaml\n", mode)
		return
	}
	if _, err := os.Stat("deploy.yaml"); err == nil {
		logFatal("deploy.yaml already exists")
	}

	// 1. Detect Context
	cwd, err := os.Getwd()
	if err != nil {
		logFatal("Could not get working directory: %v", err)
	}
	appName := filepath.Base(cwd)

	// Normalize appName (lowercase, replace spaces)
	appName = strings.ToLower(strings.ReplaceAll(appName, " ", "-"))
	if !servicePattern.MatchString(appName) {
		appName = "my-service"
	}

	// Detect User
	userName := "deploy_user"
	u, err := user.Current()
	if err == nil && u.Username != "" {
		// Clean username (e.g., on Windows "DOMAIN\User" -> "User")
		parts := strings.Split(u.Username, "\\")
		userName = parts[len(parts)-1]
	}

	data := InitContext{
		AppName:    appName,
		BinaryName: appName + "-server", // Convention
		User:       userName,
	}

	logInfo("✨ Initializing deploy.yaml for app '%s' with user '%s'...", data.AppName, data.User)

	// 2. Render Template
	configTemplate := defaultConfigTmpl
	if mode == ModeImage {
		configTemplate = imageConfigTmpl
	}
	tmpl, err := template.New("init").Parse(configTemplate)
	if err != nil {
		logFatal("Internal template error: %v", err)
	}

	f, err := os.OpenFile("deploy.yaml", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		logFatal("Failed to create file: %v", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		logFatal("Failed to write config: %v", err)
	}

	logSuccess("Created deploy.yaml. Please edit 'host' and 'ssh_key' details.")
}

const defaultConfigTmpl = `app_name: "{{ .AppName }}"
binary_name: "{{ .BinaryName }}"

build:
  arch: "amd64"
  # Standard local build.
  # This uses placeholders {{ "{{" }}.Version{{ "}}" }} which are injected during 'deploy release'
  ldflags: "-s -w -X 'main.Version={{ "{{" }}.Version{{ "}}" }}' -X 'main.Commit={{ "{{" }}.Commit{{ "}}" }}'"

  # Cross-compilation helper for Mac/Windows Users (CGO/SQLite Support).
  # Uncomment this 'cmd' block to build inside a Linux container using Podman.
  # This ensures SQLite drivers are compiled correctly for Alpine Linux.
  # cmd: >-
  #   podman run --rm -v "$(pwd):/app" -w /app
  #   docker.io/library/golang:1.24.5-alpine
  #   sh -c "apk add --no-cache gcc musl-dev git &&
  #   go build -ldflags=\"-w -s -extldflags '-static'
  #   -X 'main.buildVersion={{ "{{" }}.Version{{ "}}" }}'
  #   -X 'main.buildDate={{ "{{" }}.Date{{ "}}" }}'
  #   -X 'main.buildTag={{ "{{" }}.Tag{{ "}}" }}'
  #   -X 'main.goVersion={{ "{{" }}.GoVersion{{ "}}" }}'\"
  #   -o build/{{ .BinaryName }} ."

artifacts:
  # Files to sync to the remote server.
  # Note: No trailing slash on directories unless you specifically want rsync contents-only behavior.
  include: ["migrations", "Dockerfile.vps"]
  exclude: ["data", "*.db", ".env", ".git", ".idea", ".vscode"]

# Global Maintenance Configuration (Optional)
maintenance:
  enabled: true
  title: "Under Maintenance"
  text: "We are currently upgrading the {{ .AppName }} system. Please try again in a minute."

environments:
  prod:
    host: "vps.example.com"
    user: "{{ .User }}"
    ssh_port: 22
    # ssh_key: "~/.ssh/id_ed25519_vps"
    target_dir: "/home/{{ .User }}/web/{{ .AppName }}"
    sync_env_file: ".env"

    # Traefik configuration is now managed via 'deploy server init/provision' (server.yaml)

    quadlet:
      service_name: "{{ .AppName }}"
      image: "localhost/{{ .AppName }}:latest"
      network: "traefik-net.network"
      auto_restart: true
      timezone: "Europe/Vienna"
      exec: "/{{ .BinaryName }}"
      # Adjust this endpoint to your application's readiness check.
      health_required: true
      health_cmd: "wget -q -O /dev/null http://127.0.0.1:8080/health"
      # stop_on_deploy: true

      container_uid: 65532
      container_gid: 65532
      chown_volumes: ["./data"]

      volumes:
        - "./data:/data:Z"
        - "./migrations:/migrations:ro,Z"

      router:
        host: "{{ .AppName }}.example.com"
        internal_port: 8080
        https_redirect: true

      env_vars:
        - "APP_ENV=production"
        - "DATASTORE_TYPE=sqlite"
`

const imageConfigTmpl = `app_name: "{{ .AppName }}"
deployment:
  mode: image

environments:
  prod:
    host: vps.example.com
    user: "{{ .User }}"
    ssh_port: 22
    target_dir: "/home/{{ .User }}/web/{{ .AppName }}"
    quadlet:
      service_name: "{{ .AppName }}"
      # Replace this reference with your verified version and production digest.
      image: "docker.io/library/nginx:1.28-alpine"
      network: traefik-net.network
      network_aliases: ["{{ .AppName }}-prod"]
      router:
        enabled: false
      env_file: ""
      auto_restart: true
      health_timeout: 60s
      health_required: true
      health_cmd: "wget -q -O /dev/null http://127.0.0.1:80/"
`
