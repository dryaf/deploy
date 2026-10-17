package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
    
    # Global auth and Watchtower provisioning are not implemented.
    # Configure authentication in the application router or existing middleware.
`
	if dryRun {
		logInfo("[DRY] Create server.yaml")
		return
	}
	if err := os.WriteFile("server.yaml", []byte(defaultConfig), 0644); err != nil {
		logFatal("Failed to write server.yaml: %v", err)
	}
	logSuccess("Created server.yaml. Please edit it with your VPS details.")
}

// Provisioning returns every failure to the CLI; success means the service is active.
func doServerProvision() {
	if err := provisionServer(loadServerConfig(), commandRunner{}); err != nil {
		logFatal("Server provisioning failed: %v", err)
	}
	logSuccess("Server provisioning complete.")
}

func provisionServer(cfg ServerConfig, runner ReleaseRunner) error {
	if !hostPattern.MatchString(cfg.Host) || !userPattern.MatchString(cfg.User) || cfg.SSHPort < 0 || cfg.SSHPort > 65535 {
		return fmt.Errorf("invalid server SSH configuration")
	}
	if cfg.Stack.Traefik.Auth.Provider != "" && cfg.Stack.Traefik.Auth.Provider != "none" {
		return fmt.Errorf("global auth provisioning is not implemented; provision middleware separately or use router.basic_auth_users")
	}
	if cfg.Stack.Watchtower.Schedule != "" {
		return fmt.Errorf("Watchtower provisioning is not implemented")
	}
	if dryRun {
		logInfo("[DRY] Provision Traefik on %s", cfg.Host)
		return nil
	}
	env := Environment{Host: cfg.Host, User: cfg.User, Port: sshPort(Environment{Port: cfg.SSHPort}), SSHKey: cfg.SSHKey}
	return provisionTraefik(env, cfg.Stack.Traefik, runner)
}

func provisionTraefik(env Environment, cfg TraefikStack, runner ReleaseRunner) error {
	network := cfg.NetworkName
	if network == "" {
		network = "traefik-net"
	}
	if !servicePattern.MatchString(network) || cfg.Version == "" {
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
	data := TraefikTemplateData{TraefikConfig: TraefikConfig{Version: cfg.Version, Email: cfg.Email, Dashboard: cfg.Dashboard, NetworkName: network, CertResolver: "myresolver"}, HostUID: uid}
	if err := os.MkdirAll("build/stack", 0700); err != nil {
		return err
	}
	for name, tmpl := range map[string]string{"traefik.yml": traefikYmlTmpl, "traefik.container": traefikContainerTmpl, network + ".network": networkTmpl} {
		if err := genFile(filepath.Join("build/stack", name), tmpl, data); err != nil {
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
