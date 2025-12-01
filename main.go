package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// --- Configuration Structs ---

type Config struct {
	AppName      string                 `yaml:"app_name"`
	BinaryName   string                 `yaml:"binary_name"`
	Build        BuildConfig            `yaml:"build"`
	Artifacts    ArtifactsConfig        `yaml:"artifacts"`
	Environments map[string]Environment `yaml:"environments"`
}

type BuildConfig struct {
	Arch    string `yaml:"arch"`
	Ldflags string `yaml:"ldflags"`
	Dir     string `yaml:"dir"`
	Cmd     string `yaml:"cmd"`
}

type ArtifactsConfig struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type Environment struct {
	Host        string         `yaml:"host"`
	User        string         `yaml:"user"`
	Port        int            `yaml:"ssh_port"`
	SSHKey      string         `yaml:"ssh_key"`
	Dir         string         `yaml:"target_dir"`
	SyncEnvFile string         `yaml:"sync_env_file"`
	Quadlet     Quadlet        `yaml:"quadlet"`
	Database    DatabaseConfig `yaml:"database"`
	Traefik     TraefikConfig  `yaml:"traefik"`
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	Source string `yaml:"source"`
}

type TraefikConfig struct {
	Version       string `yaml:"version"`
	Email         string `yaml:"email"`
	CertResolver  string `yaml:"cert_resolver"`
	NetworkName   string `yaml:"network_name"`
	Dashboard     bool   `yaml:"dashboard"`
	DashboardAuth string `yaml:"dashboard_auth"`
}

type RouterConfig struct {
	Enabled       bool              `yaml:"enabled"`
	Host          string            `yaml:"host"`
	Rule          string            `yaml:"rule"`
	InternalPort  int               `yaml:"internal_port"`
	EntryPoints   []string          `yaml:"entrypoints"`
	CertResolver  string            `yaml:"cert_resolver"`
	HTTPSRedirect bool              `yaml:"https_redirect"`
	PathPrefix    string            `yaml:"path_prefix"`
	StripPrefix   bool              `yaml:"strip_prefix"`
	Compress      bool              `yaml:"compress"`
	BasicAuth     []string          `yaml:"basic_auth_users"`
	BasicAuthFile string            `yaml:"basic_auth_file"`
	IPAllowList   []string          `yaml:"ip_allowlist"`
	RateLimit     *RateLimitConfig  `yaml:"rate_limit"`
	Headers       map[string]string `yaml:"headers"`
}

type RateLimitConfig struct {
	Average int `yaml:"average"`
	Burst   int `yaml:"burst"`
}

type Quadlet struct {
	ServiceName string       `yaml:"service_name"`
	Description string       `yaml:"description"`
	Image       string       `yaml:"image"`
	Network     string       `yaml:"network"`
	Labels      []string     `yaml:"labels"`
	Router      RouterConfig `yaml:"router"`
	Volumes     []string     `yaml:"volumes"`
	EnvVars     []string     `yaml:"env_vars"`
	Ports       []string     `yaml:"ports"`
	AutoRestart bool         `yaml:"auto_restart"`
	Timezone    string       `yaml:"timezone"`
	Memory      string       `yaml:"memory"`
	CPU         string       `yaml:"cpu"`
	ReadOnly    bool         `yaml:"read_only"`
	HealthCmd   string       `yaml:"health_cmd"`
	HealthURL   string       `yaml:"health_url"` // New: Application level health check
	PodmanArgs  []string     `yaml:"podman_args"`
	Exec        string       `yaml:"exec"`
	Dockerfile  string       `yaml:"dockerfile"`

	ContainerUID int      `yaml:"container_uid"`
	ContainerGID int      `yaml:"container_gid"`
	ChownVolumes []string `yaml:"chown_volumes"`
}

type TemplateData struct {
	Quadlet
	TargetDir string
}

type TraefikTemplateData struct {
	TraefikConfig
	HostUID string
}

type BuildMetadata struct {
	Version string
	Commit  string
	Date    string
	Tag     string
}

// --- Global Flags & Constants ---
var (
	dryRun  bool
	verbose bool
)

const (
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Blue   = "\033[34m"
	Gray   = "\033[37m"
)

// --- Templates ---

const traefikContainerTmpl = `[Unit]
Description=Traefik Reverse Proxy
After=network-online.target
Wants=network-online.target

[Container]
Image=docker.io/library/traefik:{{ .Version }}
Network={{ if .NetworkName }}{{ .NetworkName }}{{ else }}traefik-net{{ end }}.network
PublishPort=80:80
PublishPort=443:443
Volume=/run/user/{{ .HostUID }}/podman/podman.sock:/var/run/docker.sock:Z
Volume=%h/traefik/traefik.yml:/etc/traefik/traefik.yml:ro,Z
Volume=%h/traefik/dynamic_conf:/etc/traefik/dynamic_conf:ro,Z
Volume=%h/traefik/letsencrypt:/letsencrypt:Z
Exec=--configfile=/etc/traefik/traefik.yml

[Install]
WantedBy=default.target
`

