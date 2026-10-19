package main

import (
	"crypto/sha256"
	_ "crypto/sha512"
	"fmt"
	"io"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/distribution/reference"
)

type DeploymentMode string

const (
	ModeBinary DeploymentMode = "binary"
	ModeImage  DeploymentMode = "image"
)

type DeploymentConfig struct {
	Mode DeploymentMode `yaml:"mode"`
}

type NormalizedConfig struct {
	Config        Config
	Env           Environment
	Mode          DeploymentMode
	EnvFile       string
	Routing       bool
	CPUQuota      string
	HealthTimeout time.Duration
}

var servicePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.:-]*$`)
var userPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]*[$]?$`)
var memoryPattern = regexp.MustCompile(`^([1-9][0-9]*)([KMGTPE]([iI]?[bB])?|[bB])?$`)

func normalizeAndValidateConfig(cfg Config, envName string) (NormalizedConfig, error) {
	n := NormalizedConfig{Config: cfg, Mode: cfg.Deployment.Mode}
	if n.Mode == "" {
		n.Mode = ModeBinary
	}
	if n.Mode != ModeBinary && n.Mode != ModeImage {
		return n, fmt.Errorf("deployment.mode must be binary or image")
	}
	env, ok := cfg.Environments[envName]
	if !ok {
		return n, fmt.Errorf("environment %q not found", envName)
	}
	n.Env = env
	n.Env.Name = envName
	if env.Database != (DatabaseConfig{}) {
		if _, _, err := databasePaths(n.Env); err != nil {
			return n, err
		}
		if env.Database.Rollback != "" && env.Database.Rollback != "manual" && env.Database.Rollback != "restore" {
			return n, fmt.Errorf("database.rollback must be manual or restore")
		}
		backupDir := path.Join(env.Dir, path.Dir(env.Database.Source), "backup")
		for _, owned := range env.Quadlet.ChownVolumes {
			if !path.IsAbs(owned) {
				owned = path.Join(env.Dir, owned)
			}
			owned = path.Clean(owned)
			if owned == backupDir || strings.HasPrefix(owned, backupDir+"/") {
				return n, fmt.Errorf("chown_volumes cannot target the database backup directory")
			}
		}
	}
	q := &n.Env.Quadlet
	if cfg.AppName == "" {
		return n, fmt.Errorf("app_name is required")
	}
	if !hostPattern.MatchString(env.Host) || !userPattern.MatchString(env.User) {
		return n, fmt.Errorf("invalid SSH host or user")
	}
	if env.Port == 0 {
		n.Env.Port = 22
	} else if env.Port < 1 || env.Port > 65535 {
		return n, fmt.Errorf("invalid ssh_port")
	}
	if !path.IsAbs(env.Dir) || path.Clean(env.Dir) == "/" || strings.Contains(env.Dir, "%") {
		return n, fmt.Errorf("target_dir must be an absolute project path without systemd specifiers")
	}
	n.Env.Dir = path.Clean(env.Dir)
	if !servicePattern.MatchString(q.ServiceName) || strings.Contains(q.ServiceName, "..") {
		return n, fmt.Errorf("invalid service_name")
	}
	if q.ContainerUID < 0 || q.ContainerGID < 0 {
		return n, fmt.Errorf("container UID and GID must be nonnegative")
	}
	if strings.HasSuffix(q.Network, ".network") && !servicePattern.MatchString(q.Network) {
		return n, fmt.Errorf("invalid network Quadlet name")
	}
	for _, a := range q.NetworkAliases {
		if !aliasPattern.MatchString(a) {
			return n, fmt.Errorf("invalid network alias %q", a)
		}
	}
	if strings.ContainsAny(q.Image, " \t\r\n\\\"'`$%") {
		return n, fmt.Errorf("invalid image reference characters")
	}
	if q.Image == "" {
		return n, fmt.Errorf("quadlet.image is required")
	}
	if n.Mode == ModeImage {
		if cfg.BinaryName != "" || cfg.Build != (BuildConfig{}) || q.Dockerfile != "" {
			return n, fmt.Errorf("image mode does not accept binary_name, build options or quadlet.dockerfile")
		}
		if err := validateImageReference(q.Image); err != nil {
			return n, err
		}
		if len(q.ChownVolumes) != 0 {
			return n, fmt.Errorf("image mode does not change ownership of mounted data; manage it separately")
		}
		n.HealthTimeout = 60 * time.Second
	} else {
		if !servicePattern.MatchString(cfg.BinaryName) {
			return n, fmt.Errorf("binary_name is required and must be a filename")
		}
		if q.Dockerfile == "" {
			q.Dockerfile = "Dockerfile.vps"
		}
		n.HealthTimeout = 30 * time.Second
	}
	if q.HealthTimeout != "" {
		d, err := time.ParseDuration(q.HealthTimeout)
		if err != nil || d <= 0 || d > 24*time.Hour {
			return n, fmt.Errorf("health_timeout must be a positive duration of at most 24h")
		}
		n.HealthTimeout = d
	}
	if q.HealthURL != "" {
		u, err := url.Parse(q.HealthURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return n, fmt.Errorf("health_url must be an HTTP(S) URL without credentials")
		}
	}
	n.Routing = routerEnabled(q.Router)
	if q.Router.Enabled != nil && *q.Router.Enabled && q.Router.Domain == "" && q.Router.Host == "" && q.Router.Rule == "" {
		return n, fmt.Errorf("router.enabled requires domain, host or rule")
	}
	if q.Router.InternalPort < 0 || q.Router.InternalPort > 65535 {
		return n, fmt.Errorf("invalid router.internal_port")
	}
	if q.EnvFile == nil {
		if n.Mode == ModeBinary || env.SyncEnvFile != "" {
			n.EnvFile = path.Join(n.Env.Dir, ".env")
		}
	} else if *q.EnvFile != "" {
		n.EnvFile = *q.EnvFile
		if !path.IsAbs(n.EnvFile) {
			n.EnvFile = path.Join(n.Env.Dir, n.EnvFile)
		}
		n.EnvFile = path.Clean(n.EnvFile)
	}
	if strings.Contains(n.EnvFile, "%") {
		return n, fmt.Errorf("env_file cannot contain systemd specifiers")
	}
	if env.SyncEnvFile != "" && n.EnvFile != path.Join(n.Env.Dir, ".env") {
		return n, fmt.Errorf("sync_env_file requires quadlet.env_file to resolve to <target_dir>/.env")
	}
	if q.CPU != "" {
		value := strings.TrimSuffix(q.CPU, "%")
		cpu, err := strconv.ParseFloat(value, 64)
		if err != nil || cpu <= 0 || cpu > 1000000 || strings.ContainsAny(value, "eENnaA+ ") {
			return n, fmt.Errorf("cpu must be a positive CPU count or percentage")
		}
		if !strings.HasSuffix(q.CPU, "%") {
			cpu *= 100
		}
		n.CPUQuota = strconv.FormatFloat(cpu, 'f', -1, 64) + "%"
	}
	if q.Memory != "" {
		memory, err := normalizeMemory(q.Memory)
		if err != nil {
			return n, err
		}
		q.Memory = memory
	}
	// Commands are intentional code. All other runtime values must fit on one unit line.
	if err := validateSingleLines(reflect.ValueOf(n.Env), "environment"); err != nil {
		return n, err
	}
	if err := validateSingleLines(reflect.ValueOf(cfg.Artifacts), "artifacts"); err != nil {
		return n, err
	}
	if err := validateSingleLines(reflect.ValueOf(cfg.AppName), "app_name"); err != nil {
		return n, err
	}
	if n.Mode == ModeBinary {
		if err := validateArtifactPath(q.Dockerfile); err != nil {
			return n, fmt.Errorf("dockerfile: %w", err)
		}
		for name, text := range map[string]string{"build.cmd": cfg.Build.Cmd, "build.ldflags": cfg.Build.Ldflags} {
			tmpl, err := template.New(name).Parse(text)
			if err != nil {
				return n, fmt.Errorf("%s template: %w", name, err)
			}
			if err := tmpl.Execute(io.Discard, BuildMetadata{}); err != nil {
				return n, fmt.Errorf("%s template: %w", name, err)
			}
		}
	}
	for i, p := range cfg.Artifacts.Include {
		if err := validateArtifactPath(p); err != nil {
			return n, err
		}
		p = path.Clean(p)
		if env.SyncEnvFile != "" && (p == ".env" || strings.HasPrefix(p, ".env/")) {
			return n, fmt.Errorf("artifact overlaps synchronized .env")
		}
		for _, other := range cfg.Artifacts.Include[:i] {
			other = path.Clean(other)
			if p == other || strings.HasPrefix(p, other+"/") || strings.HasPrefix(other, p+"/") {
				return n, fmt.Errorf("overlapping artifact paths")
			}
		}
	}
	if _, err := mergeLabels(q.Labels, generateTraefikLabels(q.ServiceName, q.Router, "myresolver")); err != nil {
		return n, err
	}
	if n.Env.Maintenance.Title == "" {
		n.Env.Maintenance.Title = cfg.Maintenance.Title
	}
	if n.Env.Maintenance.Text == "" {
		n.Env.Maintenance.Text = cfg.Maintenance.Text
	}
	n.Config.Deployment.Mode = n.Mode
	return n, nil
}

