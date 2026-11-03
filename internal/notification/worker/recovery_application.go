package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"golang.org/x/sys/unix"
)

// Application changes only the quarantined projection. Retry remains fenced by
// its revoked generation; cancellation is an unsuccessful terminal outcome.
type RecoveryDispositionApplication struct {
	Version         int
	Decision        RecoveryDispositionRecord
	Ownership       RecoveryOwnership
	State           string
	AppliedAt       time.Time
	DeliveryEnabled bool
}

type RecoveryAppliedDisposition struct {
	Application     RecoveryDispositionApplication
	ReceiptPosition int64
}

type RecoveryApplicationSession interface {
	RecoveryDispositionSession
	ApplyRecoveryMessage(context.Context, protocol.Producer, string) (RecoveryAppliedDisposition, error)
	RecoveryMessageApplication(context.Context, protocol.Producer, string) (RecoveryAppliedDisposition, error)
}

func OpenRecoveryApplicationSession(ctx context.Context, path string, c Config) (RecoveryApplicationSession, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	if err = r.initializeDispositions(ctx); err == nil {
		err = r.initializeApplications(ctx)
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (a RecoveryDispositionApplication) Validate() error {
	d, p := a.Decision.Decision, a.Ownership
	if err := d.Validate(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	state := "cancelled"
	if d.Action == "retry" {
		state = "fenced"
	}
	if a.Version != 1 || a.Decision.Version != 1 || a.Decision.RecordedAt.IsZero() || a.Decision.DeliveryEnabled ||
		a.AppliedAt.IsZero() || a.AppliedAt.Before(a.Decision.RecordedAt) || a.DeliveryEnabled || a.State != state ||
		p.Operation != d.Operation || p.After != d.Identity || p.Producer != d.Producer || p.SummaryHash != d.SummaryHash {
		return fmt.Errorf("invalid recovery decision application")
	}
	return nil
}

func applicationFile(p protocol.Producer, id string) string {
	return "application-" + protocol.Hash(struct {
		Producer protocol.Producer
		Message  string
	}{p, id}) + ".json"
}

func applicationSchema(tx *sql.Tx) (bool, error) {
	var version string
	err := tx.QueryRow("SELECT value FROM metadata WHERE key='recovery_applications_schema'").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	var tables int
	if e := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reconciliation_applications'").Scan(&tables); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && tables == 0 {
		return false, nil
	}
	if err != nil || version != "1" || tables != 1 {
		return false, fmt.Errorf("recovery application schema missing or changed")
	}
	return true, nil
}

func (r *recoveryReconciler) initializeApplications(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := applicationSchema(tx)
		if err != nil || exists {
			return err
		}
		_, err = tx.Exec("CREATE TABLE reconciliation_applications(producer TEXT NOT NULL,message TEXT NOT NULL,position INTEGER NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(producer,message)); INSERT INTO metadata VALUES('recovery_applications_schema','1');")
		return err
	})
}

