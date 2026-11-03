package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// The original envelope and sequence remain immutable. Only the delivery column
// changes, and only for an applied explicit retry under this recovery epoch.
type RecoveryRetryBinding struct {
	Version            int
	Generation         RecoveryGeneration
	Applied            RecoveryAppliedDisposition
	OriginalGeneration string
	DeliveryEnabled    bool
}

func (b RecoveryRetryBinding) Validate() error {
	if err := b.Generation.Validate(); err != nil {
		return err
	}
	if err := b.Applied.Application.Validate(); err != nil {
		return err
	}
	if b.Version != 1 || b.Applied.ReceiptPosition <= 0 || b.Applied.Application.Decision.Decision.Action != "retry" ||
		b.Generation.Ownership != b.Applied.Application.Ownership || !protocol.ValidToken(b.OriginalGeneration) ||
		b.OriginalGeneration == b.Generation.Generation || b.DeliveryEnabled {
		return fmt.Errorf("invalid recovery retry binding")
	}
	return nil
}

func retryBindingFile(p protocol.Producer, id string) string {
	return "retry-" + protocol.Hash(struct {
		Producer protocol.Producer
		Message  string
	}{p, id}) + ".json"
}

func retryBindingSchema(tx *sql.Tx) (bool, error) {
	var version string
	err := tx.QueryRow("SELECT value FROM metadata WHERE key='recovery_retries_schema'").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	var tables int
	if e := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reconciliation_retry_bindings'").Scan(&tables); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && tables == 0 {
		return false, nil
	}
	if err != nil || version != "1" || tables != 1 {
		return false, fmt.Errorf("retry binding schema missing or changed")
	}
	return true, nil
}

func (r *recoveryReconciler) initializeRetryBindings(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := retryBindingSchema(tx)
		if err != nil || exists {
			return err
		}
		_, err = tx.Exec("CREATE TABLE reconciliation_retry_bindings(producer TEXT NOT NULL,message TEXT NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(producer,message)); INSERT INTO metadata VALUES('recovery_retries_schema','1');")
		return err
	})
}

func (r *recoveryReconciler) retryBindingEvidence(tx *sql.Tx, p protocol.Producer, id string) (b RecoveryRetryBinding, phase string, err error) {
	exists, err := retryBindingSchema(tx)
	if err != nil {
		return b, "", err
	}
	var intent RecoveryRetryBinding
	fileErr := r.stage.ReadJSON(retryBindingFile(p, id), &intent)
	if fileErr != nil && !os.IsNotExist(fileErr) {
		return b, "", fileErr
	}
	var raw, hash string
	dbErr := sql.ErrNoRows
	if exists {
		dbErr = tx.QueryRow("SELECT hash,body FROM reconciliation_retry_bindings WHERE producer=? AND message=?", p.ID, id).Scan(&hash, &raw)
	}
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return b, "", dbErr
	}
	if dbErr == sql.ErrNoRows && os.IsNotExist(fileErr) {
		return b, "none", nil
	}
	if fileErr != nil {
		return b, "", fileErr
	}
	if !exists {
		return b, "", fmt.Errorf("retry intent lost its schema")
	}
	if err = intent.Validate(); err != nil {
		return
	}
	var plan recoveryGenerationIntent
	if err = r.stage.ReadJSON(generationFile(p), &plan); err != nil {
		return
	}
	if err = r.validateGenerationIntent(plan, p, intent.Generation.Ownership.SummaryHash); err != nil {
		return
	}
	if plan.Generation != intent.Generation.Generation || plan.Ownership != intent.Generation.Ownership {
		return b, "", fmt.Errorf("retry lost its generation intent")
	}
	d := intent.Applied.Application.Decision.Decision
	if d.Producer != p || d.MessageID != id || d.Operation != r.journal.Operation || d.Identity != r.journal.After || intent.Generation.Ownership.Before != r.journal.Before {
		return b, "", fmt.Errorf("retry intent binding changed")
	}
	applied, state, err := r.applicationEvidence(tx, p, id)
	if err != nil {
		return b, "", err
	}
	if state != "applied" || protocol.Hash(applied) != protocol.Hash(intent.Applied) {
		return b, "", fmt.Errorf("retry lost its applied decision")
	}
	if dbErr == sql.ErrNoRows {
		return intent, "intent", nil
	}
	if err = protocol.Decode([]byte(raw), &b, 32768); err != nil {
		return
	}
	if protocol.Hash(b) != hash || protocol.Hash(b) != protocol.Hash(intent) {
		return b, "", fmt.Errorf("retry binding projection changed")
	}
	return b, "bound", nil
}

func (r *recoveryReconciler) validateDispositionGeneration(tx *sql.Tx, d RecoveryDisposition, original, current string) error {
	b, phase, err := r.retryBindingEvidence(tx, d.Producer, d.MessageID)
	if err != nil {
		return err
	}
	want := original
	if phase != "none" {
		if b.Applied.Application.Decision.Decision != d || b.OriginalGeneration != original {
			return fmt.Errorf("retry changed its original message")
		}
		if phase == "bound" {
			want = b.Generation.Generation
		}
	}
	if current != want {
		return fmt.Errorf("delivery generation differs from retry journal")
	}
	return nil
}

