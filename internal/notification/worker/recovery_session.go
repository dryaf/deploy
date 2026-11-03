package worker

import (
	"context"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// PrepareRecoveryQuarantine stages a verified snapshot under a new recovery
// epoch. It requires an offline worker and preserves the original live database.
// This internal building block never installs its candidate or starts services.
func PrepareRecoveryQuarantine(ctx context.Context, statePath, snapshotPath string, c Config) error {
	_, err := prepareSnapshotRecovery(ctx, statePath, snapshotPath, c, nil)
	return err
}

// OpenRecoverySession acquires the exclusive offline worker lease and only
// opens an already quarantined recovery. Closing it does not remove any fence.
func OpenRecoverySession(ctx context.Context, statePath string, c Config) (protocol.RecoverySession, error) {
	r, err := openRecoveryReconciler(ctx, statePath, c, nil)
	if err != nil {
		return nil, err
	}
	return r, nil
}
