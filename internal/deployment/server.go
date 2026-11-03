package deployment

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/ui"
)

// doServerInit generates a server.yaml template
func DoServerInit() {
	file := ui.InventoryPath("")
	if _, err := os.Lstat(file); err == nil {
		ui.LogFatal("%s already exists", file)
	} else if !os.IsNotExist(err) {
		ui.LogFatal("%s: %v", file, err)
	}

	defaultConfig := `version: 2
servers:
  vps-main:
    host: "vps.example.com"
    user: "deploy"
    ssh_port: 22
    stack:
      traefik:
        version: "v3.0"
        email: "admin@example.com"
        dashboard: true
        network_name: "traefik-net"

# Provision with: deploy server provision vps-main
# Worker-only hosts may omit stack entirely.
`
	if ui.DryRun {
		ui.LogInfo("[DRY] Create %s", file)
		return
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		ui.LogFatal("Create inventory: %v", err)
	}
	_, err = f.WriteString(defaultConfig)
	closeErr := f.Close()
	if err != nil {
		ui.LogFatal("Write inventory: %v", err)
	}
	if closeErr != nil {
		ui.LogFatal("Close inventory: %v", closeErr)
	}
	ui.LogSuccess("Created %s. Edit it with your server details.", file)
}

// Provisioning returns every failure to the CLI; success means the service is active.
func doServerProvision() {
	if err := ProvisionServer(loadServerConfig(), CommandRunner{}); err != nil {
		ui.LogFatal("Server provisioning failed: %v", err)
	}
	ui.LogSuccess("Server provisioning complete.")
}

func ProvisionServer(cfg config.ServerConfig, runner ReleaseRunner) error {
	if !config.HostPattern.MatchString(cfg.Host) || !config.UserPattern.MatchString(cfg.User) || cfg.SSHPort < 0 || cfg.SSHPort > 65535 {
		return fmt.Errorf("invalid server SSH configuration")
	}
	if cfg.Stack.Traefik.Auth.Provider != "" && cfg.Stack.Traefik.Auth.Provider != "none" {
		return fmt.Errorf("global auth provisioning is not implemented; provision middleware separately or use router.basic_auth_users")
	}
	if cfg.Stack.Watchtower.Schedule != "" {
		return fmt.Errorf("Watchtower provisioning is not implemented")
	}
	if cfg.Stack.Traefik.Version == "" {
		return fmt.Errorf("selected server has no Traefik stack to provision")
	}
	if ui.DryRun {
		ui.LogInfo("[DRY] Provision Traefik on %s", cfg.Host)
		return nil
	}
	env := config.ServerEnvironment(cfg)
	return provisionTraefik(env, cfg.Stack.Traefik, runner)
}

func provisionTraefik(env config.Environment, cfg config.TraefikStack, runner ReleaseRunner) error {
	network := cfg.NetworkName
	if network == "" {
		network = "traefik-net"
	}
	if !config.ServicePattern.MatchString(network) || cfg.Version == "" {
		return fmt.Errorf("Traefik requires a version and a valid network name")
	}
	uid, err := runner.SSH(env, "id -u")
	if err != nil {
		return fmt.Errorf("read remote user: %w", err)
	}
	// UID is used in a generated unit path, so require an actual numeric response.
	uid = strings.TrimSpace(uid)
	if _, err := strconv.ParseUint(uid, 10, 32); err != nil {
		return fmt.Errorf("invalid remote UID")
	}
	data := quadlet.TraefikTemplateData{TraefikConfig: config.TraefikConfig{Version: cfg.Version, Email: cfg.Email, Dashboard: cfg.Dashboard, NetworkName: network, CertResolver: "myresolver"}, HostUID: uid}
	if err := os.MkdirAll("build/stack", 0700); err != nil {
		return err
	}
	for name, tmpl := range map[string]string{"traefik.yml": quadlet.TraefikYmlTmpl, "traefik.container": quadlet.TraefikContainerTmpl, network + ".network": quadlet.NetworkTmpl} {
		if err := ui.GenFile(filepath.Join("build/stack", name), tmpl, data); err != nil {
			return fmt.Errorf("generate %s: %w", name, err)
		}
	}
	if _, err := runner.SSH(env, "mkdir -p ~/traefik/dynamic_conf ~/traefik/letsencrypt ~/.config/containers/systemd && touch ~/traefik/letsencrypt/acme.json && chmod 600 ~/traefik/letsencrypt/acme.json"); err != nil {
		return fmt.Errorf("prepare Traefik: %w", err)
	}
	if err := runner.Sync(env, []string{"build/stack/traefik.yml"}, remoteDestination(env, "traefik/")); err != nil {
		return fmt.Errorf("upload Traefik configuration: %w", err)
	}
	if err := runner.Sync(env, []string{"build/stack/traefik.container", "build/stack/" + network + ".network"}, remoteDestination(env, ".config/containers/systemd/")); err != nil {
		return fmt.Errorf("upload Traefik units: %w", err)
	}
	if _, err := runner.SSH(env, "systemctl --user daemon-reload && systemctl --user enable --now podman.socket && systemctl --user restart traefik.service && systemctl --user is-active --quiet traefik.service"); err != nil {
		return fmt.Errorf("activate Traefik: %w", err)
	}
	return nil
}
