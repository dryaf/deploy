package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

const maxRecoveryLEDClears = maxRecoveryLEDInventories * 2048 // Union of two bounded source ledgers per inventory.
const recoveryLEDClearHead = "led-clear-head.json"

// RecoveryLEDClearResult describes a historical attempt. Acknowledged means the
// daemon accepted the exact clear, not that hardware rendered a particular state.
// Uncertain attempts are never automatically replayed.
type RecoveryLEDClearResult = protocol.RecoveryLEDClearResult

type recoveryLEDClearIntent struct {
	Version, Sequence                  int
	PreviousHash                       string
	Recovery                           recoveryBinding
	InventoryID, InventoryHash, Socket string
	Binding, FaultID, EvidenceHash     string
	UID                                int
	Observed                           *RecoveryLEDRegistryState
	IssuedAt, Deadline                 time.Time
}

type recoveryLEDClearRecord struct {
	Intent                     recoveryLEDClearIntent
	Result                     *RecoveryLEDClearResult
	Committed, ResultCommitted bool
}

func ledClearFile(v recoveryLEDClearIntent, suffix string) string {
	return fmt.Sprintf("led-clear-%d-%s.json", v.Sequence, suffix)
}

func (v recoveryLEDClearIntent) result(outcome string, at time.Time) RecoveryLEDClearResult {
	return RecoveryLEDClearResult{InventoryID: v.InventoryID, Binding: v.Binding, FaultID: v.FaultID, EvidenceHash: v.EvidenceHash, Outcome: outcome, IssuedAt: v.IssuedAt, FinishedAt: at}
}

func (r *recoveryReconciler) validateLEDClearIntent(ctx context.Context, q ledJournalReader, v recoveryLEDClearIntent, inventories recoveryLEDInventorySet) error {
	if v.Version != 1 || v.Sequence < 1 || v.Sequence > maxRecoveryLEDClears || v.Recovery != r.journal.binding() || v.Binding == workerLEDBinding || !v.Deadline.After(v.IssuedAt) {
		return fmt.Errorf("invalid LED cleanup intent")
	}
	var inventory *recoveryLEDInventory
	for i := range inventories.Records {
		if inventories.Records[i].ID == v.InventoryID {
			inventory = &inventories.Records[i]
		}
	}
	if inventory == nil || protocol.Hash(inventory) != v.InventoryHash || v.Socket != inventory.Socket || v.IssuedAt.Before(inventory.ObservedAt) || v.Deadline.After(inventory.ValidUntil) {
		return fmt.Errorf("LED cleanup inventory mismatch")
	}
	view := r.ledInventoryView(*inventory, inventory.Sources, nil)
	owned := false
	for _, item := range view.Items {
		if item.ID == v.FaultID && item.UID == v.UID && item.Binding == v.Binding && item.OwnershipVerified && item.Classification == "known_fault" && protocol.Hash(item.Observed) == protocol.Hash(v.Observed) {
			owned = true
		}
	}
	if !owned || !view.MappingsMatch {
		return fmt.Errorf("LED cleanup lost exact fault ownership")
	}
	for _, binding := range r.config.LEDBindings {
		if binding.ID != v.Binding {
			continue
		}
		slot, err := r.readLEDEvidenceSlot(ctx, q, *inventory, binding.Producer, true)
		if err != nil {
			return err
		}
		if !slot.ReplyCommitted || v.EvidenceHash != protocol.Hash(struct{ Inventory, Report string }{v.InventoryHash, protocol.Hash(slot.Reply)}) {
			return fmt.Errorf("LED cleanup lost domain proof")
		}
		if v.IssuedAt.Before(slot.Reply.ReceivedAt) || v.Deadline.After(slot.Reply.Challenge.ExpiresAt) {
			return fmt.Errorf("LED cleanup predates or outlives domain proof")
		}
		return nil
	}
	return fmt.Errorf("LED cleanup binding missing")
}

