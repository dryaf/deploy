package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

const recoveryLEDPreservationFile = "led-preservation.json"

func ledPreservationSchema(ctx context.Context, q ledJournalReader) (bool, error) {
	var version string
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-preservation-version'").Scan(&version)
	var count int
	if e := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reconciliation_led_preservation'").Scan(&count); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && count == 0 {
		return false, nil
	}
	if err != nil || version != "1" || count != 1 {
		return false, fmt.Errorf("LED preservation schema changed or missing")
	}
	return true, nil
}
func ensureLEDPreservationSchema(ctx context.Context, tx *sql.Tx) error {
	exists, err := ledPreservationSchema(ctx, tx)
	if err != nil || exists {
		return err
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE reconciliation_led_preservation(singleton INTEGER PRIMARY KEY CHECK(singleton=1),hash TEXT NOT NULL,body TEXT NOT NULL); INSERT INTO metadata VALUES('led-preservation-version','1');`)
	return err
}

// Independent intent and SQL acknowledgement must agree exactly. The immutable
// inventory remains the ownership proof; a checksum alone never creates a grant.
func (r *recoveryReconciler) readLEDPreservation(ctx context.Context, q ledJournalReader) (*recoveryLEDPreservation, string, error) {
	exists, err := ledPreservationSchema(ctx, q)
	if err != nil {
		return nil, "", err
	}
	var record recoveryLEDPreservation
	fileErr := r.stage.ReadJSON(recoveryLEDPreservationFile, &record)
	if fileErr != nil && !os.IsNotExist(fileErr) {
		return nil, "", fileErr
	}
	if !exists {
		if !os.IsNotExist(fileErr) {
			return nil, "", fmt.Errorf("LED preservation intent lost its schema")
		}
		return nil, "none", nil
	}
	var hash, body string
	var rows int
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM reconciliation_led_preservation").Scan(&rows); err != nil {
		return nil, "", err
	}
	if rows > 1 {
		return nil, "", fmt.Errorf("LED preservation contains unexpected records")
	}
	dbErr := q.QueryRowContext(ctx, "SELECT hash,body FROM reconciliation_led_preservation WHERE singleton=1").Scan(&hash, &body)
	if dbErr == sql.ErrNoRows && rows != 0 {
		return nil, "", fmt.Errorf("LED preservation singleton identity changed")
	}
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return nil, "", dbErr
	}
	if os.IsNotExist(fileErr) {
		if dbErr == nil {
			return nil, "", fmt.Errorf("LED preservation lost its independent intent")
		}
		return nil, "none", nil
	}
	if record.Version != 1 || record.Recovery != r.journal.binding() || record.ConfigHash != protocol.Hash(r.config) || record.PreparedAt.IsZero() {
		return nil, "", fmt.Errorf("LED preservation recovery binding changed")
	}
	set, err := r.readLEDInventorySet(ctx, q)
	if err != nil {
		return nil, "", err
	}
	var inventory *recoveryLEDInventory
	for i := range set.Records {
		if set.Records[i].ID == record.InventoryID {
			inventory = &set.Records[i]
			break
		}
	}
	if inventory == nil || protocol.Hash(*inventory) != record.InventoryHash {
		return nil, "", fmt.Errorf("LED preservation lost its sealed inventory")
	}
	expected, err := r.makeLEDPreservation(*inventory, record.PreparedAt)
	if err != nil {
		return nil, "", err
	}
	if protocol.Hash(expected) != protocol.Hash(record) {
		return nil, "", fmt.Errorf("LED preservation differs from proven ownership union")
	}
	if dbErr == sql.ErrNoRows {
		return &record, "prepared", nil
	}
	var saved recoveryLEDPreservation
	if err = protocol.Decode([]byte(body), &saved, maxRecoveryLEDInventoryBytes); err != nil {
		return nil, "", err
	}
	if hash != protocol.Hash(record) || protocol.Hash(saved) != hash {
		return nil, "", fmt.Errorf("LED preservation acknowledgement changed")
	}
	return &record, "committed", nil
}

func commitLEDPreservation(ctx context.Context, tx *sql.Tx, record recoveryLEDPreservation) error {
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > maxRecoveryLEDInventoryBytes {
		return fmt.Errorf("LED preservation exceeds storage budget")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO reconciliation_led_preservation(singleton,hash,body) VALUES(1,?,?)", protocol.Hash(record), string(raw))
	return err
}

func (r *recoveryReconciler) appendLEDPreservationReview(ctx context.Context, q ledJournalReader, sources recoveryLEDSources, view *RecoveryLEDReview) error {
	record, state, err := r.readLEDPreservation(ctx, q)
	if err != nil {
		return err
	}
	if record == nil {
		return nil
	}
	result := record.view(state)
	result.SourcesMatch = record.SourcesHash == protocol.Hash(sources)
	view.Preservation = &result
	return nil
}
