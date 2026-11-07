package worker

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/dryaf/deploy/internal/safefs"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Limits preserve the completed output disposition byte-for-byte. Their receipt
// binds its file/SQL authority; neither disabling nor re-enabling is replayed.
func (s *Store) completedLEDOutputPin(ctx context.Context) (string, string, error) {
	r, present, recorded, err := s.readLEDOutput(ctx, s.db)
	if err != nil || !present {
		return "", "", err
	}
	if !recorded || !r.Complete {
		return "", "", fmt.Errorf("finish the pending LED output disposition before changing limits")
	}
	return r.Intent.Request.Operation, protocol.Hash(r), nil
}

func (s *Store) requireCompletedLEDOutputPin(ctx context.Context, expectedOperation, expected string) error {
	operation, got, err := s.completedLEDOutputPin(ctx)
	if err != nil {
		return err
	}
	if got != expected || operation != expectedOperation {
		return fmt.Errorf("limits LED output history changed")
	}
	return nil
}

// Verify every pinned historical disposition even after a subsequent output
// transition. Removing both the file and SQL head cannot erase this obligation.
func checkBootstrapLimitsLEDHistory(d *safefs.Directory, db *sql.DB, v *bootstrapLimits) error {
	ctx := context.Background()
	for count := 0; v != nil && count < 32; count++ {
		if v.Receipt.LEDOutputHash != "" {
			r, err := readLEDOutputRecord(ctx, db, v.Receipt.LEDOutputOperation)
			if err != nil || !r.Complete || r.Intent.Request.Operation != v.Receipt.LEDOutputOperation || r.Intent.Request.Identity != v.Receipt.Identity || protocol.Hash(r) != v.Receipt.LEDOutputHash {
				return fmt.Errorf("limits lost pinned LED output history")
			}
		}
		if v.Receipt.Previous == "" {
			return nil
		}
		var prior bootstrapLimits
		if err := d.ReadJSON("bootstrap-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
			return err
		}
		if !prior.Complete || prior.digest() != v.Receipt.Previous || prior.validate() != nil || prior.Receipt.Identity != v.Receipt.Identity {
			return fmt.Errorf("limits LED history predecessor changed")
		}
		v = &prior
	}
	return fmt.Errorf("limits LED history exceeds bound")
}
