package worker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Recover audit from both the snapshot and preserved latest database. Later
// operator decisions remain historical evidence; this never reapplies them to
// messages, creates receipts, grants retries or adopts old acknowledgement IDs.
func (r *recoveryReconciler) reconcileAdministrativeAudit(ctx context.Context) error {
	f, err := snapshotFile(r.dir, "state.db", r.config.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	f.Close()
	source, err := snapshotDB(r.dir, "state.db", true)
	if err != nil {
		return err
	}
	defer source.Close()
	tx, err := source.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rawIdentity string
	if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&rawIdentity); err != nil {
		return err
	}
	var sourceIdentity protocol.Identity
	if err = protocol.Decode([]byte(rawIdentity), &sourceIdentity, 4096); err != nil {
		return err
	}
	if sourceIdentity != r.journal.Before {
		return fmt.Errorf("preserved administrative source identity changed")
	}
	if err = verifyAdministrativeHistory(ctx, tx, r.journal.Before); err != nil {
		return err
	}
	keys, err := administrativeKeys(ctx, tx)
	if err != nil {
		return err
	}
	a := messageAdministrator{s: &Store{db: source, dir: r.dir, identity: r.journal.Before}}
	files, err := a.historyFiles()
	if err != nil {
		return err
	}
	// Include SQL keys even when the independent directory is entirely missing:
	// later decisions must not disappear just because the snapshot predates them.
	for _, key := range keys {
		files[key[strings.LastIndex(key, ":")+1:]] = true
	}
	for id := range files {
		if _, exists, e := a.readRecord(ctx, tx, id); e != nil {
			return e
		} else if !exists {
			var intent MessageDecision
			if e = r.dir.ReadJSON(messageIntentFile(id), &intent); e != nil {
				return e
			}
			if e = intent.Validate(); e != nil {
				return e
			}
			if intent.Review.ID != id || intent.Review.Identity.Worker != r.journal.Before.Worker {
				return fmt.Errorf("preserved administrative intent identity changed")
			}
		}
	}
	type digest struct{ Key, Hash string }
	var evidence []digest
	for _, key := range keys {
		var body string
		if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&body); err != nil {
			return err
		}
		evidence = append(evidence, digest{key, protocol.Hash(body)})
	}
	hash := protocol.Hash(struct {
		Binding recoveryBinding
		Records []digest
	}{r.journal.binding(), evidence})
	return r.transaction(ctx, func(target *sql.Tx) error {
		fingerprintKey := "message-decision-recovery-source-v1:" + r.journal.Operation
		var stored string
		e := target.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", fingerprintKey).Scan(&stored)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		initialized := e == nil
		if initialized && stored != hash {
			return fmt.Errorf("preserved administrative recovery source changed")
		}
		if e = verifyAdministrativeHistory(ctx, target, r.journal.After); e != nil {
			return e
		}
		for _, key := range keys {
			var body string
			if e = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&body); e != nil {
				return e
			}
			var v MessageDecisionRecord
			if e = protocol.Decode([]byte(body), &v, 2*protocol.MaxRequest); e != nil {
				return e
			}
			var old string
			e = target.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", archivedMessageDecisionKey(v)).Scan(&old)
			if e == nil {
				if old != body {
					return fmt.Errorf("conflicting administrative recovery audit")
				}
				continue
			}
			if e != sql.ErrNoRows {
				return e
			}
			if initialized {
				return fmt.Errorf("reconciled administrative history was rolled back")
			}
			if e = r.capacity(target, len(body)+8192); e != nil {
				return e
			}
			if _, e = target.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?)", archivedMessageDecisionKey(v), body); e != nil {
				return e
			}
		}
		if e = verifyAdministrativeHistory(ctx, target, r.journal.After); e != nil {
			return e
		}
		if !initialized {
			_, e = target.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?)", fingerprintKey, hash)
			return e
		}
		return nil
	})
}
