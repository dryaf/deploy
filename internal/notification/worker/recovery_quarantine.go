package worker

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// RecoveryQuarantineView describes durable quarantine, never publication or a
// grant to dispatch. The operation pins subsequent offline producer exchanges.
type RecoveryQuarantineView struct {
	Version            int               `json:"version"`
	Operation          string            `json:"operation"`
	Before             protocol.Identity `json:"before"`
	After              protocol.Identity `json:"after"`
	Snapshot           SnapshotManifest  `json:"snapshot"`
	Phase              string            `json:"phase"`
	CandidateHash      string            `json:"candidate_hash"`
	OriginalSourceHash string            `json:"original_source_hash"`
	DeliveryEnabled    bool              `json:"delivery_enabled"`
}

// QuarantineRecovery prepares or resumes the same verified snapshot under the
// offline worker lease. The original snapshot must remain available; matching
// only a path or a mutable recovery journal is insufficient for resume.
func QuarantineRecovery(ctx context.Context, statePath, snapshotPath string, c Config, snapshotID, databaseHash string) (RecoveryQuarantineView, error) {
	return quarantineRecovery(ctx, statePath, snapshotPath, c, snapshotID, databaseHash, nil)
}

func quarantineRecovery(ctx context.Context, statePath, snapshotPath string, c Config, snapshotID, databaseHash string, fault func(string) error) (out RecoveryQuarantineView, err error) {
	if !safefs.ValidID(snapshotID, "n") || !safefs.DigestPattern.MatchString(databaseHash) {
		return out, fmt.Errorf("exact verified snapshot ID and database hash required")
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	m, err := VerifySnapshot(ctx, snapshotPath, c)
	if err != nil {
		return out, err
	}
	if m.ID != snapshotID || m.DatabaseHash != databaseHash {
		return out, fmt.Errorf("reviewed recovery snapshot changed")
	}
	dir, err := privateDirectory(statePath)
	if err != nil {
		return out, err
	}
	defer dir.Close()
	lease, err := dir.Lock("worker.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	var j snapshotRecovery
	err = dir.ReadJSON("recovery.json", &j)
	if os.IsNotExist(err) {
		// Open takes the same exclusive worker lease and refuses a recovery
		// created in this gap. The snapshot is rechecked against the full manifest
		// before any identity or recovery state changes.
		lease.Close()
		j, err = preparePinnedSnapshotRecovery(ctx, statePath, snapshotPath, c, &m, fault)
	} else if err == nil {
		if j.Snapshot != m {
			return out, fmt.Errorf("another recovery snapshot is already prepared")
		}
		err = finishSnapshotRecovery(ctx, dir, c, &j, fault)
	}
	if err != nil {
		return out, err
	}
	// Release the preparation handle before acquiring the same exclusive lease
	// for preservation. The reopened session must prove this exact operation.
	lease.Close()
	r, err := openRecoveryReconciler(ctx, statePath, c, fault)
	if err != nil {
		return out, err
	}
	defer r.Close()
	if r.journal != j {
		return out, fmt.Errorf("recovery changed before original source preservation")
	}
	originalHash, err := r.preserveOriginal(ctx)
	if err != nil {
		return out, err
	}
	if err = recoveryBoundary(fault, "quarantine_cli_result"); err != nil {
		return out, err
	}
	saved, _, exists, err := r.originalManifest(ctx, r.db)
	if err != nil {
		return out, err
	}
	if !exists || saved.hash() != originalHash {
		return out, fmt.Errorf("preserved original source changed before result")
	}
	if err = r.checkOriginalReady(ctx, saved); err != nil {
		return out, err
	}
	return RecoveryQuarantineView{Version: 1, Operation: j.Operation, Before: j.Before, After: j.After, Snapshot: j.Snapshot, Phase: j.Phase, CandidateHash: j.CandidateHash, OriginalSourceHash: originalHash}, nil
}