// Read both journals without repairing either. Receipt allocation belongs to the
// DB transaction, not the file intent: different pending applications may resume
// in either order after a crash.
func (r *recoveryReconciler) applicationEvidence(tx *sql.Tx, p protocol.Producer, id string) (record RecoveryAppliedDisposition, state string, err error) {
	exists, err := applicationSchema(tx)
	if err != nil {
		return record, "", err
	}
	var intent RecoveryDispositionApplication
	fileErr := r.stage.ReadJSON(applicationFile(p, id), &intent)
	if fileErr != nil && !os.IsNotExist(fileErr) {
		return record, "", fileErr
	}
	var body, hash string
	var position int64
	dbErr := sql.ErrNoRows
	if exists {
		dbErr = tx.QueryRow("SELECT position,hash,body FROM reconciliation_applications WHERE producer=? AND message=?", p.ID, id).Scan(&position, &hash, &body)
	}
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return record, "", dbErr
	}
	if dbErr == sql.ErrNoRows && os.IsNotExist(fileErr) {
		return record, "none", nil
	}
	if fileErr != nil {
		return record, "", fileErr
	}
	if !exists {
		return record, "", fmt.Errorf("application intent lost its schema")
	}
	if err = intent.Validate(); err != nil {
		return
	}
	if intent.Decision.Decision.Producer != p || intent.Decision.Decision.MessageID != id ||
		intent.Decision.Decision.Operation != r.journal.Operation || intent.Ownership.Before != r.journal.Before || intent.Ownership.After != r.journal.After {
		return record, "", fmt.Errorf("application intent binding changed")
	}
	saved, err := readDisposition(tx, p, id)
	if err != nil {
		return record, "", err
	}
	var decisionFile RecoveryDispositionRecord
	if err = r.stage.ReadJSON(dispositionFile(p, id), &decisionFile); err != nil {
		return
	}
	if protocol.Hash(saved) != protocol.Hash(intent.Decision) || protocol.Hash(saved) != protocol.Hash(decisionFile) {
		return record, "", fmt.Errorf("application lost its recorded decision")
	}
	if dbErr == sql.ErrNoRows {
		return RecoveryAppliedDisposition{Application: intent}, "intent", nil
	}
	if err = protocol.Decode([]byte(body), &record, 16384); err != nil {
		return
	}
	if protocol.Hash(record) != hash || protocol.Hash(record.Application) != protocol.Hash(intent) || record.ReceiptPosition != position || position <= 0 {
		return record, "", fmt.Errorf("application completion changed")
	}
	var receiptProducer, receiptBody string
	if err = tx.QueryRow("SELECT producer,body FROM receipts WHERE position=?", position).Scan(&receiptProducer, &receiptBody); err != nil {
		return
	}
	var receipt protocol.Receipt
	if err = protocol.Decode([]byte(receiptBody), &receipt, 8192); err != nil {
		return
	}
	var seq uint64
	if err = tx.QueryRow("SELECT seq FROM messages WHERE producer=? AND id=?", p.ID, id).Scan(&seq); err != nil {
		return
	}
	want := protocol.Receipt{EventID: id, Sequence: seq, State: intent.State, At: intent.AppliedAt}
	if receiptProducer != p.ID || protocol.Hash(receipt) != protocol.Hash(want) {
		return record, "", fmt.Errorf("application receipt changed")
	}
	return record, "applied", nil
}

func (r *recoveryReconciler) validateDispositionState(tx *sql.Tx, d RecoveryDisposition, state string) error {
	a, phase, err := r.applicationEvidence(tx, d.Producer, d.MessageID)
	if err != nil {
		return err
	}
	if phase != "none" && a.Application.Decision.Decision != d {
		return fmt.Errorf("application decision changed")
	}
	want := "ambiguous"
	if phase == "applied" {
		want = a.Application.State
	} else {
		var receipts int
		if err := tx.QueryRow("SELECT count(*) FROM receipts WHERE producer=? AND json_extract(body,'$.event_id')=?", d.Producer.ID, d.MessageID).Scan(&receipts); err != nil {
			return err
		}
		if receipts != 0 {
			return fmt.Errorf("unowned recovery application receipt")
		}
	}
	if state != want {
		return fmt.Errorf("message state differs from recovery decision application")
	}
	return nil
}

