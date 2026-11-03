package deployment

import (
	"github.com/dryaf/deploy/internal/config"
)

type operationLimits struct {
	Build    float64 `json:"build,omitempty"`
	Pull     float64 `json:"pull,omitempty"`
	Database float64 `json:"database,omitempty"`
	Service  float64 `json:"service,omitempty"`
	Recovery float64 `json:"recovery,omitempty"`
	Grace    float64 `json:"grace,omitempty"`
}

func limitsFor(e config.Environment) operationLimits {
	t := e.Timeouts
	return operationLimits{Build: config.Budget(t.Build, "15m").Seconds(), Pull: config.Budget(t.Pull, "15m").Seconds(), Database: config.Budget(t.Database, "5m").Seconds(), Service: config.Budget(t.Service, "60s").Seconds(), Recovery: config.Budget(t.Recovery, "5m").Seconds(), Grace: config.Budget(t.ShutdownGrace, "10s").Seconds()}
}

func configureReleasePolicy(r *imageReleaseRequest, n config.NormalizedConfig) {
	r.HealthStableSeconds = n.HealthStableFor.Seconds()
	r.HealthIntervalSeconds = n.HealthInterval.Seconds()
	r.HealthSuccesses = n.HealthSuccesses
	r.Timeouts = limitsFor(n.Env)
	r.MinFreeBytes = n.Env.Storage.MinFreeBytes
	r.MinFreeInodes = n.Env.Storage.MinFreeInodes
	r.ImageReserveBytes = n.Env.Storage.ImageReserveBytes
}
