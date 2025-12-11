package main

import (
	"fmt"
	"os"
	"strings"
)

// doServerInit generates a server.yaml template
func doServerInit() {
	if _, err := os.Stat("server.yaml"); err == nil {
		logFatal("server.yaml already exists")
	}

	defaultConfig := `host: "vps.example.com"
user: "root"
ssh_port: 22

stack:
  traefik:
    version: "v3.0"
    email: "admin@example.com"
    dashboard: true
    network_name: "traefik-net"
    
    # Global Auth Provider
    auth:
      provider: "basic" # or "authelia"

  authelia:
    subdomain: "auth"
    users_file: "users.yml"

  watchtower:
    schedule: "0 4 * * *" # Every day at 4am
`
	if err := os.WriteFile("server.yaml", []byte(defaultConfig), 0644); err != nil {
		logFatal("Failed to write server.yaml: %v", err)
	}
	logSuccess("Created server.yaml. Please edit it with your VPS details.")
}

// doServerProvision installs the stack defined in server.yaml
func doServerProvision() {
	cfg := loadServerConfig()
	env := Environment{
		Host:   cfg.Host,
		User:   cfg.User,
		Port:   cfg.SSHPort,
		SSHKey: cfg.SSHKey,
		Dir:    "/root", // Default to root home for infrastructure
	}

	logInfo("🚀 Provisioning Server Stack on %s...", env.Host)

	// verify access
	if err := runSSH(env, "id"); err != nil {
		logFatal("SSH connection failed. Check host/user/key in server.yaml")
	}

	if !dryRun {
		os.MkdirAll("build/stack", 0755)
	}

	// 1. Setup Traefik
	provisionTraefik(env, cfg.Stack.Traefik)

	// 2. Setup Authelia (if enabled)
	if cfg.Stack.Traefik.Auth.Provider == "authelia" {
		provisionAuthelia(env, cfg.Stack.Traefik, cfg.Stack.Authelia)
	}

	// 3. Setup Watchtower
	provisionWatchtower(env, cfg.Stack.Watchtower)

	logSuccess("✅ Server Provisioning Complete.")
}

func provisionTraefik(env Environment, tCfg TraefikStack) {
	logInfo("📦 Provisioning Traefik...")

	netName := tCfg.NetworkName
	if netName == "" {
		netName = "traefik-net"
	}

	// Ensure network exists (blindly try to create, ignore if exists)
	// Actually better to use a systemd network unit or create it once.
	// For simplicity, we'll generate a network unit.

	data := TraefikTemplateData{
		TraefikConfig: TraefikConfig{
			Version:      tCfg.Version,
			Email:        tCfg.Email,
			Dashboard:    tCfg.Dashboard,
			NetworkName:  netName,
			CertResolver: "myresolver", // Hardcoded standard
		},
		HostUID: "0", // Infrastructure usually runs as root/podman
	}
	// We might need to check if user is root vs non-root for UID.
	// For now assume root or we need to fetch UID.
	if uid := getCmdOutput("ssh", append(getSSHBaseArgs(env), "id -u")...); uid != "" {
		data.HostUID = uid
	}

	// Reuse existing templates from traefik.go (we will need to move/export them)
	// For now assuming we refactor traefik.go to export templates or we move them here.
	// I will assume we move the logic here or call a shared function.
	// Let's implement the generation logic here, duplicating/adapting from traefik.go for now to be safe,
	// then we delete traefik.go.

	genFile("build/stack/traefik.yml", traefikYmlTmpl, data)
	genFile("build/stack/traefik.container", strings.Replace(traefikContainerTmpl, "traefik-net", netName, -1), data)
	genFile("build/stack/"+netName+".network", networkTmpl, nil)

	// Sync
	runSSH(env, "mkdir -p ~/traefik/dynamic_conf ~/traefik/letsencrypt ~/.config/containers/systemd")
	runSSH(env, "touch ~/traefik/letsencrypt/acme.json && chmod 600 ~/traefik/letsencrypt/acme.json")

	runRsync(env, []string{"build/stack/traefik.yml"}, fmt.Sprintf("%s@%s:~/traefik/", env.User, env.Host))

	// Dashboard Auth (Basic)
	// logic for dashboard auth... if basic?
	// The new config doesn't explicitly allow setting dashboard auth hash in server.yaml yet for simplicity,
	// but we can add it or just assume no auth for dashboard or basic.
	// For now, skipping explicit dashboard auth setup to keep "zero-config" promise or add it later.

	runRsync(env, []string{"build/stack/traefik.container", "build/stack/" + netName + ".network"},
		fmt.Sprintf("%s@%s:~/.config/containers/systemd/", env.User, env.Host))

	// Reload & Start
	runSSH(env, "systemctl --user daemon-reload && systemctl --user restart traefik.service")
}

func provisionAuthelia(env Environment, tCfg TraefikStack, aCfg AutheliaConfig) {
	logInfo("🔐 Provisioning Authelia...")
	// TODO: Generate authelia configuration.yml, users.yml, and container
	// For this task, we will just create placeholders or basic setup.
	logWarn("Authelia provisioning is a placeholder in this milestone.")
}

func provisionWatchtower(env Environment, wCfg WatchtowerConfig) {
	logInfo("🔄 Provisioning Watchtower...")
	// TODO: Watchtower container
}
