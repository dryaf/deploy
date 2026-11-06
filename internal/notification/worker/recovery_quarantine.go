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
	return quarantineRecoveryOwner(ctx, statePath, snapshotPath, c, snapshotID, databaseHash, nil, "", fault)
}

// QuarantineManagedDeliveryRecovery requires the caller's exclusive installation
// lease and a stopped verification service. It preserves enrollment and refuses
// active producers; it never clears managed ownership to use unmanaged recovery.
func QuarantineManagedDeliveryRecovery(ctx context.Context, statePath, snapshotPath string, c Config, snapshotID, databaseHash, revision string, owner ManagedDeliveryOwner) (RecoveryQuarantineView, error) {
	if owner.validate() != nil || owner.ConfigHash != protocol.Hash(c) || !safefs.ValidID(revision, "n") {
		return RecoveryQuarantineView{}, fmt.Errorf("managed quarantine requires exact original owner and revision")
	}
	return quarantineRecoveryOwner(ctx, statePath, snapshotPath, c, snapshotID, databaseHash, &owner, revision, nil)
}

func quarantineRecoveryOwner(ctx context.Context, statePath, snapshotPath string, c Config, snapshotID, databaseHash string, owner *ManagedDeliveryOwner, revision string, fault func(string) error) (out RecoveryQuarantineView, err error) {
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
	if owner != nil && m.Version == 1 && m.Identity != owner.Identity {
		return out, fmt.Errorf("managed recovery snapshot does not match original owner")
	}
	dir, err := privateDirectory(statePath)
	if err != nil {
		return out, err
	}
	defer dir.Close()
	if owner != nil {
		var current ManagedDeliveryOwner
		if err = dir.ReadJSON(managedDeliveryOwnerFile, &current); err != nil {
			return out, err
		}
		if current != *owner {
			return out, fmt.Errorf("managed recovery original owner changed")
		}
	}
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
		j, err = preparePinnedSnapshotRecoveryOwner(ctx, statePath, snapshotPath, c, &m, owner, revision, fault)
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
	if owner != nil {
		originalDB, e := r.originalDB(ctx, r.db)
		if e != nil {
			return out, e
		}
		var raw string
		err = originalDB.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", managedDeliveryOwnerKey).Scan(&raw)
		closeErr := originalDB.Close()
		if err != nil {
			return out, err
		}
		if closeErr != nil {
			return out, closeErr
		}
		var original ManagedDeliveryOwner
		if protocol.Decode([]byte(raw), &original, 8192) != nil || original != *owner {
			return out, fmt.Errorf("managed recovery original SQL owner changed")
		}
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