const traefikYmlTmpl = `api:
  dashboard: {{ .Dashboard }}

entryPoints:
  web:
    address: ":80"
    http:
      redirections:
        entryPoint:
          to: websecure
          scheme: https
  websecure:
    address: ":443"

certificatesResolvers:
  {{ .CertResolver }}:
    acme:
      email: "{{ .Email }}"
      storage: "/letsencrypt/acme.json"
      httpChallenge:
        entryPoint: web

providers:
  docker:
    endpoint: "unix:///var/run/docker.sock"
    exposedByDefault: false
  file:
    directory: "/etc/traefik/dynamic_conf"
    watch: true
`

const traefikDashboardTmpl = `http:
  routers:
    dashboard:
      rule: Host("traefik.localhost") || (PathPrefix("/api") && Headers("Referer", "traefik"))
      service: api@internal
      middlewares:
        - auth
  middlewares:
    auth:
      basicAuth:
        users:
          - "{{ .DashboardAuth }}"
`

const networkTmpl = `[Network]
Driver=bridge
`

const quadletTemplate = `[Unit]
Description={{ if .Description }}{{ .Description }}{{ else }}{{ .ServiceName }} Service{{ end }}
Requires=traefik.service
After=network-online.target traefik.service
Wants=network-online.target

[Container]
Image={{ .Image }}
{{- if .Exec }}
Exec={{ .Exec }}
{{- end }}
{{- if .Network }}
Network={{ .Network }}
{{- end }}
{{- if .Timezone }}
Timezone={{ .Timezone }}
{{- end }}
{{- if .Memory }}
Memory={{ .Memory }}
{{- end }}
{{- if .CPU }}
CPUQuota={{ .CPU }}
{{- end }}
{{- if .ReadOnly }}
ReadOnly=true
{{- end }}
{{- if .HealthCmd }}
HealthCmd={{ .HealthCmd }}
HealthInterval=60s
HealthRetries=3
{{- end }}
{{- range .Ports }}
PublishPort={{ . }}
{{- end }}
{{- range .Volumes }}
Volume={{ . }}
{{- end }}
{{- range .EnvVars }}
Environment={{ . }}
{{- end }}
{{- range .PodmanArgs }}
PodmanArgs={{ . }}
{{- end }}
EnvironmentFile={{ .TargetDir }}/.env
{{- range .Labels }}
Label="{{ . }}"
{{- end }}

[Install]
WantedBy=default.target
`

func getDefaultConfig() string {
	return `app_name: "my-app"
binary_name: "server"

build:
  arch: "amd64"
  ldflags: "-s -w -X 'main.Version={{.Version}}' -X 'main.Commit={{.Commit}}'"

artifacts:
  include: ["migrations/", "Dockerfile"]
  exclude: ["data/", "*.db"]

environments:
  prod:
    host: "vps.example.com"
    user: "deploy_user"
    ssh_port: 22
    # ssh_key: "~/.ssh/id_ed25519_vps" # Optional
    target_dir: "/home/deploy_user/web/my-app"
    sync_env_file: ".env"

    traefik:
      email: "admin@example.com"
      network_name: "traefik-net"

    quadlet:
      service_name: "my-app"
      image: "localhost/my-app:latest"
      network: "traefik-net.network"
      auto_restart: true
      timezone: "Europe/Vienna"

      container_uid: 65532
      container_gid: 65532
      chown_volumes: ["./data"]

      volumes:
        - "./data:/data:Z"

      router:
        host: "app.example.com"
        internal_port: 8080
        https_redirect: true

      # health_url: "http://localhost:8080/health" # Application check

      env_vars:
        - "APP_ENV=production"
`
}

// --- Main ---

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
			logFatal("Usage: deploy rights <env> <user|container>")
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
	fmt.Println("  prune <env>           Clean up unused images/builder cache")
	fmt.Println("  traefik <env>         Setup Traefik infrastructure")
	fmt.Println("  logs <env>            Stream logs")
	fmt.Println("  db pull <env>         Sync DB (Remote -> Local)")
	fmt.Println("  db push <env>         Sync DB (Local -> Remote)")
	fmt.Println("  gen-auth <u?> <p?>    Generate Basic Auth string")
	fmt.Println("  rights <env> <target> Manual permission fix (target: 'user' or 'container')")
}

// --- Helpers ---

func doGenAuth(user, password string) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		logFatal("Hash generation failed: %v", err)
	}
	fmt.Printf("%s:%s\n", user, string(hash))
}