// systemd accepts a single binary size suffix, not MB or MiB.
func normalizeMemory(value string) (string, error) {
	m := memoryPattern.FindStringSubmatch(value)
	if m == nil {
		return "", fmt.Errorf("invalid memory limit")
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	suffix := strings.ToUpper(m[2])
	shift := uint(0)
	if suffix != "" && suffix != "B" {
		shift = uint(strings.IndexByte("KMGTPE", suffix[0])+1) * 10
		suffix = suffix[:1]
	}
	if err != nil || n > uint64(1<<63-1)>>shift {
		return "", fmt.Errorf("memory limit exceeds supported byte range")
	}
	return m[1] + suffix, nil
}

func validateImageReference(s string) error {
	// Parse the original input, without Docker Hub short-name normalization.
	r, err := reference.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid fully qualified image reference")
	}
	named, ok := r.(reference.Named)
	if !ok {
		return fmt.Errorf("image reference must include registry and repository")
	}
	domain, rest, qualified := strings.Cut(named.Name(), "/")
	if !qualified || rest == "" || !(strings.ContainsAny(domain, ".:") || domain == "localhost") {
		return fmt.Errorf("image reference must explicitly include a registry")
	}
	if _, tag := r.(reference.Tagged); !tag {
		if _, digest := r.(reference.Digested); !digest {
			return fmt.Errorf("image reference requires an explicit tag or digest")
		}
	}
	if strings.ContainsAny(s, "\r\n\t <>\\\"'$") || strings.Contains(s, "://") {
		return fmt.Errorf("invalid image reference")
	}
	return nil
}

