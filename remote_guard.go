package main

import "encoding/json"

// Share transaction lock keys without replaying recovery from a read operation.
func remoteOperationGuard(e Environment) string {
	limits, _ := json.Marshal(struct {
		Timeouts operationLimits `json:"timeouts"`
	}{limitsFor(e)})
	return transactionCommonScript + "\nconfigure_timeouts <(printf '%s' " + shellQuote(string(limits)) + ")\nservice=" + shellQuote(e.Quadlet.ServiceName) + "\ntarget=" + shellQuote(e.Dir) + "\nguard_operation\n"
}
