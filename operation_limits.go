package main

type operationLimits struct {
	Build    float64 `json:"build,omitempty"`
	Pull     float64 `json:"pull,omitempty"`
	Database float64 `json:"database,omitempty"`
	Service  float64 `json:"service,omitempty"`
	Recovery float64 `json:"recovery,omitempty"`
	Grace    float64 `json:"grace,omitempty"`
}

func limitsFor(e Environment) operationLimits {
	t := e.Timeouts
	return operationLimits{Build: budget(t.Build, "15m").Seconds(), Pull: budget(t.Pull, "15m").Seconds(), Database: budget(t.Database, "5m").Seconds(), Service: budget(t.Service, "60s").Seconds(), Recovery: budget(t.Recovery, "5m").Seconds(), Grace: budget(t.ShutdownGrace, "10s").Seconds()}
}

func configureReleasePolicy(r *imageReleaseRequest, n NormalizedConfig) {
	r.HealthStableSeconds = n.HealthStableFor.Seconds()
	r.HealthIntervalSeconds = n.HealthInterval.Seconds()
	r.HealthSuccesses = n.HealthSuccesses
	r.Timeouts = limitsFor(n.Env)
	r.MinFreeBytes = n.Env.Storage.MinFreeBytes
	r.MinFreeInodes = n.Env.Storage.MinFreeInodes
	r.ImageReserveBytes = n.Env.Storage.ImageReserveBytes
}
