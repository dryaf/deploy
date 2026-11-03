package interaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Ledger shares the specialist worker's own durable database. Apply callbacks
// may change domain tables or admit a durable job in the supplied transaction;
// they must never perform external I/O. A job executor reconciles external effects.
type Ledger struct {
	db       *sql.DB
	producer protocol.Producer
	identity protocol.Identity
}

func OpenLedger(ctx context.Context, db *sql.DB, p protocol.Producer, id protocol.Identity, create bool) (*Ledger, error) {
	return OpenDomainLedger(ctx, db, p, id, create, nil)
}

// OpenDomainLedger initializes specialist tables in the same transaction as the
// command ledger and identity pin. Normal opens never reconstruct missing state.
func OpenDomainLedger(ctx context.Context, db *sql.DB, p protocol.Producer, id protocol.Identity, create bool, initialize func(*sql.Tx) error) (*Ledger, error) {
	if !p.Valid() || !id.Valid() {
		return nil, fmt.Errorf("invalid ledger identity")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if create {
		if _, err = tx.Exec(`
CREATE TABLE interactive_identity(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL);
CREATE TABLE interactive_inbox(id TEXT PRIMARY KEY,request_key TEXT NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL,grant_id TEXT NOT NULL DEFAULT '',fenced INTEGER NOT NULL DEFAULT 0,revision INTEGER NOT NULL DEFAULT 0,phase TEXT NOT NULL DEFAULT '',state TEXT NOT NULL DEFAULT 'imported');
CREATE TABLE interactive_results(id TEXT NOT NULL,revision INTEGER NOT NULL,body TEXT NOT NULL,receipt TEXT NOT NULL DEFAULT '',acked INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(id,revision));`); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("CREATE TABLE interactive_non_admission(id TEXT PRIMARY KEY,body TEXT NOT NULL)"); err != nil {
			return nil, err
		}
		if initialize != nil {
			if err = initialize(tx); err != nil {
				return nil, err
			}
		}
	}
	pin := ledgerIdentityPin{Producer: p, Identity: id}
	encoded, _ := json.Marshal(pin)
	var old string
	err = tx.QueryRow("SELECT body FROM interactive_identity WHERE singleton=1").Scan(&old)
	if err == sql.ErrNoRows && create {
		_, err = tx.Exec("INSERT INTO interactive_identity VALUES(1,?)", string(encoded))
	} else if err == nil && old != string(encoded) {
		var saved ledgerIdentityPin
		if create || protocol.Decode([]byte(old), &saved, 4096) != nil || saved.Producer != p || saved.Identity != id || saved.RecoveryOperation == "" {
			return nil, fmt.Errorf("ledger identity changed; explicit reconciliation required")
		}
		if err = (&Ledger{db: db, producer: p, identity: id}).checkRecoveryPin(tx); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Ledger{db: db, producer: p, identity: id}, nil
}

func (l *Ledger) Import(ctx context.Context, c Command) error {
	if c.Context != nil {
		if !protocol.ValidToken(c.Context.Reference) || c.Context.Context.Validate() != nil {
			return fmt.Errorf("invalid command context")
		}
		for _, target := range c.Context.Context.Targets {
			if !c.Scope.Contains(target.Subject) {
				return fmt.Errorf("context outside command scope")
			}
		}
		if c.Descriptor.TargetRequired {
			if len(c.Context.Context.Targets) != 1 {
				return fmt.Errorf("target command has ambiguous context")
			}
			target := c.Context.Context.Targets[0]
			if c.Arguments["subject"] != target.Subject || c.Arguments["target"] != target.ID {
				return fmt.Errorf("command target and context disagree")
			}
		}
	}
	if c.Producer != l.producer || c.Scope.Validate() != nil || !protocol.ValidToken(c.ID) || c.RequestKey == "" || c.InputHash == "" || c.Deadline.IsZero() {
		return fmt.Errorf("invalid imported command")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return err
	}
	var hash string
	err = tx.QueryRow("SELECT hash FROM interactive_inbox WHERE id=?", c.ID).Scan(&hash)
	if err == nil {
		if hash != protocol.Hash(c) {
			return fmt.Errorf("command hash conflict")
		}
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	body, _ := json.Marshal(c)
	if _, err = tx.Exec("INSERT INTO interactive_inbox(id,request_key,hash,body) VALUES(?,?,?,?)", c.ID, c.RequestKey, protocol.Hash(c), string(body)); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Ledger) Result(ctx context.Context, id string, revision uint64) (r Result, err error) {
	var body string
	err = l.db.QueryRowContext(ctx, "SELECT body FROM interactive_results WHERE id=? AND revision=?", id, revision).Scan(&body)
	if err == nil {
		err = protocol.Decode([]byte(body), &r, protocol.MaxResponse)
	}
	return
}

// Execute runs admission once. The caller must revalidate the command's current
// domain preconditions and local authorization inside apply, before any writes.
// A duplicate returns retained evidence without rerunning apply, even after the
// grant deadline has elapsed.
func (l *Ledger) Execute(ctx context.Context, c Command, g Grant, preparedHash string, now time.Time, apply func(*sql.Tx, Command) (Result, error)) (result Result, err error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return result, err
	}
	var hash, phase, state string
	var rev uint64
	var fenced int
	err = tx.QueryRow("SELECT hash,phase,state,revision,fenced FROM interactive_inbox WHERE id=?", c.ID).Scan(&hash, &phase, &state, &rev, &fenced)
	if err != nil {
		return result, err
	}
	if c.Producer != l.producer || hash != protocol.Hash(c) {
		return result, fmt.Errorf("command mismatch")
	}
	if phase == "execute" {
		var body string
		if err = tx.QueryRow("SELECT body FROM interactive_results WHERE id=? AND revision=?", c.ID, rev).Scan(&body); err != nil {
			return result, err
		}
		err = protocol.Decode([]byte(body), &result, protocol.MaxResponse)
		return result, err
	}
	if fenced != 0 || Terminal(state) || g.Identity != l.identity || g.CommandID != c.ID || g.CommandHash != hash || !protocol.ValidToken(g.ID) || !g.ExpiresAt.After(now) || g.ExpiresAt.After(now.Add(10*time.Second)) || !c.Deadline.After(now) || g.ExpiresAt.After(c.Deadline) {
		return result, fmt.Errorf("execution grant unavailable")
	}
	if c.Descriptor.Confirm {
		if phase != "prepare" || g.PreparationHash == "" || g.PreparationHash != preparedHash {
			return result, fmt.Errorf("preparation mismatch")
		}
		var body string
		var prepared Result
		if err = tx.QueryRow("SELECT body FROM interactive_results WHERE id=? AND revision=?", c.ID, rev).Scan(&body); err != nil {
			return result, err
		}
		if err = protocol.Decode([]byte(body), &prepared, protocol.MaxResponse); err != nil {
			return result, err
		}
		if prepared.Preparation == nil || prepared.Preparation.Hash != preparedHash || !prepared.Preparation.ExpiresAt.After(now) {
			return result, fmt.Errorf("prepared evidence expired")
		}
	} else if g.PreparationHash != "" {
		return result, fmt.Errorf("unexpected preparation")
	}
	result, err = apply(tx, c)
	if err != nil {
		return result, err
	}
	if result.State != "succeeded" && result.State != "failed" && result.State != "running" {
		return result, fmt.Errorf("invalid admission outcome")
	}
	if !c.Scope.Allows(result.Scope) {
		return result, fmt.Errorf("domain result exceeds scope")
	}
	result.CommandID = c.ID
	result.CommandHash = hash
	result.Revision = rev + 1
	result.Phase = "execute"
	if result.Preparation != nil {
		return result, fmt.Errorf("unexpected preparation result")
	}
	if err = storeLedgerResult(tx, result); err != nil {
		return result, err
	}
	if _, err = tx.Exec("UPDATE interactive_inbox SET phase='execute',state=?,revision=?,grant_id=? WHERE id=?", result.State, result.Revision, g.ID, c.ID); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	return
}

// Prepare persists a side-effect-free proposal. It runs no domain callback; the
// worker supplies a proposal computed under its own evidence/policy checks.
func (l *Ledger) Prepare(ctx context.Context, c Command, p Preparation, now time.Time) (r Result, err error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return r, err
	}
	var hash, phase, state string
	var revision uint64
	if err = tx.QueryRow("SELECT hash,phase,state,revision FROM interactive_inbox WHERE id=?", c.ID).Scan(&hash, &phase, &state, &revision); err != nil {
		return r, err
	}
	if c.Producer != l.producer || hash != protocol.Hash(c) || !c.Descriptor.Confirm {
		return r, fmt.Errorf("preparation not applicable")
	}
	if phase != "" {
		var body string
		if err = tx.QueryRow("SELECT body FROM interactive_results WHERE id=? AND revision=?", c.ID, revision).Scan(&body); err == nil {
			err = protocol.Decode([]byte(body), &r, protocol.MaxResponse)
		}
		return
	}
	if Terminal(state) || p.Hash == "" || !p.ExpiresAt.After(now) || p.ExpiresAt.After(now.Add(10*time.Minute)) || p.ExpiresAt.After(c.Deadline) {
		return r, fmt.Errorf("invalid preparation")
	}
	r = Result{CommandID: c.ID, CommandHash: hash, Revision: 1, Phase: "prepare", State: "needs_confirmation", Scope: c.Scope, Preparation: &p, Text: p.Summary}
	if err = storeLedgerResult(tx, r); err != nil {
		return r, err
	}
	if _, err = tx.Exec("UPDATE interactive_inbox SET phase='prepare',state='needs_confirmation',revision=1 WHERE id=?", c.ID); err != nil {
		return r, err
	}
	err = tx.Commit()
	return
}
func storeLedgerResult(tx *sql.Tx, r Result) error {
	if err := r.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(body) > protocol.MaxRequest {
		return fmt.Errorf("result exceeds wire budget")
	}
	_, err = tx.Exec("INSERT INTO interactive_results(id,revision,body) VALUES(?,?,?)", r.CommandID, r.Revision, string(body))
	return err
}

// FenceAdmission durably proves no execution admission occurred. Transactions
// serialize this tombstone against Execute; an old grant cannot race through it.
func (l *Ledger) FenceAdmission(ctx context.Context, id string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return err
	}
	var phase string
	if err = tx.QueryRow("SELECT phase FROM interactive_inbox WHERE id=?", id).Scan(&phase); err != nil {
		return err
	}
	if phase == "execute" {
		return fmt.Errorf("execution already admitted")
	}
	if _, err = tx.Exec("UPDATE interactive_inbox SET fenced=1,state='expired' WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Ledger) ImportReceipt(ctx context.Context, receipt ResultReceipt) error {
	if receipt.Identity != l.identity || receipt.Producer != l.producer {
		return fmt.Errorf("result receipt identity mismatch")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return err
	}
	var body, old string
	if err = tx.QueryRow("SELECT body,receipt FROM interactive_results WHERE id=? AND revision=?", receipt.CommandID, receipt.Revision).Scan(&body, &old); err != nil {
		return err
	}
	var r Result
	if err = protocol.Decode([]byte(body), &r, protocol.MaxResponse); err != nil {
		return err
	}
	if receipt.Hash != protocol.Hash(r) {
		return fmt.Errorf("receipt result mismatch")
	}
	encoded, _ := json.Marshal(receipt)
	if old != "" && old != string(encoded) {
		return fmt.Errorf("receipt conflict")
	}
	switch receipt.Disposition {
	case "superseded_no_effect":
		if (r.Phase != "prepare" && r.State != "rejected") || !receipt.NonAdmission || receipt.TerminalDecisionID == "" || (receipt.TerminalState != "expired" && receipt.TerminalState != "rejected") {
			return fmt.Errorf("invalid supersession receipt")
		}
		var phase string
		if err = tx.QueryRow("SELECT phase FROM interactive_inbox WHERE id=?", r.CommandID).Scan(&phase); err != nil {
			return err
		}
		if phase == "execute" {
			return fmt.Errorf("cannot supersede executed command")
		}
		if _, err = tx.Exec("UPDATE interactive_inbox SET state=?,fenced=1 WHERE id=?", receipt.TerminalState, r.CommandID); err != nil {
			return err
		}
	case "execution_conflict":
		if _, err = tx.Exec("UPDATE interactive_inbox SET fenced=1 WHERE id=?", r.CommandID); err != nil {
			return err
		}
	case "applied":
	default:
		return fmt.Errorf("invalid receipt disposition")
	}
	if _, err = tx.Exec("UPDATE interactive_results SET receipt=? WHERE id=? AND revision=?", string(encoded), receipt.CommandID, receipt.Revision); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteJob records completion only for the exact admitted job. Completion and
// domain bookkeeping must be committed together by the callback.
func (l *Ledger) CompleteJob(ctx context.Context, id, job string, finish func(*sql.Tx) (Result, error)) (result Result, err error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return result, err
	}
	var body string
	if err = tx.QueryRow("SELECT r.body FROM interactive_results r JOIN interactive_inbox i ON i.id=r.id AND i.revision=r.revision WHERE i.id=?", id).Scan(&body); err != nil {
		return result, err
	}
	var previous Result
	if err = protocol.Decode([]byte(body), &previous, protocol.MaxResponse); err != nil {
		return result, err
	}
	if previous.JobID != job || job == "" {
		return result, fmt.Errorf("job mismatch")
	}
	if Terminal(previous.State) {
		return previous, nil
	}
	if previous.State != "running" {
		return result, fmt.Errorf("job not running")
	}
	result, err = finish(tx)
	if err != nil {
		return result, err
	}
	if (result.State != "succeeded" && result.State != "failed") || !previous.Scope.Allows(result.Scope) {
		return result, fmt.Errorf("invalid job completion")
	}
	result.CommandID = id
	result.CommandHash = previous.CommandHash
	result.Revision = previous.Revision + 1
	result.Phase = "execute"
	result.JobID = job
	if err = storeLedgerResult(tx, result); err != nil {
		return result, err
	}
	if _, err = tx.Exec("UPDATE interactive_inbox SET state=?,revision=? WHERE id=?", result.State, result.Revision, id); err != nil {
		return result, err
	}
	err = tx.Commit()
	return
}
