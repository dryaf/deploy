package worker

import "github.com/dryaf/deploy/internal/notification/protocol"

// ledHealthCurrent permits only deadline extensions from successful runtime
// observations. Producer evidence, loop membership and state revisions must be
// unchanged. The caller still uses the original deadline for its healthy lease;
// a concurrent renewal cannot authorize a longer lease than was checked.
func ledHealthCurrent(before, after Health) bool {
	// Output diagnostics are observations of this reconciler, not producer
	// evidence. Including them would make successful reconciliation invalidate
	// its own source observation on the next pass.
	before.LED, after.LED = nil, nil
	before.LEDOperations, after.LEDOperations = nil, nil
	before.Bindings, after.Bindings = nil, nil
	before.LEDOutputs, after.LEDOutputs = nil, nil
	if len(before.Loops) != len(after.Loops) || after.ValidUntil.Before(before.ValidUntil) {
		return false
	}
	loops := make(map[string]RuntimeLoopHealth, len(after.Loops))
	for name, old := range before.Loops {
		current, ok := after.Loops[name]
		if !ok || current.Condition != old.Condition || current.Revision != old.Revision || current.ValidUntil.Before(old.ValidUntil) {
			return false
		}
		current.ValidUntil = old.ValidUntil
		loops[name] = current
	}
	after.Loops = loops
	after.ValidUntil = before.ValidUntil
	return protocol.Hash(before) == protocol.Hash(after)
}
