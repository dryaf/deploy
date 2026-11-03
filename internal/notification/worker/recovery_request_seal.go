package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type RequestRecoverySession interface {
	SealRecoveryRequests(context.Context) (RecoveryRequestSummary, error)
	Close() error
}

func (v RecoveryRequestSummary) Validate() error {
	if v.Version != 1 || !protocol.ValidToken(v.Operation) || !v.Identity.Valid() || v.IntakeEnabled || v.Requests > 65536 || v.InboxRows > 65536 || v.Mapped > v.Requests || v.Unmapped != v.Requests-v.Mapped || v.Opaque > v.InboxRows || v.MissingSignalRows > v.InboxRows || !safefs.DigestPattern.MatchString(v.EvidenceDigest) || !safefs.DigestPattern.MatchString(v.RequestsDigest) {
		return fmt.Errorf("invalid recovery request seal")
	}
	return nil
}

func OpenRequestRecoverySession(ctx context.Context, path string, c Config) (RequestRecoverySession, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	if err = r.initializeRequestRecovery(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func requestRecoverySchema(tx *sql.Tx) (bool, error) {
	var version string
	err := tx.QueryRow("SELECT value FROM metadata WHERE key='request_recovery_schema'").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	var n int
	if e := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('reconciliation_request_fences','reconciliation_request_seal')").Scan(&n); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && n == 0 {
		return false, nil
	}
	if err != nil || version != "1" || n != 2 {
		return false, fmt.Errorf("request recovery schema missing or changed")
	}
	return true, nil
}
func (r *recoveryReconciler) initializeRequestRecovery(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := requestRecoverySchema(tx)
		if err != nil || exists {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_request_fences(key TEXT PRIMARY KEY,hash TEXT NOT NULL,body TEXT NOT NULL,target_id TEXT NOT NULL);
CREATE INDEX reconciliation_request_target ON reconciliation_request_fences(target_id);
CREATE TABLE reconciliation_request_seal(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL);
INSERT INTO metadata VALUES('request_recovery_schema','1');`)
		return err
	})
}

// Seal only after importing the available command and provider mappings. It
// freezes this inventory, not provider coverage: cutoffs/handoffs and activation
// remain separate requirements. Unmapped inputs acquire no command or reply.
func (r *recoveryReconciler) SealRecoveryRequests(ctx context.Context) (summary RecoveryRequestSummary, err error) {
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		ok, err := requestRecoverySchema(tx)
		if err != nil {
			return err
		}
		if !ok {
			return protocol.ErrRecoveryIncomplete
		}
		var records []recoveredRequest
		summary, records, err = r.requestInventory(tx)
		if err != nil {
			return err
		}
		if err = summary.Validate(); err != nil {
			return err
		}
		var prior string
		err = tx.QueryRow("SELECT body FROM reconciliation_request_seal WHERE singleton=1").Scan(&prior)
		if err == nil {
			return nil
		} // transaction guard verified the completed seal.
		if err != sql.ErrNoRows {
			return err
		}
		bytes := 16384
		for _, v := range records {
			b, e := json.Marshal(v)
			if e != nil {
				return e
			}
			bytes += len(b) + 256
		}
		if err = r.capacity(tx, bytes); err != nil {
			return err
		}
		var intent RecoveryRequestSummary
		err = r.stage.ReadJSON("request-seal.json", &intent)
		if os.IsNotExist(err) {
			if err = r.stage.WriteJSON("request-seal.json", summary, false); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if protocol.Hash(intent) != protocol.Hash(summary) {
			return fmt.Errorf("request recovery intent changed")
		}
		if err = recoveryBoundary(r.fault, "request_seal_intent"); err != nil {
			return err
		}
		if err = r.checkAuthority(); err != nil {
			return err
		}
		for _, v := range records {
			body, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO reconciliation_request_fences VALUES(?,?,?,?)", v.Key, protocol.Hash(v), string(body), v.TargetID); err != nil {
				return err
			}
		}
		body, err := json.Marshal(summary)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO reconciliation_request_seal VALUES(1,?)", string(body)); err != nil {
			return err
		}
		return recoveryBoundary(r.fault, "request_seal_transaction")
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "request_seal_commit")
	}
	return
}

// All offline recovery transactions enforce a published intent, including after
// a process crash. Later imports cannot silently invalidate the sealed inventory.
func (r *recoveryReconciler) checkRequestSeal(tx *sql.Tx) error {
	exists, err := requestRecoverySchema(tx)
	if err != nil {
		return err
	}
	var intent RecoveryRequestSummary
	fileErr := r.stage.ReadJSON("request-seal.json", &intent)
	if !exists {
		if os.IsNotExist(fileErr) {
			return nil
		}
		return fmt.Errorf("request seal lost its schema")
	}
	var raw string
	dbErr := tx.QueryRow("SELECT body FROM reconciliation_request_seal WHERE singleton=1").Scan(&raw)
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return dbErr
	}
	var count uint64
	if err = tx.QueryRow("SELECT count(*) FROM reconciliation_request_fences").Scan(&count); err != nil {
		return err
	}
	if os.IsNotExist(fileErr) {
		if dbErr == sql.ErrNoRows && count == 0 {
			return nil
		}
		return fmt.Errorf("request seal lost its intent")
	}
	if fileErr != nil {
		return fileErr
	}
	if err = intent.Validate(); err != nil {
		return err
	}
	current, records, err := r.requestInventory(tx)
	if err != nil {
		return err
	}
	if protocol.Hash(current) != protocol.Hash(intent) {
		return fmt.Errorf("sealed recovery request inventory changed")
	}
	if dbErr == sql.ErrNoRows {
		if count == 0 {
			return nil
		}
		return fmt.Errorf("partial request seal")
	}
	var completed RecoveryRequestSummary
	if err = protocol.Decode([]byte(raw), &completed, 8192); err != nil {
		return err
	}
	if protocol.Hash(completed) != protocol.Hash(intent) || count != completed.Requests {
		return fmt.Errorf("request seal completion changed")
	}
	for _, v := range records {
		var hash, body, target string
		if err = tx.QueryRow("SELECT hash,body,target_id FROM reconciliation_request_fences WHERE key=?", v.Key).Scan(&hash, &body, &target); err != nil {
			return err
		}
		var saved recoveredRequest
		if err = protocol.Decode([]byte(body), &saved, 4096); err != nil {
			return err
		}
		if saved != v || hash != protocol.Hash(v) || target != v.TargetID {
			return fmt.Errorf("recovery request fence changed")
		}
	}
	return nil
}
