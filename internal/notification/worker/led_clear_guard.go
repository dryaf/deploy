package worker

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/led"
)

// A local cleanup tombstone proves which ID we own; it does not prove that the
// daemon still holds the state we created. In particular, an edited registry
// entry must not silently become executable cleanup authority after restart.
// The daemon scopes Clear to the caller UID. Other UIDs' entries are unowned.
func (r *LEDReconciler) checkLEDClearRegistry(ctx context.Context, op ledOperation) error {
	_, healthy := r.ids()
	if op.Kind != "clear" || op.ID == healthy {
		return nil // Revoking a positive lease must remain possible on degradation.
	}
	entries, err := r.API.List(ctx)
	if err != nil {
		return err
	}
	if len(entries) > 1024 {
		return fmt.Errorf("LED registry exceeds budget")
	}
	var found *led.Entry
	for i := range entries {
		e := &entries[i]
		if e.ID != op.ID || e.OwnerUID != os.Getuid() {
			continue
		}
		if found != nil {
			return fmt.Errorf("LED registry contains duplicate owned fault IDs")
		}
		found = e
	}
	if found == nil {
		return nil // Idempotent clear of an already absent, never-reused owned ID.
	}
	style := r.style(op.Condition)
	if found.LED != r.targetLED() || found.Pattern != style.Pattern || found.Priority != style.Priority ||
		(op.Ownership != "legacy" && found.Message != "Notification state: "+op.Condition) {
		return fmt.Errorf("LED fault registry state differs from recorded ownership")
	}
	if op.Condition == "paused" {
		if found.Persistent || found.TTL == nil || *found.TTL < 1 || *found.TTL > 90 {
			return fmt.Errorf("LED pause registry state differs from recorded ownership")
		}
	} else if !found.Persistent || found.TTL != nil {
		return fmt.Errorf("LED fault persistence differs from recorded ownership")
	}
	return nil
}