func doPrune(envName string) {
	_, env := loadEnv(envName)
	logInfo("🧹 Pruning unused resources on %s (%s)...", envName, env.Host)

	logInfo("   - Pruning dangling images...")
	if err := runSSH(env, "podman image prune -f"); err != nil {
		logWarn("Image prune warning: %v", err)
	}

	logInfo("   - Pruning build cache...")
	if err := runSSH(env, "podman builder prune -f"); err != nil {
		logWarn("Builder prune warning: %v", err)
	}

	logSuccess("✅ Prune complete.")
}

func doRights(envName, target string) {
	_, env := loadEnv(envName)
	if len(env.Quadlet.ChownVolumes) == 0 {
		logWarn("No 'chown_volumes' configured for this environment.")
		return
	}

	var uid, gid string
	if target == "user" {
		logInfo("🔧 Reclaiming ownership for SSH User...")
		uid = "$(id -u)"
		gid = "$(id -g)"
	} else if target == "container" {
		logInfo("🔧 Setting ownership for Container (%d:%d)...", env.Quadlet.ContainerUID, env.Quadlet.ContainerGID)
		if env.Quadlet.ContainerUID == 0 {
			logFatal("container_uid not set in config")
		}
		uid = fmt.Sprintf("%d", env.Quadlet.ContainerUID)
		gid = fmt.Sprintf("%d", env.Quadlet.ContainerGID)
	} else {
		logFatal("Invalid target. Use 'user' or 'container'")
	}

	changeOwnership(env, uid, gid)
	logSuccess("Permissions updated.")
}

