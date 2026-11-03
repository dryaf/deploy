package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// LEDObservation describes a recent registry/hardware-status observation, never
// physical rendering. It is process-local and expires if reconciliation stops.
// Optional output diagnostics do not replace producer evidence or messenger
// receipts and cannot make an installation qualified.
type LEDObservation struct {
	Condition        string    `json:"condition"`
	RegistryAccepted bool      `json:"registry_accepted"`
	Suppressed       bool      `json:"suppressed"`
	StateID          string    `json:"state_id,omitempty"`
	ValidUntil       time.Time `json:"valid_until"`
	VisualVerified   bool      `json:"visual_verified"`
}

func (s *Store) recordLEDOutput(binding string, observation LEDObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if observation.Condition != "unknown" {
		deadline := s.now().Add(30 * time.Second)
		if observation.ValidUntil.IsZero() || deadline.Before(observation.ValidUntil) {
			observation.ValidUntil = deadline
		}
	}
	if binding == workerLEDBinding {
		s.ledObservation = &observation
	} else {
		if s.ledOutputs == nil {
			s.ledOutputs = map[string]LEDObservation{}
		}
		s.ledOutputs[binding] = observation
	}
}

// Called under the store mutex as part of Health.
func (s *Store) ledHealth(h *Health) {
	if len(s.config.LEDBindings) > 0 {
		h.LEDOutputs = map[string]LEDObservation{}
		for _, b := range s.config.LEDBindings {
			observation, ok := s.ledOutputs[b.ID]
			if !ok || s.clockUncertain || !observation.ValidUntil.After(s.now()) {
				observation.Condition = "unknown"
				observation.RegistryAccepted = false
			}
			h.LEDOutputs[b.ID] = observation
		}
	}
	if s.ledObservation == nil {
		return
	}
	observation := *s.ledObservation
	if s.clockUncertain || !observation.ValidUntil.After(s.now()) {
		observation.Condition = "unknown"
		observation.RegistryAccepted = false
	}
	h.LED = &observation
}

func (r *LEDReconciler) registryCapacity(ctx context.Context, id string) error {
	entries, err := r.API.List(ctx)
	if err != nil {
		return err
	}
	owned, exists := 0, false
	for _, entry := range entries {
		if entry.OwnerUID == os.Getuid() {
			owned++
			exists = exists || entry.ID == id
		}
	}
	if !exists && (owned >= 128 || len(entries) >= 1024) {
		return fmt.Errorf("LED registry capacity unavailable")
	}
	return nil
}

func (r *LEDReconciler) observeRegistry(ctx context.Context, expected led.State, observation *LEDObservation) error {
	entries, err := r.API.List(ctx)
	if err != nil {
		return err
	}
	baseline, _ := r.ids()
	baselineStyle := r.style("baseline")
	baselinePresent, expectedPresent, winner := false, false, false
	var selected *led.Entry
	for _, entry := range entries {
		if entry.LED == r.targetLED() && entry.Winner {
			if selected != nil {
				return fmt.Errorf("LED registry has inconsistent arbitration")
			}
			copy := entry
			selected = &copy
		}
		if entry.OwnerUID != os.Getuid() {
			continue
		}
		if entry.ID == baseline {
			baselinePresent = entry.LED == r.targetLED() && entry.Pattern == baselineStyle.Pattern && entry.Priority == baselineStyle.Priority && entry.Persistent && entry.TTL == nil
		}
		if entry.ID == expected.ID {
			expectedPresent = protocol.Hash(entry.State) == protocol.Hash(expected)
			winner = entry.Winner
		}
	}
	if !baselinePresent || !expectedPresent {
		return fmt.Errorf("LED registry does not contain the acknowledged state")
	}
	status, err := r.API.Status(ctx)
	if err != nil {
		return err
	}
	var hardware struct {
		Enabled        bool       `json:"enabled"`
		Available      bool       `json:"available"`
		DefaultPattern string     `json:"default_pattern"`
		Selected       *led.Entry `json:"selected_state"`
	}
	if json.Unmarshal(status.LEDs[r.targetLED()], &hardware) != nil || status.Degraded || !hardware.Enabled || !hardware.Available || (hardware.DefaultPattern != "off" && hardware.DefaultPattern != "blink-slow") {
		return fmt.Errorf("LED daemon degraded after registry acceptance")
	}
	if selected == nil || hardware.Selected == nil || selected.OwnerUID != hardware.Selected.OwnerUID || protocol.Hash(selected.State) != protocol.Hash(hardware.Selected.State) {
		return fmt.Errorf("LED arbitration changed during observation")
	}
	*observation = LEDObservation{Condition: "ok", RegistryAccepted: true, StateID: expected.ID, Suppressed: !winner}
	if !winner {
		observation.Condition = "suppressed"
	}
	return nil
}
