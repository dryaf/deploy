package transport

import (
	"time"

	"github.com/dryaf/deploy/internal/config"
)

func SshBudget(e config.Environment) time.Duration {
	t := e.Timeouts
	return config.Budget(t.Build, "15m") + config.Budget(t.Pull, "15m") + 6*config.Budget(t.Database, "5m") + 10*config.Budget(t.Service, "60s") + config.Budget(t.Recovery, "5m") + config.Budget(e.Quadlet.HealthTimeout, "60s") + config.Budget(t.ShutdownGrace, "10s")
}
