package deployment

import (
	"encoding/json"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/transport"
)

// Share transaction lock keys without replaying recovery from a read operation.
func remoteOperationGuard(e config.Environment) string {
	limits, _ := json.Marshal(struct {
		Timeouts operationLimits `json:"timeouts"`
	}{limitsFor(e)})
	return transactionCommonScript + "\nconfigure_timeouts <(printf '%s' " + transport.ShellQuote(string(limits)) + ")\nservice=" + transport.ShellQuote(e.Quadlet.ServiceName) + "\ntarget=" + transport.ShellQuote(e.Dir) + "\nguard_operation\n"
}
