package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type RecoveryLEDImportView struct {
	FaultID           string    `json:"fault_id"`
	RecordHash        string    `json:"record_hash"`
	ResultHash        string    `json:"result_hash"`
	State             string    `json:"state"`
	PreparedAt        time.Time `json:"prepared_at"`
	Quarantined       bool      `json:"quarantined"`
	ActivationAllowed bool      `json:"activation_allowed"`
	ClearAllowed      bool      `json:"clear_allowed"`
}

func (v recoveryLEDImport) view(committed bool) RecoveryLEDImportView {
	state := "prepared"
	if committed {
		state = "committed"
	}
	return RecoveryLEDImportView{FaultID: v.Fault.ID, RecordHash: protocol.Hash(v), ResultHash: v.ResultHash, State: state, PreparedAt: v.PreparedAt, Quarantined: true}
}

// ImportRecoveryLEDCleanup removes one exact inherited ledger row after its
// domain-authorized cleanup completed. The full incarnation remains archived.
// This operation cannot issue or retry a daemon request or release quarantine.
func ImportRecoveryLEDCleanup(ctx context.Context, path string, c Config, socket, epochHash, resultHash string) (RecoveryLEDImportView, error) {
	if !validLEDSocket(socket) || !safefs.DigestPattern.MatchString(epochHash) || !safefs.DigestPattern.MatchString(resultHash) {
		return RecoveryLEDImportView{}, fmt.Errorf("LED cleanup import requires pinned socket and exact epoch/result hashes")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return RecoveryLEDImportView{}, err
	}
	defer r.Close()
	return r.importLEDCleanup(ctx, socket, epochHash, resultHash)
}

func (r *recoveryReconciler) importLEDCleanup(ctx context.Context, socket, epochHash, resultHash string) (out RecoveryLEDImportView, err error) {
	if !validLEDSocket(socket) || !safefs.DigestPattern.MatchString(epochHash) || !safefs.DigestPattern.MatchString(resultHash) {
		return out, fmt.Errorf("invalid LED cleanup import request")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Only schema preparation precedes the independent intent; no ownership row
	// changes before the final atomic acknowledgement/deletion transaction.
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		_, _, _, e := r.ledImportAuthority(ctx, tx, socket, epochHash, resultHash)
		if e != nil {
			return e
		}
		exists, e := ledImportSchema(ctx, tx)
		if e != nil || exists {
			return e
		}
		if e = r.capacity(tx, 16384); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "CREATE TABLE reconciliation_led_imports(sequence INTEGER PRIMARY KEY,hash TEXT NOT NULL,body TEXT NOT NULL); INSERT INTO metadata VALUES('led-import-version','1');")
		return e
	})
	if err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		v, committed, adoption, e := r.ledImportAuthority(ctx, tx, socket, epochHash, resultHash)
		if e != nil {
			return e
		}
		if committed {
			out = v.view(true)
			return nil
		}
		if e = r.capacity(tx, 16384); e != nil {
			return e
		}
		var head recoveryLEDImport
		headErr := r.stage.ReadJSON(ledImportHead, &head)
		if os.IsNotExist(headErr) || (headErr == nil && head.Sequence == v.Sequence-1) {
			if e = r.stage.WriteJSON(ledImportHead, v, headErr == nil); e != nil {
				return e
			}
		} else if headErr != nil || protocol.Hash(head) != protocol.Hash(v) {
			return fmt.Errorf("LED import head changed before publication")
		}
		if e = recoveryBoundary(r.fault, "led_import_intent"); e != nil {
			return e
		}
		var saved recoveryLEDImport
		if e = r.stage.ReadJSON(ledImportFile(v), &saved); os.IsNotExist(e) {
			e = r.stage.WriteJSON(ledImportFile(v), v, false)
		} else if e == nil && protocol.Hash(saved) != protocol.Hash(v) {
			e = fmt.Errorf("LED import intent changed")
		}
		if e != nil {
			return e
		}
		if e = recoveryBoundary(r.fault, "led_import_file"); e != nil {
			return e
		}
		raw, e := json.Marshal(v)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO reconciliation_led_imports VALUES(?,?,?)", v.Sequence, protocol.Hash(v), string(raw)); e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, "DELETE FROM led_states WHERE id=? AND binding=? AND condition=? AND identity=? AND obsolete=0", v.Fault.ID, v.Fault.Binding, v.Fault.Condition, v.Fault.Identity)
		if e != nil {
			return e
		}
		count, e := result.RowsAffected()
		if e != nil || count != 1 {
			return fmt.Errorf("LED import lost exact inherited row")
		}
		if e = recoveryBoundary(r.fault, "led_import_rows"); e != nil {
			return e
		}
		if _, e = r.readLEDAdoption(ctx, tx); e != nil {
			return e
		}
		if e = r.checkLEDEpochSources(ctx, tx, adoption); e != nil {
			return e
		}
		out = v.view(true)
		return recoveryBoundary(r.fault, "led_import_transaction")
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "led_import_commit")
	}
	return
}

