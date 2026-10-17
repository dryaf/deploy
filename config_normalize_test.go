package main

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func imageConfig() Config {
	return Config{AppName: "pdf", Deployment: DeploymentConfig{Mode: ModeImage}, Environments: map[string]Environment{"prod": {Host: "vps.example.com", User: "sal", Dir: "/srv/pdf", Quadlet: Quadlet{ServiceName: "pdf", Image: "docker.io/gotenberg/gotenberg:8-chromium"}}}}
}
func ptr[T any](v T) *T { return &v }
func changeEnv(c *Config, f func(*Environment)) {
	e := c.Environments["prod"]
	f(&e)
	c.Environments["prod"] = e
}

func TestConfigDefaults(t *testing.T) {
	for _, mode := range []DeploymentMode{ModeImage, ModeBinary, ""} {
		c := imageConfig()
		c.Deployment.Mode = mode
		if mode != ModeImage {
			c.BinaryName = "server"
		}
		n, err := normalizeAndValidateConfig(c, "prod")
		if err != nil {
			t.Fatal(err)
		}
		if n.Env.Port != 22 {
			t.Fatal(n.Env.Port)
		}
		if mode == ModeImage {
			if n.EnvFile != "" || n.HealthTimeout != 60*time.Second {
				t.Fatal(n)
			}
		} else {
			if n.Mode != ModeBinary || n.EnvFile != "/srv/pdf/.env" || n.Env.Quadlet.Dockerfile != "Dockerfile.vps" || n.HealthTimeout != 30*time.Second {
				t.Fatal(n)
			}
		}
	}
}
func TestOptionalEnvFile(t *testing.T) {
	cases := []struct {
		name string
		mode DeploymentMode
		file *string
		sync string
		want string
		bad  bool
	}{
		{"image absent", ModeImage, nil, "", "", false}, {"image sync", ModeImage, nil, ".env.prod", "/srv/pdf/.env", false},
		{"binary absent", ModeBinary, nil, "", "/srv/pdf/.env", false}, {"binary empty", ModeBinary, ptr(""), "", "", false},
		{"relative", ModeImage, ptr("conf/env"), "", "/srv/pdf/conf/env", false}, {"absolute", ModeImage, ptr("/etc/pdf/env"), "", "/etc/pdf/env", false},
		{"sync empty", ModeImage, ptr(""), ".env", "", true}, {"sync mismatch", ModeImage, ptr("other"), ".env", "", true}, {"sync match", ModeImage, ptr(".env"), ".env", "/srv/pdf/.env", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := imageConfig()
			c.Deployment.Mode = tt.mode
			if tt.mode == ModeBinary {
				c.BinaryName = "server"
			}
			changeEnv(&c, func(e *Environment) { e.SyncEnvFile = tt.sync; e.Quadlet.EnvFile = tt.file })
			n, err := normalizeAndValidateConfig(c, "prod")
			if (err != nil) != tt.bad {
				t.Fatal(err)
			}
			if !tt.bad && n.EnvFile != tt.want {
				t.Fatalf("got %q want %q", n.EnvFile, tt.want)
			}
		})
	}
}
func TestInvalidConfig(t *testing.T) {
	cases := map[string]func(*Config){
		"mode": func(c *Config) { c.Deployment.Mode = "container" }, "build": func(c *Config) { c.Build.Arch = "amd64" }, "binary": func(c *Config) { c.BinaryName = "fake" },
		"dockerfile":  func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.Dockerfile = "Dockerfile" }) },
		"empty image": func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.Image = "" }) },
		"short":       func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.Image = "nginx:1" }) },
		"untagged":    func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.Image = "docker.io/library/nginx" }) },
		"credentials": func(c *Config) {
			changeEnv(c, func(e *Environment) { e.Quadlet.Image = "user:secret@registry.io/foo:1" })
		},
		"duration":     func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.HealthTimeout = "0s" }) },
		"cpu":          func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.CPU = "NaN" }) },
		"newline":      func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.Description = "bad\nExecStart=evil" }) },
		"continuation": func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.Exec = "bad\\" }) },
		"service":      func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.ServiceName = "pdf; touch /tmp/oops" }) },
		"host":         func(c *Config) { changeEnv(c, func(e *Environment) { e.Host = "-oProxyCommand=oops" }) },
		"alias": func(c *Config) {
			changeEnv(c, func(e *Environment) { e.Quadlet.NetworkAliases = []string{"bad alias"} })
		},
		"routing": func(c *Config) { changeEnv(c, func(e *Environment) { e.Quadlet.Router.Enabled = ptr(true) }) },
		"label": func(c *Config) {
			changeEnv(c, func(e *Environment) {
				e.Quadlet.Router.Enabled = ptr(false)
				e.Quadlet.Labels = []string{"traefik.enable=true"}
			})
		},
		"traversal": func(c *Config) { c.Artifacts.Include = []string{"foo/../secret"} }, "state": func(c *Config) { c.Artifacts.Include = []string{".deploy"} },
		"absolute": func(c *Config) { c.Artifacts.Include = []string{"/etc/passwd"} }, "overlap": func(c *Config) { c.Artifacts.Include = []string{"files", "files/a"} },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			c := imageConfig()
			f(&c)
			if _, err := normalizeAndValidateConfig(c, "prod"); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}
