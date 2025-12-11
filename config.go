package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	AppName      string                 `yaml:"app_name"`
	BinaryName   string                 `yaml:"binary_name"`
	Build        BuildConfig            `yaml:"build"`
	Artifacts    ArtifactsConfig        `yaml:"artifacts"`
	Maintenance  MaintenanceConfig      `yaml:"maintenance"` // Global Default
	Environments map[string]Environment `yaml:"environments"`
}

type ServerConfig struct {
	Host    string      `yaml:"host"`
	User    string      `yaml:"user"`
	SSHPort int         `yaml:"ssh_port"`
	SSHKey  string      `yaml:"ssh_key"`
	Stack   ServerStack `yaml:"stack"`
}

type ServerStack struct {
	Traefik    TraefikStack     `yaml:"traefik"`
	Authelia   AutheliaConfig   `yaml:"authelia"`
	Watchtower WatchtowerConfig `yaml:"watchtower"`
}

type TraefikStack struct {
	Version     string     `yaml:"version"`
	Email       string     `yaml:"email"`
	Dashboard   bool       `yaml:"dashboard"`
	NetworkName string     `yaml:"network_name"`
	Auth        AuthConfig `yaml:"auth"` // Global Auth
}

type AuthConfig struct {
	Provider string `yaml:"provider"` // "basic" or "authelia"
}

type AutheliaConfig struct {
	Subdomain string `yaml:"subdomain"`
	UsersFile string `yaml:"users_file"`
	// We can add SMTP, etc later as needed, keeping it simple for now
}

type WatchtowerConfig struct {
	Schedule string `yaml:"schedule"`
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
	Host        string            `yaml:"host"`
	User        string            `yaml:"user"`
	Port        int               `yaml:"ssh_port"`
	SSHKey      string            `yaml:"ssh_key"`
	Dir         string            `yaml:"target_dir"`
	SyncEnvFile string            `yaml:"sync_env_file"`
	Quadlet     Quadlet           `yaml:"quadlet"`
	Maintenance MaintenanceConfig `yaml:"maintenance"` // Env Override
	Database    DatabaseConfig    `yaml:"database"`
	// Traefik config removed from here, now in ServerConfig
}

type MaintenanceConfig struct {
	Enabled bool   `yaml:"enabled"`
	Title   string `yaml:"title"`
	Text    string `yaml:"text"`
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
	Enabled       bool     `yaml:"enabled"`
	Domain        string   `yaml:"domain"` // Replaces Host/Rule simplicity
	Host          string   `yaml:"host"`   // Legacy support
	Rule          string   `yaml:"rule"`
	InternalPort  int      `yaml:"internal_port"`
	EntryPoints   []string `yaml:"entrypoints"`
	CertResolver  string   `yaml:"cert_resolver"`
	HTTPSRedirect bool     `yaml:"https_redirect"`
	PathPrefix    string   `yaml:"path_prefix"`
	StripPrefix   bool     `yaml:"strip_prefix"`
	Compress      bool     `yaml:"compress"`
	Auth          bool     `yaml:"auth"` // Boolean intent

	// Legacy Header/RateLimit support kept for power users
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
	ServiceName  string       `yaml:"service_name"`
	Description  string       `yaml:"description"`
	Image        string       `yaml:"image"`
	Network      string       `yaml:"network"`
	Labels       []string     `yaml:"labels"`
	Router       RouterConfig `yaml:"router"`
	Volumes      []string     `yaml:"volumes"`
	EnvVars      []string     `yaml:"env_vars"`
	Ports        []string     `yaml:"ports"`
	AutoRestart  bool         `yaml:"auto_restart"`
	StopOnDeploy bool         `yaml:"stop_on_deploy"`
	Timezone     string       `yaml:"timezone"`
	Memory       string       `yaml:"memory"`
	CPU          string       `yaml:"cpu"`
	ReadOnly     bool         `yaml:"read_only"`
	HealthCmd    string       `yaml:"health_cmd"`
	HealthURL    string       `yaml:"health_url"`
	PodmanArgs   []string     `yaml:"podman_args"`
	Exec         string       `yaml:"exec"`
	Dockerfile   string       `yaml:"dockerfile"`

	ContainerUID int      `yaml:"container_uid"`
	ContainerGID int      `yaml:"container_gid"`
	ChownVolumes []string `yaml:"chown_volumes"`
}

type BuildMetadata struct {
	Version     string
	Commit      string
	Date        string
	Tag         string
	MainVersion string
	GoVersion   string
}

func loadConfig() Config {
	data, err := os.ReadFile("deploy.yaml")
	if err != nil {
		logFatal("Read error: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		logFatal("Parse error: %v", err)
	}
	return cfg
}

func loadServerConfig() ServerConfig {
	data, err := os.ReadFile("server.yaml")
	if err != nil {
		logFatal("Read error (server.yaml): %v", err)
	}
	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		logFatal("Parse error (server.yaml): %v", err)
	}
	// Defaults
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 22
	}
	if cfg.User == "" {
		cfg.User = "root"
	}
	return cfg
}

func loadEnv(envName string) (Config, Environment) {
	cfg := loadConfig()
	env, ok := cfg.Environments[envName]
	if !ok {
		logFatal("Env %s not found", envName)
	}

	// Merge Global Maintenance Defaults into Environment
	if env.Maintenance.Title == "" {
		env.Maintenance.Title = cfg.Maintenance.Title
	}
	if env.Maintenance.Text == "" {
		env.Maintenance.Text = cfg.Maintenance.Text
	}

	return cfg, env
}