func changeOwnership(env Environment, uid, gid string) {
	var paths []string
	for _, p := range env.Quadlet.ChownVolumes {
		if strings.HasPrefix(p, "./") {
			p = fmt.Sprintf("%s/%s", strings.TrimRight(env.Dir, "/"), strings.TrimPrefix(p, "./"))
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return
	}

	cmd := fmt.Sprintf("podman unshare chown -R %s:%s %s", uid, gid, strings.Join(paths, " "))
	runSSH(env, cmd)
}

// --- Traefik Setup ---

func doTraefikSetup(envName string) {
	_, env := loadEnv(envName)
	if env.Traefik.Email == "" {
		logFatal("Traefik email missing in deploy.yaml")
	}

	version := env.Traefik.Version
	if version == "" || version == "latest" {
		logInfo("🔍 Checking GitHub for latest Traefik version...")
		if v, err := fetchLatestGitHubRelease("traefik/traefik"); err == nil {
			version = v
			logInfo("Latest version: %s", version)
		} else {
			version = "v3.0"
			logWarn("GitHub check failed. Defaulting to %s", version)
		}
	}
	env.Traefik.Version = version

	logInfo("🚀 Configuring Traefik on %s...", env.Host)

	sshArgs := getSSHBaseArgs(env)
	sshArgs = append(sshArgs, "id -u")
	uidStr := getCmdOutput("ssh", sshArgs...)
	if uidStr == "" {
		logFatal("Cannot determine remote UID")
	}

	if !dryRun {
		os.MkdirAll("build/traefik", 0755)
	}
	tmplData := TraefikTemplateData{env.Traefik, uidStr}

	netName := env.Traefik.NetworkName
	if netName == "" {
		netName = "traefik-net"
	}

	genFile("build/traefik/traefik.yml", traefikYmlTmpl, tmplData)
	genFile("build/traefik/traefik.container", strings.Replace(traefikContainerTmpl, "traefik-net", netName, -1), tmplData)
	genFile("build/traefik/"+netName+".network", networkTmpl, nil)

	if env.Traefik.Dashboard && env.Traefik.DashboardAuth != "" {
		if !dryRun {
			os.MkdirAll("build/traefik/dynamic_conf", 0755)
		}
		genFile("build/traefik/dynamic_conf/dashboard.yml", traefikDashboardTmpl, tmplData)
	}

	logInfo("📂 Setting up remote directories & permissions...")
	runSSH(env, "mkdir -p ~/traefik/dynamic_conf ~/traefik/letsencrypt ~/.config/containers/systemd")
	runSSH(env, "touch ~/traefik/letsencrypt/acme.json && chmod 600 ~/traefik/letsencrypt/acme.json")

	logInfo("📤 Syncing configs...")
	runRsync(env, []string{"build/traefik/traefik.yml"}, fmt.Sprintf("%s@%s:~/traefik/", env.User, env.Host))
	if env.Traefik.DashboardAuth != "" {
		runRsync(env, []string{"build/traefik/dynamic_conf/"}, fmt.Sprintf("%s@%s:~/traefik/dynamic_conf/", env.User, env.Host))
	}
	runRsync(env, []string{"build/traefik/traefik.container", "build/traefik/" + netName + ".network"},
		fmt.Sprintf("%s@%s:~/.config/containers/systemd/", env.User, env.Host))

	logInfo("🔄 Starting Traefik...")
	script := strings.Join([]string{
		"systemctl --user daemon-reload",
		"systemctl --user restart traefik.service",
		"sleep 2",
		"systemctl --user is-active traefik.service",
	}, " && ")

	if err := runSSH(env, script); err != nil {
		logFatal("Traefik failed to start. Check 'deploy logs traefik'")
	}
	logSuccess("✅ Traefik deployed successfully.")
}

// --- App Deployment ---

func doRun(envName string) {
	cfg, env := loadEnv(envName)

	if _, err := exec.LookPath("rsync"); err != nil {
		logFatal("Local rsync missing")
	}

	// Pre-flight checks (Optimized SSH connection check)
	logInfo("🔍 Verifying remote environment on %s...", env.Host)
	if err := runSSH(env, "command -v rsync >/dev/null && command -v podman >/dev/null"); err != nil {
		logFatal("Remote check failed: 'rsync' and 'podman' are required on the host.")
	}

	logInfo("🚀 Deploying %s to %s...", cfg.AppName, envName)

	if !dryRun {
		os.MkdirAll("build", 0755)
	}

	// Build
	arch := cfg.Build.Arch
	if arch == "" {
		arch = "amd64"
	}
	logInfo("🔨 Building binary (%s)...", arch)

	buildMeta := getBuildMetadata()
	var ldflags string
	if cfg.Build.Ldflags != "" {
		tmpl, err := template.New("ld").Parse(cfg.Build.Ldflags)
		if err != nil {
			logFatal("LDFLAGS template error: %v", err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, buildMeta); err != nil {
			logFatal("LDFLAGS exec: %v", err)
		}
		ldflags = buf.String()
	} else {
		ldflags = fmt.Sprintf("-s -w -X 'main.buildVersion=%s' -X 'main.buildDate=%s'", buildMeta.Version, buildMeta.Date)
	}

	var cmd *exec.Cmd
	if cfg.Build.Cmd != "" {
		logInfo("   Using custom build command...")
		cmd = exec.Command("sh", "-c", cfg.Build.Cmd)
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, fmt.Sprintf("LDFLAGS=%s", ldflags))
	} else {
		srcDir := "."
		if cfg.Build.Dir != "" {
			srcDir = cfg.Build.Dir
		}
		output := fmt.Sprintf("build/%s", cfg.BinaryName)
		cmd = exec.Command("go", "build", "-ldflags", ldflags, "-o", output, srcDir)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	}

	// --- FIX: Error output visible ---
	if err := runCommand("Build", cmd); err != nil {
		logFatal("Build failed: %v", err)
	}

	// Generate Labels
	logInfo("📄 Generating configuration...")
	env.Quadlet.Labels = generateTraefikLabels(env.Quadlet.ServiceName, env.Quadlet.Router, env.Traefik.CertResolver)
	containerPath := generateQuadlet(env, "build")

	// Sync
	logInfo("📤 Syncing...")
	runSSH(env, fmt.Sprintf("mkdir -p %s/data %s/migrations ~/.config/containers/systemd", env.Dir, env.Dir))

	binPath := fmt.Sprintf("%s/%s", env.Dir, cfg.BinaryName)
	runSSH(env, fmt.Sprintf("[ -f %s ] && cp %s %s.bak || true", binPath, binPath, binPath))

	artifacts := []string{}
	artifacts = append(artifacts, "build/"+cfg.BinaryName)
	if len(cfg.Artifacts.Include) > 0 {
		artifacts = append(artifacts, cfg.Artifacts.Include...)
	} else {
		artifacts = append(artifacts, "Dockerfile.vps", "migrations/", "files/")
	}

	runRsync(env, artifacts, fmt.Sprintf("%s@%s:%s/", env.User, env.Host, env.Dir), "--delete")

	if env.SyncEnvFile != "" {
		runRsync(env, []string{env.SyncEnvFile}, fmt.Sprintf("%s@%s:%s/.env", env.User, env.Host, env.Dir))
	}
	runRsync(env, []string{containerPath}, fmt.Sprintf("%s@%s:~/.config/containers/systemd/", env.User, env.Host))

	// Activate
	logInfo("🔄 Activating...")
	permCmd := "true"
	if env.Quadlet.ContainerUID > 0 && len(env.Quadlet.ChownVolumes) > 0 {
		var paths []string
		for _, p := range env.Quadlet.ChownVolumes {
			if strings.HasPrefix(p, "./") {
				p = fmt.Sprintf("%s/%s", strings.TrimRight(env.Dir, "/"), strings.TrimPrefix(p, "./"))
			}
			paths = append(paths, p)
		}
		if len(paths) > 0 {
			permCmd = fmt.Sprintf("podman unshare chown -R %d:%d %s", env.Quadlet.ContainerUID, env.Quadlet.ContainerGID, strings.Join(paths, " "))
		}
	}

	dockerfile := env.Quadlet.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile.vps"
	}

	// Activation Script
	script := strings.Join([]string{
		fmt.Sprintf("cd %s", env.Dir),
		fmt.Sprintf("podman build -f %s -t %s .", dockerfile, env.Quadlet.Image),
		permCmd,
		"systemctl --user daemon-reload",
		"mkdir -p ~/.config/systemd/user/default.target.wants",
		fmt.Sprintf("ln -sf /run/user/$(id -u)/systemd/generator/%s.service ~/.config/systemd/user/default.target.wants/%s.service", env.Quadlet.ServiceName, env.Quadlet.ServiceName),
		"systemctl --user daemon-reload",
		fmt.Sprintf("systemctl --user restart %s.service", env.Quadlet.ServiceName),
		fmt.Sprintf("sleep 2 && systemctl --user is-active %s.service", env.Quadlet.ServiceName),
	}, " && ")

	// --- FIX: Detailed logging & Diagnostics ---
	if err := runSSH(env, script); err != nil {
		logError("Activation failed: %v", err)
		rollback(env, binPath, dockerfile)
		logFatal("Deployment failed but successfully rolled back.")
	}

	// --- NEW: Application Health Check ---
	if env.Quadlet.HealthURL != "" {
		logInfo("🩺 Performing Application Health Check (%s)...", env.Quadlet.HealthURL)
		// Try for 30 seconds
		checkScript := fmt.Sprintf(`
			for i in {1..15}; do
				if curl -s -f "%s" > /dev/null; then
					echo "OK"
					exit 0
				fi
				sleep 2
			done
			echo "Health check timed out"
			exit 1
		`, env.Quadlet.HealthURL)

		if err := runSSH(env, checkScript); err != nil {
			logError("Health Check failed!")
			rollback(env, binPath, dockerfile)
			logFatal("Deployment failed (Unhealthy) but successfully rolled back.")
		}
	}

	logSuccess("✅ Deployed successfully.")
}

