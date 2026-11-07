package worker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func (r *LEDReconciler) bindingID() string {
	if r.binding == nil {
		return workerLEDBinding
	}
	return r.binding.ID
}

func (r *LEDReconciler) targetLED() string {
	if r.binding == nil {
		return "green"
	}
	return r.binding.LED
}

func (r *LEDReconciler) style(condition string) LEDStyle {
	if condition == "retired" {
		condition = "baseline"
	}
	if r.binding != nil {
		return r.binding.Styles[condition]
	}
	return standardLEDStyles()[condition]
}

func (r *LEDReconciler) sourceHealth(ctx context.Context) (Health, error) {
	h, err := r.Store.Health(ctx)
	if err != nil || r.binding == nil {
		return h, err
	}
	selected, ok := h.Bindings[r.binding.ID]
	if !ok {
		return Health{}, fmt.Errorf("LED binding no longer enrolled")
	}
	return Health{Identity: h.Identity, Condition: selected.Condition, ValidUntil: selected.ValidUntil, Loops: h.Loops, Subjects: map[string][]protocol.Subject{r.binding.Producer.ID: selected.evidence}}, nil
}

// Step serializes outputs while bounding a whole pass. When a slow daemon
// exhausts the budget, the next pass starts at the next binding. No store lock
// is held during RPCs and this loop never blocks messenger dispatch. Committing
// an in-flight acknowledgement has a separate two-second completion reserve.
func (r *LEDReconciler) Step(ctx context.Context) error {
	if !r.Store.ledMu.TryLock() {
		return fmt.Errorf("LED reconciliation already running")
	}
	defer r.Store.ledMu.Unlock()
	var disabled bool
	outputErr := r.Store.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		disabled, err = r.Store.ledOutputAdmission(ctx, tx)
		return err
	})
	if disabled || outputErr != nil {
		r.Store.recordLEDOutput(workerLEDBinding, LEDObservation{Condition: "unknown"})
		for _, b := range r.Store.config.LEDBindings {
			r.Store.recordLEDOutput(b.ID, LEDObservation{Condition: "unknown"})
		}
		return outputErr
	}
	if err := r.Store.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := r.Store.recoveredLEDFaults(ctx, tx); err != nil {
			return err
		}
		if err := checkLEDLayout(ctx, tx, r.Store.config, r.Store.identity, true); err != nil {
			return err
		}
		return r.Store.checkInheritedLEDHistory(ctx, tx)
	}); err != nil {
		r.Store.recordLEDOutput(workerLEDBinding, LEDObservation{Condition: "unknown"})
		for _, b := range r.Store.config.LEDBindings {
			r.Store.recordLEDOutput(b.ID, LEDObservation{Condition: "unknown"})
		}
		return err
	}
	if r.children == nil {
		r.children = []*LEDReconciler{}
		layout := desiredLEDLayout(r.Store.config, r.Store.identity.Worker, 0)
		for _, b := range layout.Bindings {
			binding := b
			r.children = append(r.children, &LEDReconciler{Store: r.Store, API: r.API, Budget: r.Budget, binding: &binding})
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	outputs := append([]*LEDReconciler{r}, r.children...)
	start := r.next
	var first error
	for offset := 0; offset < len(outputs); offset++ {
		if err := ctx.Err(); err != nil {
			if first == nil {
				first = err
			}
			break
		}
		i := (start + offset) % len(outputs)
		r.next = (i + 1) % len(outputs)
		if err := outputs[i].stepOne(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}
