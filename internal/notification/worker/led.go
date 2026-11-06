package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
)

type LEDAPI interface {
	Set(context.Context, led.State) error
	Clear(context.Context, string) error
	Status(context.Context) (led.DaemonStatus, error)
	List(context.Context) ([]led.Entry, error)
}

// LEDReconciler is confined to one goroutine. Ledger changes precede RPCs, so a
// failed clear remains retryable. Healthy and fault IDs never share a namespace.
type LEDReconciler struct {
	Store    *Store
	API      LEDAPI
	Budget   time.Duration
	binding  *LEDBinding
	children []*LEDReconciler
	next     int
	started  bool
}

func (r *LEDReconciler) ids() (string, string) {
	base := ledBindingNamespace(r.Store.identity.Worker, r.bindingID())
	return base + ".baseline", base + ".healthy"
}

func (r *LEDReconciler) stepOne(ctx context.Context) (err error) {
	if err = r.Store.requireInstallationNotRetired(); err != nil {
		return err
	}
	observation := LEDObservation{Condition: "unknown"}
	if r.Budget <= 0 || r.Budget >= 5*time.Second {
		r.Store.recordLEDOutput(r.bindingID(), observation)
		return fmt.Errorf("invalid LED latency budget")
	}
	defer func() {
		var cleanupErr error
		if err != nil {
			observation = LEDObservation{Condition: "unknown"}
			_, healthy := r.ids()
			cleanupErr = r.clearLED(ctx, healthy, nil)
		}
		r.Store.recordLEDOutput(r.bindingID(), observation)
		// A concurrent source update is a conservative deferral, not a daemon
		// outage. The journal still records that this attempt was not sent.
		if errors.Is(err, errLEDObservationChanged) {
			err = cleanupErr
		}
	}()
	return r.step(ctx, &observation)
}