func rollback(env Environment, binPath, dockerfile string) {
	logWarn("🔍 Diagnosing with remote logs (last 50 lines)...")
	runSSHStream(env, fmt.Sprintf("journalctl --user -u %s.service -n 50 --no-pager", env.Quadlet.ServiceName))

	logWarn("🚨 INITIATING AUTOMATIC ROLLBACK...")
	rbScript := strings.Join([]string{
		fmt.Sprintf("cd %s", env.Dir),
		fmt.Sprintf("[ -f %s.bak ] && mv %s.bak %s", binPath, binPath, binPath),
		fmt.Sprintf("podman build -f %s -t %s .", dockerfile, env.Quadlet.Image),
		fmt.Sprintf("systemctl --user restart %s.service", env.Quadlet.ServiceName),
	}, " && ")
	if rbErr := runSSH(env, rbScript); rbErr != nil {
		logFatal("CRITICAL: Rollback failed! Error: %v", rbErr)
	}
}

// --- DB Operations ---

func doDBPull(envName string) {
	_, env := loadEnv(envName)
	if env.Database.Driver != "sqlite" {
		logFatal("Only sqlite supported")
	}

	local := filepath.Clean(env.Database.Source)
	remote := fmt.Sprintf("%s/%s", strings.TrimRight(env.Dir, "/"), env.Database.Source)

	logInfo("📥 Pulling DB from %s...", env.Host)

	// Backup Local DB if it exists
	if _, err := os.Stat(local); err == nil {
		if !confirm(fmt.Sprintf("Local file %s exists. Backup and overwrite?", local)) {
			return
		}
		backup := local + ".bak"
		logInfo("📦 Backing up local DB to %s...", backup)
		if err := copyFile(local, backup); err != nil {
			logFatal("Failed to backup local file: %v", err)
		}
	} else {
		if !confirm(fmt.Sprintf("Download to %s?", local)) {
			return
		}
	}

	if !dryRun {
		os.MkdirAll(filepath.Dir(local), 0755)
	}

	f, err := os.Create(local)
	if err != nil {
		logFatal("Failed to create local file: %v", err)
	}
	defer f.Close()

	// ROBUST STRATEGY:
	// 1. Create a temp directory
	// 2. Backup DB to a file in that temp dir (handles WAL mode safely)
	// 3. Cat the file to stdout (stream to us)
	// 4. Clean up
	remoteScript := fmt.Sprintf(`
		set -e
		TEMP_DIR=$(mktemp -d)
		trap "rm -rf $TEMP_DIR" EXIT
		if ! command -v sqlite3 &> /dev/null; then
            echo "sqlite3 not found on remote" >&2
            exit 1
        fi
		sqlite3 '%s' ".backup '$TEMP_DIR/backup.db'"
		cat "$TEMP_DIR/backup.db"
	`, remote)

	sshArgs := getSSHBaseArgs(env)
	sshArgs = append(sshArgs, remoteScript)

	cmd := exec.Command("ssh", sshArgs...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		f.Close()
		os.Remove(local) // Don't leave a 0-byte corrupted file
		logFatal("Pull failed: %v", err)
	}
	logSuccess("Synced to %s", local)
}