func (r *recoveryReconciler) ApplyRecoveryMessage(ctx context.Context, p protocol.Producer, id string) (record RecoveryAppliedDisposition, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	if !protocol.ValidToken(id) {
		return record, fmt.Errorf("invalid recovery message")
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		if exists, err := applicationSchema(tx); err != nil {
			return err
		} else if !exists {
			return protocol.ErrRecoveryIncomplete
		}
		d, phase, err := r.dispositionEvidence(tx, p, id)
		if err != nil {
			return err
		}
		if phase != "recorded" {
			return protocol.ErrRecoveryIncomplete
		}
		lease, err := r.dir.Lock(ownershipName(p)+".lock", true, false)
		if err != nil {
			return err
		}
		defer lease.Close()
		check := func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.checkAuthority(); err != nil {
				return err
			}
			held, err := lease.File().Stat()
			if err != nil {
				return err
			}
			f, err := r.dir.Open(ownershipName(p)+".lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			info, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if !os.SameFile(held, info) {
				return fmt.Errorf("application producer lock changed")
			}
			return nil
		}
		if err = check(); err != nil {
			return err
		}
		ownership, err := r.readRecoveryOwnership(p, d.Decision.SummaryHash)
		if err != nil {
			return err
		}
		record, phase, err = r.applicationEvidence(tx, p, id)
		if err != nil {
			return err
		}
		if phase != "none" && record.Application.Ownership != ownership {
			return fmt.Errorf("application ownership proof changed")
		}
		if phase == "applied" {
			return nil
		}
		var seq uint64
		var attempt, generation string
		if err = tx.QueryRow("SELECT seq,attempt,generation FROM messages WHERE producer=? AND id=?", p.ID, id).Scan(&seq, &attempt, &generation); err != nil {
			return err
		}
		if attempt != "" {
			return fmt.Errorf("application still has an owned attempt")
		}
		s := &Store{dir: r.dir, identity: r.journal.After, config: r.config}
		owner, err := s.readOwnership(p)
		if err != nil {
			return err
		}
		if owner.Generations[generation] != "revoked" {
			return fmt.Errorf("message generation is not revoked")
		}
		if phase == "none" {
			state := "cancelled"
			if d.Decision.Action == "retry" {
				state = "fenced"
			}
			record.Application = RecoveryDispositionApplication{Version: 1, Decision: d, Ownership: ownership, State: state, AppliedAt: r.now().UTC()}
			if err = record.Application.Validate(); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(record.Application)
		if err != nil {
			return err
		}
		if err = r.capacity(tx, len(raw)+8192); err != nil {
			return err
		}
		if phase == "none" {
			if err = r.stage.WriteJSON(applicationFile(p, id), record.Application, false); err != nil {
				return err
			}
		}
		if err = recoveryBoundary(r.fault, "application_intent"); err != nil {
			return err
		}
		if err = check(); err != nil {
			return err
		}
		if proof, err := r.readRecoveryOwnership(p, d.Decision.SummaryHash); err != nil {
			return err
		} else if proof != ownership {
			return fmt.Errorf("application ownership changed")
		}
		if _, err = tx.Exec("UPDATE messages SET state=? WHERE producer=? AND id=?", record.Application.State, p.ID, id); err != nil {
			return err
		}
		receipt := protocol.Receipt{EventID: id, Sequence: seq, State: record.Application.State, At: record.Application.AppliedAt}
		raw, err = json.Marshal(receipt)
		if err != nil {
			return err
		}
		insert, err := tx.Exec("INSERT INTO receipts(producer,body) VALUES(?,?)", p.ID, string(raw))
		if err != nil {
			return err
		}
		if record.ReceiptPosition, err = insert.LastInsertId(); err != nil {
			return err
		}
		raw, err = json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO reconciliation_applications VALUES(?,?,?,?,?)", p.ID, id, record.ReceiptPosition, protocol.Hash(record), string(raw)); err != nil {
			return err
		}
		if err = recoveryBoundary(r.fault, "application_transaction"); err != nil {
			return err
		}
		return check()
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "application_commit")
	}
	return
}

func (r *recoveryReconciler) RecoveryMessageApplication(ctx context.Context, p protocol.Producer, id string) (record RecoveryAppliedDisposition, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	if !protocol.ValidToken(id) {
		return record, fmt.Errorf("invalid recovery message")
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		if _, phase, err := r.dispositionEvidence(tx, p, id); err != nil {
			return err
		} else if phase != "recorded" {
			return protocol.ErrRecoveryIncomplete
		}
		var phase string
		record, phase, err = r.applicationEvidence(tx, p, id)
		if err == nil && phase != "applied" {
			return protocol.ErrRecoveryIncomplete
		}
		return err
	})
	return
}
