package deployment

import (
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
)

func TestRenderModes(t *testing.T) {
	for _, mode := range []config.DeploymentMode{config.ModeImage, config.ModeBinary} {
		for _, enabled := range []*bool{nil, ptr(true), ptr(false)} {
			c := imageConfig()
			c.Deployment.Mode = mode
			if mode == config.ModeBinary {
				c.BinaryName = "server"
			}
			changeEnv(&c, func(e *config.Environment) {
				q := &e.Quadlet
				q.Router = config.RouterConfig{Enabled: enabled, Host: "pdf.example.com"}
				q.Network = "traefik-net.network"
				q.NetworkAliases = []string{"pdf-prod"}
				q.AutoRestart = true
				q.CPU = "0.5"
				q.Memory = "1G"
				q.Labels = []string{`custom=quote" and $dollar`}
			})
			n, err := config.Normalize(c, "prod")
			if err != nil {
				t.Fatal(err)
			}
			out, err := quadlet.Render(n, quadlet.ImagePlaceholder)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"NetworkAlias=pdf-prod", "Restart=on-failure", "RestartSec=5s", "[Service]\nExecStartPre=/bin/bash \"/srv/pdf/.deploy/" + string(mode) + "/activation-check.sh\"\nRestart=on-failure\nRestartSec=5s\nCPUQuota=50%\nMemoryMax=1G", `Label="custom=quote\" and $dollar"`} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "Requires=traefik.service") != (enabled == nil || *enabled) {
				t.Fatal(out)
			}
			if mode == config.ModeImage && (strings.Contains(out, "EnvironmentFile=") || strings.Contains(out, "Exec=") || !strings.Contains(out, "Pull=never")) {
				t.Fatal(out)
			}
			if enabled != nil && !*enabled && !strings.Contains(out, `Label="traefik.enable=false"`) {
				t.Fatal(out)
			}
		}
	}
	for _, value := range []string{"1", "0.5", "50%", "125%"} {
		c := imageConfig()
		changeEnv(&c, func(e *config.Environment) { e.Quadlet.CPU = value })
		n, err := config.Normalize(c, "prod")
		if err != nil || n.CPUQuota == "" {
			t.Fatalf("%s: %v", value, err)
		}
	}
}
