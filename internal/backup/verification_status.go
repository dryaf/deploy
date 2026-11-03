package backup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Receipts remain immutable historical evidence. Later checks have their own
// current status; an execution error never erases a definitive corruption result.
type BackupVerificationStatus struct {
	Last       ArchiveVerification  `json:"last_attempt"`
	Definitive *ArchiveVerification `json:"last_definitive,omitempty"`
}

// Selection is also used by explicit re-verification, which must remain possible
// after failure. Downloads hold a shared bundle lease and verification holds an
// exclusive one. The control lease protects only this short metadata read, not
// a potentially slow transfer or SQLite validation.
func (a *backupArchive) downloadVerificationSafe(receipt SnapshotReceipt) error {
	lock, err := a.dir.Lock(".locks/control.lock", false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	statuses, err := a.verificationStatuses(100000)
	if err == nil {
		status := statuses[receipt.BackupID]
		records := []ArchiveVerification{status.Last}
		if status.Definitive != nil {
			records = append(records, *status.Definitive)
		}
		for _, record := range records {
			if record.ID != "" && (record.ArchiveID != receipt.ArchiveID || record.BackupID != receipt.BackupID || record.SHA256 != receipt.Header.SHA256) {
				err = fmt.Errorf("verification identity conflict blocks download")
				break
			}
		}
		if err == nil && status.Definitive != nil && status.Definitive.ErrorKind != "" {
			err = fmt.Errorf("known %s failure for %s blocks download; use explicit verification to investigate", status.Definitive.ErrorKind, receipt.BackupID)
		}
	}
	return err
}

func mergeVerificationStatus(old BackupVerificationStatus, next ArchiveVerification) BackupVerificationStatus {
	newer := func(a, b ArchiveVerification) bool {
		return a.ControlRevision > b.ControlRevision || a.ControlRevision == b.ControlRevision && (a.StartedAt.After(b.StartedAt) || a.StartedAt.Equal(b.StartedAt) && a.ID > b.ID)
	}
	if old.Last.ID == "" || !newer(old.Last, next) {
		old.Last = next
	}
	if next.ErrorKind == "checksum" || next.ErrorKind == "integrity" || next.ErrorKind == "" && next.Validation.Result == "ok" {
		if old.Definitive != nil && newer(*old.Definitive, next) && !(next.ErrorKind == "integrity" && old.Definitive.ErrorKind == "" && old.Definitive.Validation.Check != "integrity_check") {
			return old
		}
		if old.Definitive != nil && old.Definitive.ErrorKind == "integrity" && next.ErrorKind != "integrity" && (next.ErrorKind != "" || next.Validation.Check != "integrity_check") {
			return old
		}
		copy := next
		old.Definitive = &copy
	}
	return old
}

func (s *backupState) rememberVerification(next ArchiveVerification) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key := "verification/" + next.BackupID
	var raw string
	var old BackupVerificationStatus
	err = tx.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if err = safefs.DecodeJSON([]byte(raw), &old, safefs.MaxRecordBytes); err != nil {
			return err
		}
		if old.Last.SHA256 != next.SHA256 || old.Last.ArchiveID != next.ArchiveID {
			return fmt.Errorf("verification identity conflict")
		}
	}
	body, err := json.Marshal(mergeVerificationStatus(old, next))
	if err != nil {
		return err
	}
	if string(body) == raw {
		return tx.Commit()
	}
	if _, err = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(body)); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *backupArchive) writeVerification(state *backupState, control *ArchiveControl, record *ArchiveVerification) error {
	record.ControlRevision = control.Revision + 1
	// Fail closed if the process dies before the archive checkpoint is written.
	if record.ErrorKind != "" {
		if err := state.rememberVerification(*record); err != nil {
			return err
		}
	}
	if err := a.dir.WriteJSON("control/verification/"+record.ID+".json", record, false); err != nil {
		return err
	}
	return state.rememberVerification(*record)
}

func (a *backupArchive) verificationStatuses(limit int) (map[string]BackupVerificationStatus, error) {
	result := map[string]BackupVerificationStatus{}
	var records []ArchiveVerification
	dir, err := a.dir.Sub("control/verification")
	if err != nil {
		return result, err
	}
	defer dir.Close()
	entries, err := dir.File().ReadDir(limit + 1)
	if err != nil && err != io.EOF {
		return result, err
	}
	if len(entries) > limit {
		return result, fmt.Errorf("verification history scan limit reached")
	}
	for _, entry := range entries {
		name := entry.Name()
		if safefs.IsJSONTemporary(name) {
			continue
		}
		if !strings.HasSuffix(name, ".json") || !safefs.ValidID(strings.TrimSuffix(name, ".json"), "n") {
			return result, fmt.Errorf("unknown verification history entry")
		}
		file, err := dir.Open(name, unix.O_RDONLY, 0)
		if err != nil {
			return result, err
		}
		body, err := io.ReadAll(io.LimitReader(file, protocol.MaxBackupHeader+1))
		file.Close()
		if err != nil {
			return result, err
		}
		var shape map[string]json.RawMessage
		if err = protocol.StrictBackupJSON(body, &shape); err != nil {
			return result, err
		}
		if _, application := shape["contract_sha256"]; application {
			continue
		}
		var record ArchiveVerification
		if err = protocol.StrictBackupJSON(body, &record); err != nil {
			return result, err
		}
		if record.Schema != 1 || record.ArchiveID != a.identity.ID || record.ID+".json" != name || !safefs.ValidID(record.BackupID, "b") || !safefs.DigestPattern.MatchString(record.SHA256) {
			return result, fmt.Errorf("invalid verification identity")
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.ControlRevision != b.ControlRevision {
			return a.ControlRevision < b.ControlRevision
		}
		if !a.StartedAt.Equal(b.StartedAt) {
			return a.StartedAt.Before(b.StartedAt)
		}
		return a.ID < b.ID
	})
	for _, record := range records {
		old := result[record.BackupID]
		if old.Last.ID != "" && old.Last.SHA256 != record.SHA256 {
			return result, fmt.Errorf("verification identity conflict")
		}
		result[record.BackupID] = mergeVerificationStatus(old, record)
	}
	return result, nil
}

func (a *backupArchive) syncVerificationStatuses(state *backupState) error {
	statuses, err := a.verificationStatuses(100000)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		if status.Definitive != nil {
			if err = state.rememberVerification(*status.Definitive); err != nil {
				return err
			}
		}
		if err = state.rememberVerification(status.Last); err != nil {
			return err
		}
	}
	return nil
}

func (s *backupState) latestVerificationProblem(instance ExpectedInstance) (string, error) {
	receipt, err := s.latestCatalogReceipt(instance)
	if err == sql.ErrNoRows {
		return "latest snapshot is unavailable", nil
	}
	if err != nil {
		return "", err
	}
	var unavailable string
	if err = s.get("unavailable/"+receipt.BackupID, &unavailable); err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if unavailable != "" {
		return "latest snapshot is unavailable: " + unavailable, nil
	}
	var status BackupVerificationStatus
	err = s.get("verification/"+receipt.BackupID, &status)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if status.Last.SHA256 != receipt.Header.SHA256 {
		return "verification identity conflict", nil
	}
	if status.Definitive != nil && status.Definitive.ErrorKind != "" {
		return "known-corrupt latest snapshot (" + status.Definitive.ErrorKind + ")", nil
	}
	if status.Last.ErrorKind != "" {
		return "latest verification inconclusive (" + status.Last.ErrorKind + ")", nil
	}
	return "", nil
}
