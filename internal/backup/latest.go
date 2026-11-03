package backup

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/safefs"
)

// Publication intent is checkpointed before a bundle is renamed into place.
// It survives complete bundle loss, state reconstruction, and generation changes.
// An interrupted publication is deliberately uncertain until a newer capture;
// explicit backup IDs remain usable for recovery.
type LatestSnapshot struct {
	BackupID        string `json:"backup_id"`
	WriterEpoch     uint64 `json:"writer_epoch"`
	CaptureSequence uint64 `json:"capture_sequence"`
}

func latestSnapshotKey(r SnapshotReceipt) string {
	return instanceKey(r.Header.Attempt.SourceID, r.Header.Attempt.Service) + "/" + r.Header.Generation
}

func rememberLatestSnapshot(control *ArchiveControl, receipt SnapshotReceipt) error {
	if control.LatestSnapshots == nil {
		control.LatestSnapshots = map[string]LatestSnapshot{}
	}
	key := latestSnapshotKey(receipt)
	next := LatestSnapshot{receipt.BackupID, receipt.WriterEpoch, receipt.Header.Attempt.CaptureSequence}
	previous, ok := control.LatestSnapshots[key]
	if ok {
		if previous.WriterEpoch > next.WriterEpoch || (previous.WriterEpoch == next.WriterEpoch && previous.CaptureSequence > next.CaptureSequence) {
			return nil
		}
		if previous.WriterEpoch == next.WriterEpoch && previous.CaptureSequence == next.CaptureSequence && previous.BackupID != next.BackupID {
			return fmt.Errorf("conflicting latest capture identities")
		}
	}
	control.LatestSnapshots[key] = next
	return nil
}

// Upgrade older archives using the catalog, including rows for missing bundles.
// Scanning surviving directories alone would erase evidence of lost captures.
func (a *backupArchive) reconcileLatestSnapshots(state *backupState) error {
	// Rebuilt catalogs contain only survivors, so they cannot supply missing
	// legacy evidence. Preserve checkpointed identities or await a new capture.
	var recovered ArchiveControl
	if err := state.get("recovered_control", &recovered); err == nil {
		return nil
	} else if err != sql.ErrNoRows {
		return err
	}
	rows, err := state.db.Query("SELECT record FROM bundles")
	if err != nil {
		return err
	}
	var receipts []SnapshotReceipt
	for rows.Next() {
		var raw string
		var receipt SnapshotReceipt
		if err = rows.Scan(&raw); err == nil {
			err = safefs.DecodeJSON([]byte(raw), &receipt, safefs.MaxRecordBytes)
		}
		if err == nil {
			err = validateReceipt(receipt, a.identity.ID)
		}
		if err != nil {
			rows.Close()
			return err
		}
		receipts = append(receipts, receipt)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(receipts) == 0 {
		return nil
	}
	return a.updateControl(state, func(control *ArchiveControl) error {
		for _, receipt := range receipts {
			if err := rememberLatestSnapshot(control, receipt); err != nil {
				return err
			}
		}
		return nil
	})
}

func requireLatestSnapshot(control ArchiveControl, receipt SnapshotReceipt) error {
	latest, ok := control.LatestSnapshots[latestSnapshotKey(receipt)]
	if !ok || latest.BackupID != receipt.BackupID || latest.WriterEpoch != receipt.WriterEpoch || latest.CaptureSequence != receipt.Header.Attempt.CaptureSequence {
		return fmt.Errorf("latest snapshot is missing or uncertain; list backups and select an explicit backup ID")
	}
	// Older archives may also have success evidence newer than their remaining
	// catalog. Do not let an upgrade manufacture certainty from that catalog.
	instance := control.Instances[instanceKey(receipt.Header.Attempt.SourceID, receipt.Header.Attempt.Service)]
	if instance.Generation == receipt.Header.Generation && instance.LastSuccess.After(receipt.Header.Attempt.RequestedAt) {
		return fmt.Errorf("latest successful capture is unavailable; select an explicit backup ID")
	}
	return nil
}
