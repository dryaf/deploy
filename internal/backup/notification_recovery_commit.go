package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

type backupRecoveryCommitIntent struct {
	Version   int
	Ownership backupRecoveryOwnershipRecord
}

type backupRecoveryCommitRecord struct {
	Intent       backupRecoveryCommitIntent
	Proof        notify.RecoveryProducerCommit
	BeforeCursor notify.Cursor
	After        backupNotificationBinding
}

func (v backupRecoveryCommitRecord) validate(j backupNotificationRecovery) error {
	if err := v.Proof.Validate(); err != nil {
		return err
	}
	i := v.Intent.Ownership.Intent
	if err := i.matches(v.Intent.Ownership.Ownership); err != nil {
		return err
	}
	want := j.Binding
	want.Identity = v.Proof.Cursor.Identity
	want.Generation = v.Proof.Generation.Generation
	if v.Intent.Version != 1 || i.Binding != j.Binding || i.Operation != j.Challenge.Operation || i.After != j.Challenge.Identity ||
		v.Proof.Generation.Ownership != v.Intent.Ownership.Ownership || v.Proof.HighestIssued != j.Manifest.HighestIssued ||
		v.BeforeCursor != j.Manifest.ImportedReceipts || v.After != want {
		return fmt.Errorf("backup recovery commit mapping changed")
	}
	return nil
}

// Resolve the only permitted post-reconciliation binding. The old journal and
// cursor remain audit evidence; a mapping and its intent must survive together.
func backupRecoveryDeliveryBinding(tx *sql.Tx, j backupNotificationRecovery) (b backupNotificationBinding, cursor int64, err error) {
	b, cursor = j.Binding, j.Manifest.ImportedReceipts.Position
	var v backupRecoveryCommitRecord
	err = backupNotificationMeta(tx, "notification_recovery_commit", &v)
	if err == sql.ErrNoRows {
		return b, cursor, nil
	}
	if err != nil {
		return
	}
	if err = v.validate(j); err != nil {
		return
	}
	var intent backupRecoveryCommitIntent
	if err = backupNotificationMeta(tx, "notification_recovery_commit_intent", &intent); err != nil {
		return
	}
	if intent != v.Intent {
		return b, cursor, fmt.Errorf("backup recovery commit lost its intent")
	}
	var summary notify.RecoverySummary
	if err = backupEvaluationMeta(tx, "notification_recovery_summary", &summary); err != nil {
		return
	}
	if err = validateBackupRecoverySummary(tx, j, summary); err != nil {
		return
	}
	if notify.Hash(summary) != intent.Ownership.Intent.SummaryHash {
		return b, cursor, fmt.Errorf("backup recovery commit summary changed")
	}
	prior, err := readBackupRecoveryOwnership(tx, intent.Ownership.Intent)
	if err != nil {
		return b, cursor, err
	}
	if prior != intent.Ownership {
		return b, cursor, fmt.Errorf("backup recovery ownership proof changed")
	}
	var ownershipIntent backupRecoveryOwnershipIntent
	if err = backupNotificationMeta(tx, "notification_recovery_ownership_intent", &ownershipIntent); err != nil {
		return b, cursor, err
	}
	if ownershipIntent != prior.Intent {
		return b, cursor, fmt.Errorf("backup recovery ownership intent changed")
	}
	var pending int
	if err = tx.QueryRow("SELECT count(*) FROM backup_notification_transfers WHERE submitted<>1 OR cancel_sent<>0").Scan(&pending); err != nil {
		return b, cursor, err
	}
	if pending != 0 {
		return b, cursor, fmt.Errorf("backup recovery transfer commit regressed")
	}
	return v.After, v.Proof.Cursor.Position, nil
}

// Recheck immutable payloads at commit, including recovered submissions that
// never reached the old worker. Domain decisions and pending cancellations may
// still change while fenced; they are intentionally not rewritten here.
func validateBackupCommitPayloads(tx *sql.Tx, j backupNotificationRecovery) error {
	return validateBackupRetainedCommitPayloads(tx, j, false)
}