func doDBPush(envName string) {
	_, env := loadEnv(envName)
	local := filepath.Clean(env.Database.Source)
	remote := fmt.Sprintf("%s/%s", strings.TrimRight(env.Dir, "/"), env.Database.Source)

	logWarn("🔥 PUSHING DB to %s. Service will STOP.", envName)
	if !confirm("Sure?") {
		return
	}

	// 1. Stop Service
	logInfo("🛑 Stopping service...")
	if err := runSSH(env, fmt.Sprintf("systemctl --user stop %s.service", env.Quadlet.ServiceName)); err != nil {
		logFatal("Failed to stop service: %v", err)
	}

	// Use a cleanup/restore function block to ensure service starts even if rsync fails
	err := func() error {
		// 2. Permission Fix (if needed)
		if env.Quadlet.ContainerUID > 0 {
			logInfo("🔧 Reclaiming file permissions...")
			// Claim main DB and potential WAL files
			runSSH(env, fmt.Sprintf("podman unshare chown $(id -u):$(id -g) %s %s-wal %s-shm || true", remote, remote, remote))
		}

		// 3. Backup Remote
		logInfo("📦 Creating remote backup...")
		// Copy .db to .db.bak
		if err := runSSH(env, fmt.Sprintf("cp %s %s.bak || true", remote, remote)); err != nil {
			return fmt.Errorf("remote backup failed: %v", err)
		}
		// Remove old WAL/SHM to avoid corruption when new DB lands
		runSSH(env, fmt.Sprintf("rm -f %s-wal %s-shm", remote, remote))

		// 4. Upload
		logInfo("📤 Uploading...")
		if err := runRsyncSafe(env, []string{local}, fmt.Sprintf("%s@%s:%s", env.User, env.Host, remote)); err != nil {
			logError("Rsync failed: %v", err)
			logInfo("Restoring from backup...")
			runSSH(env, fmt.Sprintf("mv %s.bak %s", remote, remote))
			return err
		}

		return nil
	}()

	// 5. Restore Permissions
	if env.Quadlet.ContainerUID > 0 {
		logInfo("🔧 Restoring container permissions...")
		runSSH(env, fmt.Sprintf("podman unshare chown %d:%d %s %s.bak", env.Quadlet.ContainerUID, env.Quadlet.ContainerGID, remote, remote))
	}

	// 6. Start Service
	logInfo("▶️ Starting service...")
	runSSH(env, fmt.Sprintf("systemctl --user start %s.service", env.Quadlet.ServiceName))

	if err != nil {
		logFatal("Push failed: %v", err)
	}
	logSuccess("Pushed successfully.")
}

// --- Common Helpers ---