func (r *recoveryReconciler) BindRecoveryRetry(ctx context.Context, p protocol.Producer, id string) (b RecoveryRetryBinding, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	if !protocol.ValidToken(id) {
		return b, fmt.Errorf("invalid retry message")
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		if exists, err := retryBindingSchema(tx); err != nil {
			return err
		} else if !exists {
			return protocol.ErrRecoveryIncomplete
		}
		d, phase, err := r.dispositionEvidence(tx, p, id)
		if err != nil {
			return err
		}
		if phase != "recorded" || d.Decision.Action != "retry" {
			return fmt.Errorf("binding requires a recorded retry decision")
		}
		applied, phase, err := r.applicationEvidence(tx, p, id)
		if err != nil {
			return err
		}
		if phase != "applied" {
			return protocol.ErrRecoveryIncomplete
		}
		lease, check, err := r.lockRecoveryProducer(ctx, p)
		if err != nil {
			return err
		}
		defer lease.Close()
		if err = check(); err != nil {
			return err
		}
		generation, err := r.readRecoveryGeneration(p, d.Decision.SummaryHash)
		if err != nil {
			return err
		}
		b, phase, err = r.retryBindingEvidence(tx, p, id)
		if err != nil {
			return err
		}
		if phase != "none" && b.Generation != generation {
			return fmt.Errorf("retry generation changed")
		}
		if phase == "bound" {
			return nil
		}
		job, err := readJob(tx, p.ID, id)
		if err != nil {
			return err
		}
		if phase == "none" {
			b = RecoveryRetryBinding{Version: 1, Generation: generation, Applied: applied, OriginalGeneration: job.Message.Generation}
			if err = b.Validate(); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		refs, err := json.Marshal(job.Message.Incidents)
		if err != nil {
			return err
		}
		if err = r.capacity(tx, len(raw)+len(refs)+len(job.Message.ReportKey)+1024); err != nil {
			return err
		}
		if phase == "none" {
			if err = r.stage.WriteJSON(retryBindingFile(p, id), b, false); err != nil {
				return err
			}
		}
		if err = recoveryBoundary(r.fault, "retry_binding_intent"); err != nil {
			return err
		}
		if err = check(); err != nil {
			return err
		}
		if current, err := r.readRecoveryGeneration(p, d.Decision.SummaryHash); err != nil {
			return err
		} else if current != generation {
			return fmt.Errorf("retry generation changed before commit")
		}
		if err = restoreRetryFloors(tx, job.Message); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE messages SET generation=? WHERE producer=? AND id=?", generation.Generation, p.ID, id); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO reconciliation_retry_bindings VALUES(?,?,?,?)", p.ID, id, protocol.Hash(b), string(raw)); err != nil {
			return err
		}
		if err = recoveryBoundary(r.fault, "retry_binding_transaction"); err != nil {
			return err
		}
		return check()
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "retry_binding_commit")
	}
	return
}

// Reconstructed messages may not have existed in the snapshot's semantic
// indexes. Rebuild the needed floors from all retained messages, including newer
// revisions that were not selected for retry. Never lower a surviving floor.
func restoreRetryFloors(tx *sql.Tx, m protocol.Message) error {
	if m.ReportKey != "" {
		var revision uint64
		if err := tx.QueryRow("SELECT COALESCE(max(json_extract(body,'$.report_revision')),?) FROM messages WHERE producer=? AND json_extract(body,'$.report_key')=?", m.ReportRevision, m.Producer.ID, m.ReportKey).Scan(&revision); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO reports(producer,key,revision) VALUES(?,?,?) ON CONFLICT(producer,key) DO UPDATE SET revision=max(revision,excluded.revision)", m.Producer.ID, m.ReportKey, revision); err != nil {
			return err
		}
	}
	if len(m.Incidents) > 0 {
		refs, err := json.Marshal(m.Incidents)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO incidents(producer,subject,id,revision)
SELECT ?,json_extract(i.value,'$.subject'),json_extract(i.value,'$.id'),max(json_extract(i.value,'$.revision'))
FROM messages m,json_each(m.body,'$.incident_refs') i
JOIN json_each(?) wanted ON json_extract(wanted.value,'$.subject')=json_extract(i.value,'$.subject') AND json_extract(wanted.value,'$.id')=json_extract(i.value,'$.id')
WHERE m.producer=? GROUP BY json_extract(i.value,'$.subject'),json_extract(i.value,'$.id')
ON CONFLICT(producer,subject,id) DO UPDATE SET revision=max(revision,excluded.revision)`, m.Producer.ID, string(refs), m.Producer.ID)
		if err != nil {
			return err
		}
	}
	return nil
}
