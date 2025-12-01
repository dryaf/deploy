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
	Traefik     TraefikConfig     `yaml:"traefik"`
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

	// Merge Global Maintenance Defaults into Environment
	// We only overwrite if the Environment value is empty/zero
	if env.Maintenance.Title == "" {
		env.Maintenance.Title = cfg.Maintenance.Title
	}
	if env.Maintenance.Text == "" {
		env.Maintenance.Text = cfg.Maintenance.Text
	}
	// Note: We don't merge 'Enabled' strictly because false is a valid setting.
	// However, since the CLI command ignores 'Enabled' and forces deploy,
	// checking Title/Text is sufficient for templates.

	return cfg, env
}