func loadEnv(envName string) (Config, Environment) {
	data, err := os.ReadFile("deploy.yaml")
	if err != nil {
		logFatal("Read error: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		logFatal("Parse error: %v", err)
	}
	env, ok := cfg.Environments[envName]
	if !ok {
		logFatal("Env %s not found", envName)
	}
	return cfg, env
}

func getBuildMetadata() BuildMetadata {
	get := func(args ...string) string {
		if dryRun {
			return "dry"
		}
		out, _ := exec.Command(args[0], args[1:]...).Output()
		return strings.TrimSpace(string(out))
	}
	return BuildMetadata{
		Version: get("git", "rev-parse", "--short", "HEAD"),
		Date:    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Tag:     get("git", "describe", "--tags", "--abbrev=0"),
		Commit:  get("git", "rev-parse", "HEAD"),
	}
}

func generateQuadlet(env Environment, outDir string) string {
	var absVolumes []string
	for _, vol := range env.Quadlet.Volumes {
		parts := strings.Split(vol, ":")
		if len(parts) > 0 && strings.HasPrefix(parts[0], "./") {
			rel := strings.TrimPrefix(parts[0], "./")
			abs := strings.TrimRight(env.Dir, "/") + "/" + rel
			parts[0] = abs
			absVolumes = append(absVolumes, strings.Join(parts, ":"))
		} else {
			absVolumes = append(absVolumes, vol)
		}
	}
	data := TemplateData{Quadlet: env.Quadlet, TargetDir: env.Dir}
	data.Quadlet.Volumes = absVolumes

	var buf bytes.Buffer
	t, _ := template.New("q").Parse(quadletTemplate)
	t.Execute(&buf, data)
	path := filepath.Join(outDir, env.Quadlet.ServiceName+".container")
	if !dryRun {
		os.WriteFile(path, buf.Bytes(), 0644)
	}
	return path
}

func generateTraefikLabels(serviceName string, r RouterConfig, defaultResolver string) []string {
	var labels []string
	if r.Host != "" && !r.Enabled {
	}
	if r.Host == "" && r.Rule == "" {
		return labels
	}

	labels = append(labels, "traefik.enable=true")
	rule := r.Rule
	if rule == "" {
		rule = fmt.Sprintf("Host(`%s`)", r.Host)
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.rule=%s", serviceName, rule))

	eps := r.EntryPoints
	if len(eps) == 0 {
		eps = []string{"websecure"}
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.entrypoints=%s", serviceName, strings.Join(eps, ",")))

	resolver := r.CertResolver
	if resolver == "" {
		resolver = defaultResolver
	}
	if resolver == "" {
		resolver = "myresolver"
	}
	labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.tls.certresolver=%s", serviceName, resolver))

	var mws []string
	if r.StripPrefix && r.PathPrefix != "" {
		mw := serviceName + "-strip"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.stripprefix.prefixes=%s", mw, r.PathPrefix))
		mws = append(mws, mw)
	}
	if len(r.BasicAuth) > 0 {
		mw := serviceName + "-auth"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.basicauth.users=%s", mw, strings.Join(r.BasicAuth, ",")))
		mws = append(mws, mw)
	}
	if r.BasicAuthFile != "" {
		mw := serviceName + "-authfile"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.basicauth.usersfile=%s", mw, r.BasicAuthFile))
		mws = append(mws, mw)
	}
	if len(r.IPAllowList) > 0 {
		mw := serviceName + "-ip"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ipallowlist.sourcerange=%s", mw, strings.Join(r.IPAllowList, ",")))
		mws = append(mws, mw)
	}
	if r.RateLimit != nil && r.RateLimit.Average > 0 {
		mw := serviceName + "-rate"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ratelimit.average=%d", mw, r.RateLimit.Average))
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.ratelimit.burst=%d", mw, r.RateLimit.Burst))
		mws = append(mws, mw)
	}
	if r.Compress {
		mw := serviceName + "-compress"
		labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.compress=true", mw))
		mws = append(mws, mw)
	}
	if len(r.Headers) > 0 {
		mw := serviceName + "-headers"
		for k, v := range r.Headers {
			labels = append(labels, fmt.Sprintf("traefik.http.middlewares.%s.headers.customrequestheaders.%s=%s", mw, k, v))
		}
		mws = append(mws, mw)
	}
	if len(mws) > 0 {
		labels = append(labels, fmt.Sprintf("traefik.http.routers.%s.middlewares=%s", serviceName, strings.Join(mws, ",")))
	}

	port := r.InternalPort
	if port == 0 {
		port = 8080
	}
	labels = append(labels, fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port=%d", serviceName, port))
	return labels
}

func doInit() {
	if _, err := os.Stat("deploy.yaml"); err == nil {
		logFatal("deploy.yaml exists")
	}
	os.WriteFile("deploy.yaml", []byte(getDefaultConfig()), 0644)
	logSuccess("Created deploy.yaml")
}

func doLogs(envName string, usePodman bool) {
	_, env := loadEnv(envName)
	cmd := fmt.Sprintf("journalctl --user -u %s.service -f", env.Quadlet.ServiceName)
	if usePodman {
		cmd = fmt.Sprintf("podman logs -f systemd-%s", env.Quadlet.ServiceName)
	}
	logInfo("Streaming logs...")

	sshArgs := getSSHBaseArgs(env)
	sshArgs = append(sshArgs, "-t", cmd) // -t for tty

	c := exec.Command("ssh", sshArgs...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	c.Run()
}

func getCmdOutput(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out))
}

func genFile(path string, tmplStr string, data any) {
	if dryRun {
		return
	}
	t, _ := template.New("t").Parse(tmplStr)
	f, _ := os.Create(path)
	defer f.Close()
	t.Execute(f, data)
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

func runCommand(desc string, cmd *exec.Cmd) error {
	if dryRun {
		logDebug("[DRY] %s", cmd.String())
		return nil
	}
	if verbose {
		logDebug("[EXEC] %s", cmd.String())
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		// FIX: Capture both stdout and stderr for error context!
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			// Combine output for useful error message
			return fmt.Errorf("%s\nSTDOUT:\n%s\nSTDERR:\n%s", err, outBuf.String(), errBuf.String())
		}
		return nil
	}
	return cmd.Run()
}

func runCommandRaw(name string, args ...string) error {
	if dryRun {
		fmt.Printf("[DRY] %s %v\n", name, args)
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runRsync(env Environment, sources []string, dest string, extraArgs ...string) {
	if err := runRsyncSafe(env, sources, dest, extraArgs...); err != nil {
		logFatal("Rsync failed: %v", err)
	}
}

func runRsyncSafe(env Environment, sources []string, dest string, extraArgs ...string) error {
	args := []string{"-avz"}

	sshCmd := "ssh"
	// IMPROVEMENT: Reuse the multiplexed socket for rsync too!
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("deploy-%s-%s", env.User, env.Host))
	sshCmd += fmt.Sprintf(" -o ControlMaster=auto -o ControlPath=%s -o ControlPersist=5m", socketPath)

	needsE := false
	if env.Port != 0 && env.Port != 22 {
		sshCmd += fmt.Sprintf(" -p %d", env.Port)
		needsE = true
	}
	if env.SSHKey != "" {
		sshCmd += fmt.Sprintf(" -i %s", env.SSHKey)
		needsE = true
	}
	// Always use -e to inject options (or just to be safe)
	if !needsE {
		args = append(args, "-e", sshCmd)
	} else {
		args = append(args, "-e", sshCmd)
	}

	args = append(args, extraArgs...)
	args = append(args, sources...)
	args = append(args, dest)

	return runCommandRaw("rsync", args...)
}

func getSSHBaseArgs(env Environment) []string {
	args := []string{}
	// IMPROVEMENT: SSH Multiplexing
	// Create a socket in /tmp/deploy-<user>-<host>
	// - ControlMaster=auto: Try to use existing master, or become one.
	// - ControlPersist=5m: Keep master connection open for 5 mins after last command.
	// - ControlPath: Path to the socket.
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("deploy-%s-%s", env.User, env.Host))

	args = append(args, "-o", "ControlMaster=auto")
	args = append(args, "-o", "ControlPersist=5m")
	args = append(args, "-o", fmt.Sprintf("ControlPath=%s", socketPath))

	if env.SSHKey != "" {
		args = append(args, "-i", env.SSHKey)
	}
	args = append(args, "-p", fmt.Sprintf("%d", env.Port))
	args = append(args, fmt.Sprintf("%s@%s", env.User, env.Host))
	return args
}

func runSSH(env Environment, cmd string) error {
	args := getSSHBaseArgs(env)
	args = append(args, cmd)

	if dryRun {
		logDebug("[SSH] %s", cmd)
		return nil
	}

	// Use verbose flag inside runCommand, not hardcoded behavior here.
	// We reconstruct the exec.Cmd here.
	c := exec.Command("ssh", args...)
	return runCommand("SSH", c)
}

func runSSHStream(env Environment, cmd string) error {
	args := getSSHBaseArgs(env)
	args = append(args, cmd)
	if dryRun {
		logDebug("[SSH-STREAM] %s", cmd)
		return nil
	}

	c := exec.Command("ssh", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func runSSHWithRetry(env Environment, cmd string, attempts int) error {
	args := getSSHBaseArgs(env)
	args = append(args, cmd)

	var err error
	for i := 0; i < attempts; i++ {
		// We use a fresh command struct each time
		if err = runCommand("SSH", exec.Command("ssh", args...)); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(1 * time.Second)
		}
	}
	return err
}

func confirm(prompt string) bool {
	if dryRun {
		return true
	}
	fmt.Printf("%s [y/N]: ", prompt)
	r := bufio.NewReader(os.Stdin)
	res, _ := r.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(res)) == "y"
}

func fetchLatestGitHubRelease(repo string) (string, error) {
	resp, err := http.Get(fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.TagName, nil
}

func logFatal(f string, a ...any)   { fmt.Printf(Red+"[FATAL] "+Reset+f+"\n", a...); os.Exit(1) }
func logInfo(f string, a ...any)    { fmt.Printf(Blue+"[INFO] "+Reset+f+"\n", a...) }
func logSuccess(f string, a ...any) { fmt.Printf(Green+"[DONE] "+Reset+f+"\n", a...) }
func logWarn(f string, a ...any)    { fmt.Printf(Yellow+"[WARN] "+Reset+f+"\n", a...) }
func logError(f string, a ...any)   { fmt.Printf(Red+"[ERR] "+Reset+f+"\n", a...) }
func logDebug(f string, a ...any) {
	if verbose {
		fmt.Printf(Gray+f+Reset+"\n", a...)
	}
}
