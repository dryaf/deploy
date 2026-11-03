package worker

import (
	"database/sql"
	"fmt"
)

// Historical IDs cannot re-enter ordinary execution/result protocols after an
// epoch transition. These audit tables survive projection installation; newly
// authorized commands have new IDs and are not covered by this fence.
func historicalCommand(tx *sql.Tx, id string) (bool, error) {
	var marker, tables int
	if err := tx.QueryRow("SELECT count(*) FROM metadata WHERE key='recovery'").Scan(&marker); err != nil {
		return false, err
	}
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('recovery_commands','reconciliation_command_records')").Scan(&tables); err != nil {
		return false, err
	}
	if marker == 0 && tables == 0 {
		return false, nil
	}
	if marker != 1 || tables != 2 {
		return false, fmt.Errorf("command recovery history missing")
	}

	sealed, err := requestRecoverySchema(tx)
	if err != nil {
		return false, err
	}
	if sealed {
		if _, err = requestRecoveryAdmissionReady(tx); err != nil {
			return false, err
		}
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM reconciliation_request_fences WHERE target_id=?", id).Scan(&count); err != nil {
			return false, err
		}
		if count != 0 {
			return true, nil
		}
	}
	var historical bool
	err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM recovery_commands WHERE id=? UNION ALL SELECT 1 FROM reconciliation_command_records WHERE id=?)", id, id).Scan(&historical)
	return historical, err
}
func requireCurrentCommand(tx *sql.Tx, id string) error {
	historical, err := historicalCommand(tx, id)
	if err != nil {
		return err
	}
	if historical {
		return fmt.Errorf("historical command is fenced by recovery")
	}
	return nil
}
