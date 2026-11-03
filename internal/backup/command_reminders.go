package backup

import (
	"database/sql"
	"fmt"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

// The caller holds the existing exclusive notifier lease. Reasserting the guard
// after a crash is safe only under that lease; an abandoned guard blocks mutations
// until a subsequent dispatch pass finishes. Pre-command installations are unchanged.
func backupCommandDeliveryGuard(state *backupState) (release func() error, err error) {
	tx, err := state.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	installed, err := backupCommandsInstalled(tx)
	if err != nil {
		return nil, err
	}
	if !installed {
		return func() error { return nil }, tx.Commit()
	}
	if _, err = tx.Exec("UPDATE backup_command_delivery_guard SET active=1 WHERE singleton=1"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return func() error {
		_, err := state.db.Exec("UPDATE backup_command_delivery_guard SET active=0 WHERE singleton=1")
		return err
	}, nil
}

func backupCommandsInstalled(tx *sql.Tx) (bool, error) {
	var exists int
	err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='backup_command_evidence'").Scan(&exists)
	return exists == 1, err
}

func syncBackupCommandEvidence(tx *sql.Tx, id, class, opened, message string) (uint64, error) {
	installed, err := backupCommandsInstalled(tx)
	if err != nil || !installed {
		return 0, err
	}
	hash := notify.Hash([]string{class, opened, message})
	var old string
	var revision uint64
	err = tx.QueryRow("SELECT hash,revision FROM backup_command_evidence WHERE incident=?", id).Scan(&old, &revision)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if old == hash {
		return revision, nil
	}
	if revision >= uint64(1<<63-1) {
		return 0, fmt.Errorf("incident evidence revision exhausted")
	}
	revision++
	_, err = tx.Exec("INSERT INTO backup_command_evidence VALUES(?,?,?) ON CONFLICT(incident) DO UPDATE SET hash=excluded.hash,revision=excluded.revision", id, hash, revision)
	return revision, err
}

func incidentReminderSuppressed(tx *sql.Tx, id string, evidence uint64, now time.Time) (bool, error) {
	if evidence == 0 {
		return false, nil
	}
	var clock string
	err := tx.QueryRow("SELECT value FROM meta WHERE key='clock_suspect'").Scan(&clock)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err == nil && clock != "false" {
		return false, nil
	}
	var previous uint64
	var created, expires string
	err = tx.QueryRow("SELECT evidence,created,expires FROM backup_command_suppressions WHERE incident=?", id).Scan(&previous, &created, &expires)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return previous == evidence && suppressionCurrent(created, expires, now), nil
}

func reminderPolicySuffix(tx *sql.Tx, id string, evidence uint64) (suffix string, changed bool, err error) {
	if evidence == 0 {
		return
	}
	var decision, previous uint64
	err = tx.QueryRow("SELECT revision,evidence FROM backup_command_suppressions WHERE incident=?", id).Scan(&decision, &previous)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return
	}
	return fmt.Sprintf("/policy/%d/evidence/%d", decision, evidence), previous != evidence, nil
}

// Preserve cancelled rows and their attempts as audit evidence. A frozen batch
// containing a suppressed reminder is invalidated and unrelated members requeued.
func cancelIncidentReminders(tx *sql.Tx, incident, command string) error {
	if err := sharedReminderCancellation(tx, incident, "reminder policy: "+command); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT DISTINCT o.covered_by FROM outbox o JOIN outbox b ON b.id=o.covered_by WHERE o.kind='reminder' AND o.accepted IS NULL AND b.accepted IS NULL AND b.covered_by IS NULL AND json_extract(o.record,'$.incident_id')=?`, incident)
	if err != nil {
		return err
	}
	var batches []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		batches = append(batches, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range batches {
		if _, err = tx.Exec("UPDATE outbox SET covered_by=NULL WHERE covered_by=? AND accepted IS NULL", id); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE outbox SET covered_by=? WHERE id=? AND accepted IS NULL", "command/"+command, id); err != nil {
			return err
		}
	}
	_, err = tx.Exec("UPDATE outbox SET covered_by=? WHERE kind='reminder' AND accepted IS NULL AND covered_by IS NULL AND json_extract(record,'$.incident_id')=?", "command/"+command, incident)
	return err
}
