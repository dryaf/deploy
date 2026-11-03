package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

const ledImportHead = "led-import-head.json"

// Imports acknowledge an already completed domain-authorized cleanup. They
// neither perform RPCs nor turn an ambiguous clear into a successful outcome.
type recoveryLEDImport struct {
	Version, Sequence, ClearSequence    int
	PreviousHash, EpochHash, ResultHash string
	Recovery                            recoveryBinding
	Fault                               recoveryLEDFault
	PreparedAt                          time.Time
}

type ledImportRecord struct {
	Intent    recoveryLEDImport
	Committed bool
}

func ledImportFile(v recoveryLEDImport) string {
	return fmt.Sprintf("led-import-%d.json", v.Sequence)
}

func ledImportSchema(ctx context.Context, q ledJournalReader) (bool, error) {
	var version string
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-import-version'").Scan(&version)
	var tables int
	if e := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='reconciliation_led_imports'").Scan(&tables); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && tables == 0 {
		return false, nil
	}
	if err != nil || version != "1" || tables != 1 {
		return false, fmt.Errorf("LED import schema changed")
	}
	return true, nil
}

func (r *recoveryReconciler) readLEDImports(ctx context.Context, q ledJournalReader, adoption *ledAdoptionEvidence, epoch *recoveryLEDEpoch) ([]ledImportRecord, error) {
	exists, err := ledImportSchema(ctx, q)
	if err != nil {
		return nil, err
	}
	var head recoveryLEDImport
	headErr := r.stage.ReadJSON(ledImportHead, &head)
	if !exists {
		if !os.IsNotExist(headErr) {
			return nil, fmt.Errorf("LED import lost schema")
		}
		return nil, nil
	}
	if headErr != nil && !os.IsNotExist(headErr) {
		return nil, headErr
	}
	rows, err := q.QueryContext(ctx, "SELECT sequence,hash,body FROM reconciliation_led_imports ORDER BY sequence LIMIT 2049")
	if err != nil {
		return nil, err
	}
	var records []ledImportRecord
	for rows.Next() {
		var sequence int
		var hash, body string
		var v recoveryLEDImport
		if err = rows.Scan(&sequence, &hash, &body); err != nil {
			break
		}
		if err = protocol.Decode([]byte(body), &v, 8192); err != nil {
			break
		}
		if sequence != len(records)+1 || sequence != v.Sequence || hash != protocol.Hash(v) || len(records) >= 2*maxRecoveryLEDFaults {
			err = fmt.Errorf("LED import history gap or digest conflict")
			break
		}
		records = append(records, ledImportRecord{v, true})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if os.IsNotExist(headErr) {
		if len(records) != 0 {
			return nil, fmt.Errorf("LED import lost independent head")
		}
		return nil, nil
	}
	if head.Sequence == len(records)+1 && len(records) < 2*maxRecoveryLEDFaults {
		records = append(records, ledImportRecord{Intent: head})
	} else if len(records) == 0 || head.Sequence != len(records) || protocol.Hash(head) != protocol.Hash(records[len(records)-1].Intent) {
		return nil, fmt.Errorf("LED import head disagrees with history")
	}
	if adoption == nil || adoption.Phase != "committed" || epoch == nil {
		return nil, fmt.Errorf("LED import lost epoch authority")
	}
	clears, err := r.readLEDClears(ctx, q)
	if err != nil {
		return nil, err
	}
	faults := map[string]recoveryLEDFault{}
	for _, f := range adoption.After.Faults {
		faults[f.ID] = f
	}
	seen := map[string]bool{}
	previous := ""
	for _, record := range records {
		v := record.Intent
		if v.Version != 1 || v.Recovery != r.journal.binding() || v.EpochHash != protocol.Hash(epoch) || v.PreviousHash != previous || v.Sequence < 1 || v.Sequence > 2*maxRecoveryLEDFaults || v.PreparedAt.Before(epoch.PreparedAt) || v.Fault != faults[v.Fault.ID] || v.Fault.ID == "" || seen[v.Fault.ID] || v.Fault.Binding == workerLEDBinding {
			return nil, fmt.Errorf("LED import ownership or predecessor changed")
		}
		if v.ClearSequence < 1 || v.ClearSequence > len(clears) {
			return nil, fmt.Errorf("LED import cleanup receipt missing")
		}
		clear := clears[v.ClearSequence-1]
		if !clear.Committed || !clear.ResultCommitted || clear.Result == nil || (clear.Result.Outcome != "acknowledged" && clear.Result.Outcome != "observed_absent") || clear.Intent.Socket != epoch.Socket || clear.Intent.FaultID != v.Fault.ID || clear.Intent.Binding != v.Fault.Binding || clear.Intent.UID != epoch.AfterLayout.UID || protocol.Hash(clear.Result) != v.ResultHash || v.PreparedAt.Before(clear.Result.FinishedAt) {
			return nil, fmt.Errorf("LED import requires exact completed domain-authorized cleanup")
		}
		var saved recoveryLEDImport
		err = r.stage.ReadJSON(ledImportFile(v), &saved)
		if !(os.IsNotExist(err) && !record.Committed) && (err != nil || protocol.Hash(saved) != protocol.Hash(v)) {
			return nil, fmt.Errorf("LED import intent file missing or changed")
		}
		previous, seen[v.Fault.ID] = protocol.Hash(v), true
	}
	return records, nil
}

func remainingInheritedFaults(adoption *ledAdoptionEvidence, records []ledImportRecord) []recoveryLEDFault {
	removed := map[string]bool{}
	for _, record := range records {
		if record.Committed {
			removed[record.Intent.Fault.ID] = true
		}
	}
	var faults []recoveryLEDFault
	for _, f := range adoption.After.Faults {
		if !removed[f.ID] {
			faults = append(faults, f)
		}
	}
	return faults
}
