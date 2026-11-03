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

const recoveryLEDAdoptionFile = "led-adoption.json"

type recoveryLEDAdoption struct {
	Version                                              int
	Recovery                                             recoveryBinding
	PreservationHash, InventoryID, InventoryHash, Socket string
	SourcesHash, BeforeHash, AfterHash                   string
	PreparedAt                                           time.Time
}
type ledAdoptionEvidence struct {
	Record        recoveryLEDAdoption
	Phase         string
	Before, After recoveryLEDSource
}

func ledAdoptionSchema(ctx context.Context, q ledJournalReader) (bool, error) {
	var version string
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-adoption-version'").Scan(&version)
	var count int
	if e := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reconciliation_led_adoption'").Scan(&count); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && count == 0 {
		return false, nil
	}
	if err != nil || version != "1" || count != 1 {
		return false, fmt.Errorf("LED adoption schema changed or missing")
	}
	return true, nil
}
func ensureLEDAdoptionSchema(ctx context.Context, tx *sql.Tx) error {
	exists, err := ledAdoptionSchema(ctx, tx)
	if err != nil || exists {
		return err
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE reconciliation_led_adoption(singleton INTEGER PRIMARY KEY CHECK(singleton=1),hash TEXT NOT NULL,body TEXT NOT NULL); INSERT INTO metadata VALUES('led-adoption-version','1');`)
	return err
}

func (r *recoveryReconciler) makeLEDAdoption(v recoveryLEDInventory, p recoveryLEDPreservation, at time.Time) (ledAdoptionEvidence, error) {
	current, err := r.makeLEDPreservation(v, at)
	if err != nil {
		return ledAdoptionEvidence{}, err
	}
	if current.SourcesHash != p.SourcesHash || current.LayoutHash != p.LayoutHash || current.Recovery != p.Recovery || current.Socket != p.Socket {
		return ledAdoptionEvidence{}, fmt.Errorf("LED adoption changed preserved ownership")
	}
	after := v.Sources.Snapshot
	after.Faults = nil
	// Retain every exact old fault, including a previously obsolete row. Historical
	// cleanup flags remain in preservation evidence, never in executable tombstones.
	for _, f := range p.Faults {
		after.Faults = append(after.Faults, recoveryLEDFault{ID: f.ID, Binding: f.Binding, Condition: f.Condition, Identity: f.Identity})
	}
	if err = r.validateLEDSourceLimit(after, r.journal.After, 2*maxRecoveryLEDFaults); err != nil {
		return ledAdoptionEvidence{}, err
	}
	record := recoveryLEDAdoption{1, r.journal.binding(), protocol.Hash(p), v.ID, protocol.Hash(v), v.Socket, p.SourcesHash, protocol.Hash(v.Sources.Snapshot), protocol.Hash(after), at}
	return ledAdoptionEvidence{Record: record, Phase: "prepared", Before: v.Sources.Snapshot, After: after}, nil
}

// readLEDAdoption also checks the exact quarantined projection. Runtime ownership
// checks use the immutable authority separately; neither reader repairs evidence.
func (r *recoveryReconciler) readLEDAdoption(ctx context.Context, q ledJournalReader) (*ledAdoptionEvidence, error) {
	e, err := r.readLEDAdoptionAuthority(ctx, q)
	if err != nil {
		return e, err
	}
	epoch, phase, err := r.readLEDEpoch(ctx, q, e)
	if err != nil {
		return e, err
	}
	imports, err := r.readLEDImports(ctx, q, e, epoch)
	if err != nil || e == nil {
		return e, err
	}
	if phase == "committed" {
		// readLEDEpoch already validated the complete frozen source and its
		// original history. Only the live fault union remains to be checked.
		faults, err := readLEDEpochFaults(ctx, q)
		if err != nil {
			return nil, err
		}
		if protocol.Hash(faults) != protocol.Hash(remainingInheritedFaults(e, imports)) {
			return nil, fmt.Errorf("quarantined LED epoch fault union changed")
		}
	} else {
		ledger, err := r.readLEDSourceLimit(ctx, q, r.journal.After, 2*maxRecoveryLEDFaults)
		if err != nil {
			return nil, err
		}
		want := e.Record.BeforeHash
		if e.Phase == "committed" {
			want = e.Record.AfterHash
		}
		if protocol.Hash(ledger) != want {
			return nil, fmt.Errorf("LED adoption ledger differs from its atomic checkpoint")
		}
	}
	return e, nil
}

func (r *recoveryReconciler) readLEDAdoptionAuthority(ctx context.Context, q ledJournalReader) (*ledAdoptionEvidence, error) {
	exists, err := ledAdoptionSchema(ctx, q)
	if err != nil {
		return nil, err
	}
	var record recoveryLEDAdoption
	fileErr := r.stage.ReadJSON(recoveryLEDAdoptionFile, &record)
	if fileErr != nil && !os.IsNotExist(fileErr) {
		return nil, fileErr
	}
	if !exists {
		if !os.IsNotExist(fileErr) {
			return nil, fmt.Errorf("LED adoption intent lost its schema")
		}
		return nil, nil
	}
	var count int
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM reconciliation_led_adoption").Scan(&count); err != nil {
		return nil, err
	}
	if count > 1 {
		return nil, fmt.Errorf("LED adoption contains unexpected records")
	}
	var hash, body string
	dbErr := q.QueryRowContext(ctx, "SELECT hash,body FROM reconciliation_led_adoption WHERE singleton=1").Scan(&hash, &body)
	if dbErr == sql.ErrNoRows && count != 0 {
		return nil, fmt.Errorf("LED adoption singleton changed")
	}
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return nil, dbErr
	}
	if os.IsNotExist(fileErr) {
		if dbErr == nil {
			return nil, fmt.Errorf("LED adoption lost its independent intent")
		}
		return nil, nil
	}
	p, phase, err := r.readLEDPreservation(ctx, q)
	if err != nil {
		return nil, err
	}
	if p == nil || phase != "committed" || protocol.Hash(*p) != record.PreservationHash {
		return nil, fmt.Errorf("LED adoption lacks its exact committed preservation")
	}
	inventories, err := r.readLEDInventorySet(ctx, q)
	if err != nil {
		return nil, err
	}
	var inventory *recoveryLEDInventory
	for i := range inventories.Records {
		if inventories.Records[i].ID == record.InventoryID {
			inventory = &inventories.Records[i]
			break
		}
	}
	if inventory == nil || protocol.Hash(*inventory) != record.InventoryHash {
		return nil, fmt.Errorf("LED adoption lost its sealed inventory")
	}
	expected, err := r.makeLEDAdoption(*inventory, *p, record.PreparedAt)
	if err != nil {
		return nil, err
	}
	if protocol.Hash(expected.Record) != protocol.Hash(record) {
		return nil, fmt.Errorf("LED adoption authority changed")
	}
	if dbErr == nil {
		var saved recoveryLEDAdoption
		if err = protocol.Decode([]byte(body), &saved, 8192); err != nil {
			return nil, err
		}
		if protocol.Hash(saved) != hash || hash != protocol.Hash(record) {
			return nil, fmt.Errorf("LED adoption acknowledgement changed")
		}
		expected.Phase = "committed"
	}
	return &expected, nil
}

// Inventory and domain proofs continue to use the immutable pre-adoption source.
// Before using it, verify every live projection row against the adopted union.
func (r *recoveryReconciler) recoveryLEDSnapshotSource(ctx context.Context, q ledJournalReader) (recoveryLEDSource, error) {
	evidence, err := r.readLEDAdoption(ctx, q)
	if err != nil {
		return recoveryLEDSource{}, err
	}
	if evidence != nil {
		return evidence.Before, nil
	}
	return r.readLEDSource(ctx, q, r.journal.After)
}
func commitLEDAdoption(ctx context.Context, tx *sql.Tx, e ledAdoptionEvidence, fault func(string) error) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM led_states"); err != nil {
		return err
	}
	insert, err := tx.PrepareContext(ctx, "INSERT INTO led_states(id,binding,condition,identity,obsolete) VALUES(?,?,?,?,0)")
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, f := range e.After.Faults {
		if _, err := insert.ExecContext(ctx, f.ID, f.Binding, f.Condition, f.Identity); err != nil {
			return err
		}
	}
	if err := recoveryBoundary(fault, "led_adoption_rows"); err != nil {
		return err
	}
	raw, err := json.Marshal(e.Record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO reconciliation_led_adoption(singleton,hash,body) VALUES(1,?,?)", protocol.Hash(e.Record), string(raw))
	return err
}
