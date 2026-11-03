package backup

import (
	"database/sql"
	"fmt"
)

// This guard belongs at the mutation boundary, not general maintenance admission:
// listing and explicit integrity/restore checks remain useful during clock repair.
func (s *backupState) retentionClockSafe() error {
	var suspect bool
	if err := s.get("clock_suspect", &suspect); err != nil && err != sql.ErrNoRows {
		return err
	}
	if suspect {
		return fmt.Errorf("retention suspended for clock uncertainty; reconcile time and use ack-clock --reason")
	}
	return nil
}

func retentionVerificationSafe(state *backupState, preview RetentionPreview, history map[string]BackupVerificationStatus) error {
	for _, row := range preview.Candidates {
		receipt := row.Receipt
		status := history[receipt.BackupID]
		var local BackupVerificationStatus
		err := state.get("verification/"+receipt.BackupID, &local)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		// Archive evidence survives state reconstruction; local evidence also covers
		// failures detected while archive checkpoint publication was unavailable.
		for _, evidence := range []BackupVerificationStatus{status, local} {
			records := []ArchiveVerification{evidence.Last}
			if evidence.Definitive != nil {
				records = append(records, *evidence.Definitive)
			}
			for _, record := range records {
				if record.ID == "" {
					continue
				}
				if record.ArchiveID != receipt.ArchiveID || record.BackupID != receipt.BackupID || record.SHA256 != receipt.Header.SHA256 {
					return fmt.Errorf("verification identity conflict blocks retention")
				}
				status = mergeVerificationStatus(status, record)
			}
		}
		if status.Definitive != nil && status.Definitive.ErrorKind != "" {
			return fmt.Errorf("known %s failure for %s blocks retention in this scope", status.Definitive.ErrorKind, receipt.BackupID)
		}
	}
	return nil
}
