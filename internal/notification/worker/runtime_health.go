package worker

import (
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Process-local observations deliberately expire and are never restored as
// healthy. Error text can contain provider secrets and is not stored or exposed.
type RuntimeLoopHealth struct {
	Condition  string    `json:"condition"`
	ValidUntil time.Time `json:"valid_until"`
	Revision   uint64    `json:"revision"`
}

func (s *Store) MonitorRuntimeLoop(name string) (func(error), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !protocol.ValidToken(name) || len(s.runtimeLoops) >= 16 {
		return nil, fmt.Errorf("invalid runtime monitor")
	}
	if s.runtimeLoops == nil {
		s.runtimeLoops = map[string]RuntimeLoopHealth{}
	}
	if _, exists := s.runtimeLoops[name]; exists {
		return nil, fmt.Errorf("runtime monitor already registered")
	}
	s.runtimeLoops[name] = RuntimeLoopHealth{Condition: "unknown", Revision: 1}
	return func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		previous := s.runtimeLoops[name]
		state := RuntimeLoopHealth{Condition: "unknown", Revision: previous.Revision}
		if err == nil && s.clockSafe() {
			state.Condition = "ok"
			state.ValidUntil = s.now().Add(90 * time.Second)
		}
		// Renewal is not a state change. A failure/recovery or a stalled loop
		// returning must invalidate LED work that was prepared before it.
		if state.Condition != previous.Condition || (state.Condition == "ok" && !previous.ValidUntil.After(s.now())) {
			state.Revision++
		}
		s.runtimeLoops[name] = state
	}, nil
}

// Called inside Health's transaction, under the Store mutex.
func (s *Store) runtimeHealth(h *Health) {
	if len(s.runtimeLoops) == 0 {
		return
	}
	h.Loops = make(map[string]RuntimeLoopHealth, len(s.runtimeLoops))
	for name, state := range s.runtimeLoops {
		if !state.ValidUntil.After(s.now()) || s.clockUncertain {
			state.Condition = "unknown"
		}
		h.Loops[name] = state
		if state.Condition != "ok" && rank(state.Condition) > rank(h.Condition) {
			h.Condition = state.Condition
		}
		if state.Condition == "ok" && (h.ValidUntil.IsZero() || state.ValidUntil.Before(h.ValidUntil)) {
			h.ValidUntil = state.ValidUntil
		}
	}
}