func validateBackupRetainedCommitPayloads(tx *sql.Tx, j backupNotificationRecovery, active bool) error {
	_, inherited, err := readBackupRecoveryAncestors(tx, j, nil)
	if err != nil {
		return err
	}
	var exports, transfers uint64
	if err := tx.QueryRow("SELECT (SELECT count(*) FROM notification_exports),(SELECT count(*) FROM backup_notification_transfers)").Scan(&exports, &transfers); err != nil {
		return err
	}
	if (!active && (exports != j.Records || transfers != j.Records)) || exports < j.Records || transfers < j.Records {
		return fmt.Errorf("backup recovery commit export membership changed")
	}
	rows, err := tx.Query(`SELECT r.seq,r.hash,r.body,e.id,e.body FROM backup_notification_recovery_records r
 JOIN notification_exports e ON e.seq=r.seq JOIN backup_notification_transfers t ON t.event=e.id ORDER BY r.seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var count uint64
	for rows.Next() {
		var seq uint64
		var hash, raw, id, body string
		if err = rows.Scan(&seq, &hash, &raw, &id, &body); err != nil {
			return err
		}
		var record notify.RecoveryRecord
		if err = notify.Decode([]byte(raw), &record, notify.MaxRequest); err != nil {
			return err
		}
		var message notify.Message
		if err = notify.Decode([]byte(body), &message, notify.MaxRequest); err != nil {
			return err
		}
		if err = message.Validate(); err != nil {
			return err
		}
		count++
		if seq != count || record.Message.Sequence != seq || id != message.ID || hash != notify.Hash(record) ||
			notify.Hash(record.Message) != notify.Hash(message) || message.Producer != j.Binding.Producer || (message.Generation != j.Binding.Generation && notify.Hash(inherited[message.ID]) != notify.Hash(message)) || message.Route != j.Binding.Route {
			return fmt.Errorf("backup recovery payload changed before commit")
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != j.Records {
		return fmt.Errorf("backup recovery commit payload history incomplete")
	}
	return nil
}

// Internal delivery-only commit. The source and worker remain fenced, command
// identity/cursors remain unchanged, and every new-epoch receipt remains unread.
func commitBackupNotificationRecovery(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoveryProducerSession) (backupRecoveryCommitRecord, error) {
	return commitBackupNotificationRecoveryWith(ctx, s, c, session, nil)
}

func commitBackupNotificationRecoveryWith(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoveryProducerSession, fault func(string) error) (result backupRecoveryCommitRecord, err error) {
	if session == nil {
		return result, fmt.Errorf("producer recovery session missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	check, release, err := lockBackupRecovery(ctx, s)
	if err != nil {
		return result, err
	}
	defer release()
	var want backupRecoveryCommitIntent
	validate := func(tx *sql.Tx) (backupNotificationRecovery, error) {
		if err := check(); err != nil {
			return backupNotificationRecovery{}, err
		}
		j, err := readBackupRecoveryAuthority(tx, c)
		if err != nil {
			return j, err
		}
		authority, err := backupRecoveryOwnershipAuthority(tx, c)
		if err != nil {
			return j, err
		}
		ownership, err := readBackupRecoveryOwnership(tx, authority)
		if err != nil {
			return j, err
		}
		var intent backupRecoveryOwnershipIntent
		if err = backupNotificationMeta(tx, "notification_recovery_ownership_intent", &intent); err != nil {
			return j, err
		}
		if intent != authority {
			return j, fmt.Errorf("backup ownership intent changed")
		}
		current := backupRecoveryCommitIntent{Version: 1, Ownership: ownership}
		if want.Version == 0 {
			want = current
		} else if current != want {
			return j, fmt.Errorf("backup commit authority changed")
		}
		return j, validateBackupCommitPayloads(tx, j)
	}
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if _, err := validate(tx); err != nil {
			return err
		}
		var prior backupRecoveryCommitIntent
		err := backupNotificationMeta(tx, "notification_recovery_commit_intent", &prior)
		if err == nil {
			if prior != want {
				return fmt.Errorf("backup recovery commit intent changed")
			}
		} else if err == sql.ErrNoRows {
			if err = storeBackupEvaluationMeta(tx, "notification_recovery_commit_intent", want); err != nil {
				return err
			}
		} else {
			return err
		}
		if err = backupRecoveryBoundary(fault, "producer_intent_transaction"); err != nil {
			return err
		}
		return check()
	})
	if err != nil {
		return
	}
	if err = backupRecoveryBoundary(fault, "producer_intent_commit"); err != nil {
		return
	}
	if err = check(); err != nil {
		return
	}
	summary, err := session.Summary(ctx, want.Ownership.Intent.Binding.Producer)
	if err != nil {
		return result, err
	}
	if notify.Hash(summary) != want.Ownership.Intent.SummaryHash {
		return result, fmt.Errorf("worker producer recovery summary changed")
	}
	generation, err := session.PrepareRecoveryGeneration(ctx, summary.Challenge.Producer)
	if err != nil {
		return result, err
	}
	if generation.Ownership != want.Ownership.Ownership {
		return result, fmt.Errorf("worker producer generation changed ownership")
	}
	proof, err := session.PrepareRecoveryProducerCommit(ctx, summary.Challenge.Producer)
	if err != nil {
		return result, err
	}
	if err = proof.Validate(); err != nil {
		return
	}
	if proof.Generation != generation || proof.HighestIssued != summary.Manifest.HighestIssued {
		return result, fmt.Errorf("worker producer commit proof changed")
	}
	if err = backupRecoveryBoundary(fault, "producer_worker_prepared"); err != nil {
		return
	}
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		j, err := validate(tx)
		if err != nil {
			return err
		}
		var intent backupRecoveryCommitIntent
		if err = backupNotificationMeta(tx, "notification_recovery_commit_intent", &intent); err != nil {
			return err
		}
		if intent != want {
			return fmt.Errorf("backup recovery commit intent changed during handoff")
		}
		if err = interaction.ValidateDeliveryRecoveryCommit(tx, proof); err != nil {
			return err
		}
		after := j.Binding
		after.Identity, after.Generation = proof.Cursor.Identity, proof.Generation.Generation
		result = backupRecoveryCommitRecord{Intent: want, Proof: proof, BeforeCursor: j.Manifest.ImportedReceipts, After: after}
		if err = result.validate(j); err != nil {
			return err
		}
		var previous backupRecoveryCommitRecord
		err = backupNotificationMeta(tx, "notification_recovery_commit", &previous)
		if err == nil {
			if previous != result {
				return fmt.Errorf("backup recovery commit proof changed on replay")
			}
			return check()
		}
		if err != sql.ErrNoRows {
			return err
		}
		body, err := json.Marshal(after)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE backup_notification_binding SET body=? WHERE singleton=1", string(body)); err != nil {
			return err
		}
		identity, err := json.Marshal(after.Identity)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE notification_producer SET identity=?,cursor=0 WHERE singleton=1", string(identity)); err != nil {
			return err
		}
		owner := j.Owner
		owner.Mode, owner.Binding = "fenced", after
		if err = storeBackupEvaluationMeta(tx, "notification_owner", owner); err != nil {
			return err
		}
		// All captured sequences were reconciled at the worker. Do not enqueue their
		// old envelopes again; replay cancellation intents against the new epoch.
		if _, err = tx.Exec("UPDATE backup_notification_transfers SET submitted=1,cancel_sent=0"); err != nil {
			return err
		}
		if err = storeBackupEvaluationMeta(tx, "notification_recovery_commit", result); err != nil {
			return err
		}
		if err = backupRecoveryBoundary(fault, "producer_commit_transaction"); err != nil {
			return err
		}
		return check()
	})
	if err == nil {
		err = backupRecoveryBoundary(fault, "producer_commit")
	}
	return
}