func TestImageReferences(t *testing.T) {
	for _, s := range []string{"docker.io/gotenberg/gotenberg:8-chromium", "quay.io/org/app@sha256:" + strings.Repeat("a", 64), "registry.example.com:5000/app:1@sha256:" + strings.Repeat("b", 64), "localhost:5000/app:dev"} {
		if err := validateImageReference(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"nginx", "library/nginx:1", "nginx:latest", "docker.io/library/nginx", "https://docker.io/nginx:1", "docker.io/foo:<VERSION>@sha256:<DIGEST>"} {
		if err := validateImageReference(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
func TestTriStateYAML(t *testing.T) {
	for _, s := range []string{"{}", "{enabled: false}", "{enabled: true}"} {
		var r RouterConfig
		if err := yaml.Unmarshal([]byte(s), &r); err != nil {
			t.Fatal(err)
		}
		if s == "{}" && r.Enabled != nil {
			t.Fatal("omitted not nil")
		}
		if s != "{}" && r.Enabled == nil {
			t.Fatal("explicit nil")
		}
	}
}
func TestRenderModes(t *testing.T) {
	for _, mode := range []DeploymentMode{ModeImage, ModeBinary} {
		for _, enabled := range []*bool{nil, ptr(true), ptr(false)} {
			c := imageConfig()
			c.Deployment.Mode = mode
			if mode == ModeBinary {
				c.BinaryName = "server"
			}
			changeEnv(&c, func(e *Environment) {
				q := &e.Quadlet
				q.Router = RouterConfig{Enabled: enabled, Host: "pdf.example.com"}
				q.Network = "traefik-net.network"
				q.NetworkAliases = []string{"pdf-prod"}
				q.AutoRestart = true
				q.CPU = "0.5"
				q.Memory = "1G"
				q.Labels = []string{`custom=quote" and $dollar`}
			})
			n, err := normalizeAndValidateConfig(c, "prod")
			if err != nil {
				t.Fatal(err)
			}
			out, err := renderQuadlet(n, imagePlaceholder)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"NetworkAlias=pdf-prod", "Restart=on-failure", "RestartSec=5s", "[Service]\nRestart=on-failure\nRestartSec=5s\nCPUQuota=50%\nMemoryMax=1G", `Label="custom=quote\" and $dollar"`} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "Requires=traefik.service") != (enabled == nil || *enabled) {
				t.Fatal(out)
			}
			if mode == ModeImage && (strings.Contains(out, "EnvironmentFile=") || strings.Contains(out, "Exec=") || !strings.Contains(out, "Pull=never")) {
				t.Fatal(out)
			}
			if enabled != nil && !*enabled && !strings.Contains(out, `Label="traefik.enable=false"`) {
				t.Fatal(out)
			}
		}
	}
	for _, value := range []string{"1", "0.5", "50%", "125%"} {
		c := imageConfig()
		changeEnv(&c, func(e *Environment) { e.Quadlet.CPU = value })
		n, err := normalizeAndValidateConfig(c, "prod")
		if err != nil || n.CPUQuota == "" {
			t.Fatalf("%s: %v", value, err)
		}
	}
}
