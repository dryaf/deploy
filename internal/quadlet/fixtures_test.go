package quadlet

import (
	"github.com/dryaf/deploy/internal/config"
)

func imageConfig() config.Config {
	return config.Config{AppName: "pdf", Deployment: config.DeploymentConfig{Mode: config.ModeImage}, Environments: map[string]config.Environment{"prod": {Host: "vps.example.com", User: "user", Dir: "/srv/pdf", Quadlet: config.Quadlet{ServiceName: "pdf", Image: "docker.io/gotenberg/gotenberg:8-chromium"}}}}
}

func ptr[T any](v T) *T { return &v }

func changeEnv(c *config.Config, f func(*config.Environment)) {
	e := c.Environments["prod"]
	f(&e)
	c.Environments["prod"] = e
}
