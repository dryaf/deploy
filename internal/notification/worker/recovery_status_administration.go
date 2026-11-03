package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Audit validation is deliberately read-only. A committed decision whose result
// file was not published is pending acknowledgment, not a decision to replay.
func (r *recoveryReconciler) reviewRecoveryAdministration(ctx context.Context, target *sql.Tx, out *RecoveryStatus) error {
	if out.OriginalSourceHash == "" {
		out.add("worker", "administrative_audit", "pending", "")
		return nil
	}
	source, err := r.originalDB(ctx, target)
	if err != nil {
		return err
	}
	defer source.Close()
	tx, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = verifyAdministrativeHistory(ctx, tx, r.journal.Before); err != nil {
		return err
	}
	if err = verifyAdministrativeHistory(ctx, target, r.journal.After); err != nil {
		return err
	}
	keys, err := administrativeKeys(ctx, tx)
	if err != nil {
		return err
	}
	type digest struct{ Key, Hash string }
	var evidence []digest
	for _, key := range keys {
		var body, archived string
		if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&body); err != nil {
			return err
		}
		var v MessageDecisionRecord
		if err = protocol.Decode([]byte(body), &v, 2*protocol.MaxRequest); err != nil {
			return err
		}
		if err = target.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", archivedMessageDecisionKey(v)).Scan(&archived); err != nil {
			return err
		}
		if archived != body {
			return fmt.Errorf("recovered administrative audit differs from retained original")
		}
		evidence = append(evidence, digest{key, protocol.Hash(body)})
	}
	want := protocol.Hash(struct {
		Binding recoveryBinding
		Records []digest
	}{r.journal.binding(), evidence})
	var ack string
	if err = target.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", "message-decision-recovery-source-v1:"+r.journal.Operation).Scan(&ack); err != nil {
		return err
	}
	if ack != want {
		return fmt.Errorf("administrative reconciliation acknowledgment changed")
	}
	a := messageAdministrator{s: &Store{dir: r.dir, identity: r.journal.After}}
	ids, err := a.historyFiles()
	if err != nil {
		return err
	}
	keys, err = administrativeKeys(ctx, target)
	if err != nil {
		return err
	}
	for _, key := range keys {
		ids[key[strings.LastIndex(key, ":")+1:]] = true
	}
	if len(ids) > maxMessageDecisions {
		return fmt.Errorf("administrative review exceeds history budget")
	}
	pendingResults, pendingIntents := false, false
	for id := range ids {
		_, exists, e := a.readRecord(ctx, target, id)
		if e != nil {
			return e
		}
		var intent MessageDecision
		if e = readCheckpointJSON(r.dir, messageIntentFile(id), &intent); e != nil {
			return e
		}
		if e = intent.Validate(); e != nil {
			return e
		}
		if intent.Review.ID != id || intent.Review.Identity.Worker != r.journal.After.Worker {
			return fmt.Errorf("administrative review intent belongs to another worker")
		}
		var result MessageDecisionRecord
		e = readCheckpointJSON(r.dir, messageResultFile(id), &result)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		pendingResults = pendingResults || (exists && os.IsNotExist(e))
		pendingIntents = pendingIntents || !exists
	}
	out.add("worker", "administrative_audit", "complete", want)
	if pendingResults {
		out.add("worker", "administrative_acknowledgments", "pending", "")
	}
	if pendingIntents {
		out.add("worker", "administrative_intents", "retained_fenced", "")
	}
	return tx.Commit()
}