func validateArtifactPath(p string) error {
	clean := path.Clean(p)
	if p == "" || path.IsAbs(p) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(p, "\\") || strings.ContainsAny(p, "\n\r\x00") {
		return fmt.Errorf("artifact must be a relative project path: %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == ".deploy" {
			return fmt.Errorf("unsafe artifact path %q", p)
		}
	}
	return nil
}

func validateSingleLines(v reflect.Value, field string) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return validateSingleLines(v.Elem(), field)
	}
	switch v.Kind() {
	case reflect.String:
		if strings.IndexFunc(v.String(), func(r rune) bool { return unicode.IsControl(r) }) >= 0 || strings.HasSuffix(v.String(), "\\") {
			return fmt.Errorf("%s contains control characters or a unit continuation", field)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := validateSingleLines(v.Field(i), field+"."+v.Type().Field(i).Name); err != nil {
				return err
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := validateSingleLines(v.Index(i), field); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if err := validateSingleLines(k, field); err != nil {
				return err
			}
			if err := validateSingleLines(v.MapIndex(k), field); err != nil {
				return err
			}
		}
	}
	return nil
}

func routerEnabled(r RouterConfig) bool {
	if r.Enabled != nil {
		return *r.Enabled
	}
	return r.Domain != "" || r.Host != "" || r.Rule != ""
}
func mergeLabels(custom, generated []string) ([]string, error) {
	out := []string{}
	values := map[string]string{}
	for _, group := range [][]string{custom, generated} {
		for _, l := range group {
			k, v, ok := strings.Cut(l, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("label must be key=value")
			}
			if old, exists := values[k]; exists {
				if old != v {
					return nil, fmt.Errorf("conflicting label %s", k)
				}
				continue
			}
			values[k] = v
			out = append(out, l)
		}
	}
	return out, nil
}

func serviceLockName(service string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(service))) }