func (r *recoveryReconciler) readLEDClears(ctx context.Context, q ledJournalReader) ([]recoveryLEDClearRecord, error) {
	var version string
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-clear-version'").Scan(&version)
	var tables int
	if e := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='reconciliation_led_clears'").Scan(&tables); e != nil {
		return nil, e
	}
	var head recoveryLEDClearIntent
	headErr := r.stage.ReadJSON(recoveryLEDClearHead, &head)
	if err == sql.ErrNoRows && tables == 0 && os.IsNotExist(headErr) {
		return nil, nil
	}
	if err != nil || version != "1" || tables != 1 || (headErr != nil && !os.IsNotExist(headErr)) {
		return nil, fmt.Errorf("LED cleanup lost schema or head")
	}
	rows, err := q.QueryContext(ctx, "SELECT sequence,hash,body,result FROM reconciliation_led_clears ORDER BY sequence LIMIT ?", maxRecoveryLEDClears+1)
	if err != nil {
		return nil, err
	}
	var records []recoveryLEDClearRecord
	for rows.Next() {
		var seq int
		var hash, body, result string
		var record recoveryLEDClearRecord
		if err = rows.Scan(&seq, &hash, &body, &result); err != nil {
			break
		}
		if err = protocol.Decode([]byte(body), &record.Intent, 8192); err != nil {
			break
		}
		if seq != len(records)+1 || record.Intent.Sequence != seq || protocol.Hash(record.Intent) != hash {
			err = fmt.Errorf("LED cleanup history gap or digest conflict")
			break
		}
		record.Committed = true
		if result != "" {
			record.Result = &RecoveryLEDClearResult{}
			if err = protocol.Decode([]byte(result), record.Result, 4096); err != nil {
				break
			}
			record.ResultCommitted = true
		}
		records = append(records, record)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if os.IsNotExist(headErr) {
		if len(records) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("LED cleanup head missing")
	}
	if head.Sequence == len(records)+1 {
		records = append(records, recoveryLEDClearRecord{Intent: head})
	} else if len(records) == 0 || head.Sequence != len(records) || protocol.Hash(head) != protocol.Hash(records[len(records)-1].Intent) {
		return nil, fmt.Errorf("LED cleanup head disagrees with history")
	}
	inventories, err := r.readLEDInventorySet(ctx, q)
	if err != nil {
		return nil, err
	}
	previous := ""
	seen := map[string]bool{}
	for i := range records {
		record := &records[i]
		v := record.Intent
		if err = r.validateLEDClearIntent(ctx, q, v, inventories); err != nil {
			return nil, err
		}
		key := v.InventoryID + "/" + v.Binding + "/" + v.FaultID
		if v.PreviousHash != previous || seen[key] {
			return nil, fmt.Errorf("LED cleanup predecessor or duplicate conflict")
		}
		seen[key], previous = true, protocol.Hash(v)
		var saved recoveryLEDClearIntent
		err = r.stage.ReadJSON(ledClearFile(v, "intent"), &saved)
		if !(os.IsNotExist(err) && !record.Committed) && (err != nil || protocol.Hash(saved) != protocol.Hash(v)) {
			return nil, fmt.Errorf("LED cleanup intent file missing or changed")
		}
		var result RecoveryLEDClearResult
		err = r.stage.ReadJSON(ledClearFile(v, "result"), &result)
		if os.IsNotExist(err) && !record.ResultCommitted {
			continue
		}
		if err != nil || (record.ResultCommitted && protocol.Hash(record.Result) != protocol.Hash(result)) {
			return nil, fmt.Errorf("LED cleanup result file missing or changed")
		}
		if !record.Committed || protocol.Hash(result) != protocol.Hash(v.result(result.Outcome, result.FinishedAt)) || result.FinishedAt.Before(v.IssuedAt) {
			return nil, fmt.Errorf("LED cleanup result authority mismatch")
		}
		switch result.Outcome {
		case "acknowledged", "uncertain", "not_sent", "observed_absent":
		default:
			return nil, fmt.Errorf("invalid LED cleanup outcome")
		}
		record.Result = &result
	}
	return records, nil
}

// Reconcile storage only. An intent that survived a restart is never dispatchable.
func (r *recoveryReconciler) syncLEDClears(ctx context.Context, tx *sql.Tx) ([]recoveryLEDClearRecord, error) {
	records, err := r.readLEDClears(ctx, tx)
	if err != nil {
		return nil, err
	}
	for i := range records {
		v := &records[i]
		if !v.Committed {
			var saved recoveryLEDClearIntent
			if err = r.stage.ReadJSON(ledClearFile(v.Intent, "intent"), &saved); os.IsNotExist(err) {
				err = r.stage.WriteJSON(ledClearFile(v.Intent, "intent"), v.Intent, false)
			}
			if err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(v.Intent)
			if _, err = tx.ExecContext(ctx, "INSERT INTO reconciliation_led_clears(sequence,hash,body) VALUES(?,?,?)", v.Intent.Sequence, protocol.Hash(v.Intent), string(raw)); err != nil {
				return nil, err
			}
			v.Committed = true
		}
		if v.Result != nil && !v.ResultCommitted {
			raw, _ := json.Marshal(v.Result)
			if _, err = tx.ExecContext(ctx, "UPDATE reconciliation_led_clears SET result=? WHERE sequence=?", string(raw), v.Intent.Sequence); err != nil {
				return nil, err
			}
			v.ResultCommitted = true
		}
	}
	return records, nil
}

func (r *recoveryReconciler) ensureLEDClears(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := r.readLEDClears(ctx, tx); err != nil {
			return err
		}
		var version string
		err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-clear-version'").Scan(&version)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err = r.capacity(tx, 4096); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `CREATE TABLE reconciliation_led_clears(sequence INTEGER PRIMARY KEY,hash TEXT NOT NULL,body TEXT NOT NULL,result TEXT NOT NULL DEFAULT ''); INSERT INTO metadata VALUES('led-clear-version','1');`)
		return err
	})
}
