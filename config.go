package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

type Config struct {
	AppName      string                 `yaml:"app_name"`
	Deployment   DeploymentConfig       `yaml:"deployment"`
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
	Name           string            `yaml:"-"`
	Host           string            `yaml:"host"`
	User           string            `yaml:"user"`
	Port           int               `yaml:"ssh_port"`
	SSHKey         string            `yaml:"ssh_key"`
	KnownHostsFile string            `yaml:"ssh_known_hosts"`
	Dir            string            `yaml:"target_dir"`
	SyncEnvFile    string            `yaml:"sync_env_file"`
	Quadlet        Quadlet           `yaml:"quadlet"`
	Maintenance    MaintenanceConfig `yaml:"maintenance"` // Env Override
	Database       DatabaseConfig    `yaml:"database"`
	Timeouts       TimeoutConfig     `yaml:"timeouts"`
	Storage        StorageConfig     `yaml:"storage"`
	// Traefik config removed from here, now in ServerConfig
}

type MaintenanceConfig struct {
	Enabled bool   `yaml:"enabled"`
	Title   string `yaml:"title"`
	Text    string `yaml:"text"`
}

type DatabaseConfig struct {
	Driver    string          `yaml:"driver"`
	Source    string          `yaml:"source"`
	Rollback  string          `yaml:"rollback"` // manual (default), or restore with possible loss of candidate writes
	Retention RetentionConfig `yaml:"retention"`
	OffHost   OffHostConfig   `yaml:"off_host"`
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
	Enabled       *bool    `yaml:"enabled"`
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
	ServiceName     string       `yaml:"service_name"`
	Description     string       `yaml:"description"`
	Image           string       `yaml:"image"`
	Network         string       `yaml:"network"`
	NetworkAliases  []string     `yaml:"network_aliases"`
	EnvFile         *string      `yaml:"env_file"`
	Labels          []string     `yaml:"labels"`
	Router          RouterConfig `yaml:"router"`
	Volumes         []string     `yaml:"volumes"`
	EnvVars         []string     `yaml:"env_vars"`
	Ports           []string     `yaml:"ports"`
	AutoRestart     bool         `yaml:"auto_restart"`
	StopOnDeploy    bool         `yaml:"stop_on_deploy"`
	Timezone        string       `yaml:"timezone"`
	Memory          string       `yaml:"memory"`
	CPU             string       `yaml:"cpu"`
	ReadOnly        bool         `yaml:"read_only"`
	HealthCmd       string       `yaml:"health_cmd"`
	HealthURL       string       `yaml:"health_url"`
	HealthTimeout   string       `yaml:"health_timeout"`
	HealthRequired  bool         `yaml:"health_required"`
	HealthStableFor string       `yaml:"health_stable_for"`
	HealthInterval  string       `yaml:"health_interval"`
	HealthSuccesses int          `yaml:"health_successes"`
	PodmanArgs      []string     `yaml:"podman_args"`
	Exec            string       `yaml:"exec"`
	Dockerfile      string       `yaml:"dockerfile"`

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

func readConfig(file string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(file)
	if err != nil {
		return cfg, err
	}
	if err := decodeConfig(file, data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func loadConfig() Config {
	cfg, err := readConfig("deploy.yaml")
	if err != nil {
		logFatal("Config: %v", err)
	}
	return cfg
}

func loadServerConfig() ServerConfig {
	cfg, err := readServerConfig("server.yaml")
	if err != nil {
		logFatal("Config: %v", err)
	}
	return cfg
}

func readServerConfig(file string) (ServerConfig, error) {
	var cfg ServerConfig
	data, err := os.ReadFile(file)
	if err != nil {
		return cfg, err
	}
	if err := decodeConfig(file, data, &cfg); err != nil {
		return cfg, err
	}
	// Defaults
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 22
	}
	if cfg.User == "" {
		cfg.User = "root"
	}
	if !hostPattern.MatchString(cfg.Host) || !userPattern.MatchString(cfg.User) || cfg.SSHPort < 1 || cfg.SSHPort > 65535 {
		return cfg, fmt.Errorf("%s: invalid SSH host, user or port", file)
	}
	return cfg, nil
}

// A second decode is required: Decoder.Decode otherwise accepts trailing YAML.
var yamlTypeValuePattern = regexp.MustCompile(`(?s)cannot unmarshal .* into`)

func decodeConfig(file string, data []byte, destination any) error {
	var document yaml.Node
	check := yaml.NewDecoder(bytes.NewReader(data))
	if err := check.Decode(&document); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if len(document.Content) == 0 || document.Content[0].Tag == "!!null" {
		return fmt.Errorf("%s: configuration must be a nonempty mapping", file)
	}
	if document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: configuration must be a mapping", file)
	}
	var extra yaml.Node
	if err := check.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s: expected exactly one YAML document", file)
	}
	if _, ok := destination.(*Config); ok {
		root := document.Content[0]
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value != "environments" {
				continue
			}
			envs := root.Content[i+1]
			for j := 0; j+1 < len(envs.Content); j += 2 {
				env := envs.Content[j+1]
				for k := 0; k+1 < len(env.Content); k += 2 {
					if env.Content[k].Value == "traefik" {
						return fmt.Errorf("%s: line %d: environments.%s.traefik was previously ignored; move infrastructure settings to server.yaml stack.traefik and application network/router settings to quadlet", file, env.Content[k].Line, envs.Content[j].Value)
					}
				}
			}
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(destination); err != nil {
		if typed, ok := err.(*yaml.TypeError); ok {
			redacted := make([]string, len(typed.Errors))
			for i, message := range typed.Errors {
				redacted[i] = yamlTypeValuePattern.ReplaceAllString(message, "value has incompatible type for")
			}
			err = &yaml.TypeError{Errors: redacted}
		}
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

func validateConfiguration(env string, server bool) error {
	if server {
		_, err := readServerConfig("server.yaml")
		return err
	}
	cfg, err := readConfig("deploy.yaml")
	if err != nil {
		return err
	}
	n, err := normalizeAndValidateConfig(cfg, env)
	if err != nil {
		return err
	}
	_, err = renderQuadlet(n, "")
	return err
}

func loadEnv(envName string) (Config, Environment) {
	cfg := loadConfig()
	n, err := normalizeAndValidateConfig(cfg, envName)
	if err != nil {
		logFatal("Config: %v", err)
	}
	return n.Config, n.Env
}