func (r *recoveryReconciler) ledImportAuthority(ctx context.Context, tx *sql.Tx, socket, epochHash, resultHash string) (recoveryLEDImport, bool, *ledAdoptionEvidence, error) {
	var none recoveryLEDImport
	a, err := r.readLEDAdoption(ctx, tx)
	if err != nil {
		return none, false, a, err
	}
	epoch, phase, err := r.readLEDEpoch(ctx, tx, a)
	if err != nil {
		return none, false, a, err
	}
	if epoch == nil || phase != "committed" || protocol.Hash(epoch) != epochHash || epoch.Socket != socket {
		return none, false, a, fmt.Errorf("LED cleanup import requires exact committed epoch handoff")
	}
	if err = r.checkLEDEpochSources(ctx, tx, a); err != nil {
		return none, false, a, err
	}
	records, err := r.readLEDImports(ctx, tx, a, epoch)
	if err != nil {
		return none, false, a, err
	}
	for _, old := range records {
		if old.Intent.ResultHash == resultHash {
			return old.Intent, old.Committed, a, nil
		}
	}
	if len(records) != 0 && !records[len(records)-1].Committed {
		return none, false, a, fmt.Errorf("resume pending LED cleanup import first")
	}
	if len(records) >= 2*maxRecoveryLEDFaults {
		return none, false, a, fmt.Errorf("LED import history exhausted")
	}
	clears, err := r.readLEDClears(ctx, tx)
	if err != nil {
		return none, false, a, err
	}
	for _, clear := range clears {
		if clear.Result == nil || protocol.Hash(clear.Result) != resultHash {
			continue
		}
		if !clear.Committed || !clear.ResultCommitted || (clear.Result.Outcome != "acknowledged" && clear.Result.Outcome != "observed_absent") || clear.Intent.Socket != socket || clear.Intent.UID != epoch.AfterLayout.UID {
			return none, false, a, fmt.Errorf("LED cleanup outcome is not committed success")
		}
		for _, old := range records {
			if old.Intent.Fault.ID == clear.Intent.FaultID {
				return none, false, a, fmt.Errorf("LED fault already imported through another receipt")
			}
		}
		for _, fault := range a.After.Faults {
			if fault.ID != clear.Intent.FaultID || fault.Binding != clear.Intent.Binding || fault.Binding == workerLEDBinding {
				continue
			}
			at := r.now().UTC()
			if at.Before(epoch.PreparedAt) || at.Before(clear.Result.FinishedAt) {
				return none, false, a, fmt.Errorf("LED import clock predates its proof")
			}
			v := recoveryLEDImport{Version: 1, Sequence: len(records) + 1, ClearSequence: clear.Intent.Sequence, EpochHash: epochHash, ResultHash: resultHash, Recovery: r.journal.binding(), Fault: fault, PreparedAt: at}
			if len(records) > 0 {
				v.PreviousHash = protocol.Hash(records[len(records)-1].Intent)
			}
			return v, false, a, nil
		}
	}
	return none, false, a, fmt.Errorf("LED cleanup result does not identify an inherited fault")
}