func (r *LEDReconciler) step(ctx context.Context, observation *LEDObservation) error {
	baseline, healthy := r.ids()
	baselineStyle := r.style("baseline")
	baselineState := led.State{ID: baseline, LED: r.targetLED(), Pattern: baselineStyle.Pattern, Priority: baselineStyle.Priority, Persistent: true, Message: "Notification observations unavailable"}
	if err := r.registryCapacity(ctx, baseline); err != nil {
		return err
	}
	if _, _, err := r.mutateLED(ctx, "set", baseline, &baselineState, nil); err != nil {
		_ = r.clearLED(ctx, healthy, nil)
		return err
	}
	if !r.started {
		if err := r.clearLED(ctx, healthy, nil); err != nil {
			return err
		}
		r.started = true
	}
	if r.Store.VerificationOnly() {
		// Verification can revoke a healthy lease and establish unknown baselines,
		// but never retires or clears fault incarnations, even with fresh status.
		if err := r.clearLED(ctx, healthy, nil); err != nil {
			return err
		}
		return r.observeRegistry(ctx, baselineState, observation)
	}
	h, err := r.sourceHealth(ctx)
	if err != nil {
		_ = r.clearLED(ctx, healthy, nil)
		return err
	}
	// Invalid/missing hardware status can never justify a positive lease.
	status, err := r.API.Status(ctx)
	if err != nil {
		_ = r.clearLED(ctx, healthy, nil)
		return err
	}
	var green struct {
		Enabled        bool   `json:"enabled"`
		Available      bool   `json:"available"`
		DefaultPattern string `json:"default_pattern"`
	}
	if json.Unmarshal(status.LEDs[r.targetLED()], &green) != nil || !green.Enabled || !green.Available || status.Degraded || (green.DefaultPattern != "off" && green.DefaultPattern != "blink-slow") {
		_ = r.clearLED(ctx, healthy, nil)
		return fmt.Errorf("LED daemon unavailable or degraded")
	}
	if r.binding != nil {
		for _, style := range r.binding.Styles {
			if _, ok := status.Patterns[style.Pattern]; !ok {
				return fmt.Errorf("configured LED pattern unavailable")
			}
		}
	}
	condition := h.Condition
	if (condition == "ok" || condition == "paused") && led.HealthyTTL(r.Store.now(), h.ValidUntil, r.Budget, 500*time.Millisecond) == 0 {
		condition = "unknown"
	}
	var current string
	var leaseUntil time.Time
	expected := baselineState
	if condition != "ok" && condition != "retired" {
		err = r.Store.transaction(ctx, func(tx *sql.Tx) error {
			var err error
			current, err = r.Store.currentLEDState(tx, r.bindingID(), condition)
			return err
		})
		if err != nil {
			return err
		}
		style := r.style(condition)
		expected = led.State{ID: current, LED: r.targetLED(), Pattern: style.Pattern, Priority: style.Priority, Persistent: true, Message: "Notification state: " + condition}
		if condition == "paused" {
			ttl := led.HealthyTTL(r.Store.now(), h.ValidUntil, r.Budget, 500*time.Millisecond)
			if ttl < 1 {
				return fmt.Errorf("pause observation expired")
			}
			expected.TTL, expected.Persistent = &ttl, false
		}
		if err = r.registryCapacity(ctx, expected.ID); err != nil {
			return err
		}
		if expected, leaseUntil, err = r.mutateLED(ctx, "set", expected.ID, &expected, &h); err != nil {
			_ = r.clearLED(ctx, healthy, nil)
			return err
		}
		if err = r.clearLED(ctx, healthy, nil); err != nil {
			return err
		}
	}
	if condition == "retired" {
		if err = r.clearLED(ctx, healthy, nil); err != nil {
			return err
		}
	}
	// Re-read after I/O before retiring faults. If source state changed, leave
	// old faults visible and let the next tick reconcile the new observation.
	latest, err := r.sourceHealth(ctx)
	if err != nil {
		return err
	}
	if !ledHealthCurrent(h, latest) {
		return nil
	}
	var obsolete []string
	err = r.Store.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		obsolete, err = r.Store.obsoleteLEDStates(tx, r.bindingID(), current, condition == "ok" || condition == "critical" || condition == "warning" || condition == "retired")
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range obsolete {
		latest, err = r.sourceHealth(ctx)
		if err != nil {
			return err
		}
		if !ledHealthCurrent(h, latest) {
			return nil
		}
		if err = r.clearLED(ctx, id, &h); err != nil {
			return err
		}
		if err = r.Store.transaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec("DELETE FROM led_states WHERE id=? AND binding=? AND obsolete=1", id, r.bindingID())
			return err
		}); err != nil {
			return err
		}
	}
	if condition == "ok" {
		if err = r.registryCapacity(ctx, healthy); err != nil {
			return err
		}
		latest, err = r.sourceHealth(ctx)
		if err != nil {
			return err
		}
		if !ledHealthCurrent(h, latest) {
			return nil
		}
		ttl := led.HealthyTTL(r.Store.now(), h.ValidUntil, r.Budget, 500*time.Millisecond)
		if ttl < 1 {
			return r.clearLED(ctx, healthy, nil)
		}
		style := r.style("ok")
		expected = led.State{ID: healthy, LED: r.targetLED(), Pattern: style.Pattern, Priority: style.Priority, TTL: &ttl, Message: "Required producers have fresh evidence"}
		if expected, leaseUntil, err = r.mutateLED(ctx, "set", expected.ID, &expected, &h); err != nil {
			_ = r.clearLED(ctx, healthy, nil)
			return err
		}
	}
	if err = r.observeRegistry(ctx, expected, observation); err != nil {
		return err
	}
	if !leaseUntil.IsZero() {
		observation.ValidUntil = leaseUntil
	}
	latest, err = r.sourceHealth(ctx)
	if err != nil {
		return err
	}
	if !ledHealthCurrent(h, latest) || (condition == "ok" && !h.ValidUntil.After(r.Store.now())) {
		*observation = LEDObservation{Condition: "unknown"}
		return r.clearLED(ctx, healthy, nil)
	}
	return nil
}
