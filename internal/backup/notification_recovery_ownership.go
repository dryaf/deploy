package backup

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

type backupRecoveryOwnershipIntent struct {
	Version                int
	Binding                backupNotificationBinding
	Operation, SummaryHash string
	After                  notify.Identity
}

type backupRecoveryOwnershipRecord struct {
	Intent    backupRecoveryOwnershipIntent
	Ownership notify.RecoveryOwnership
}

func (i backupRecoveryOwnershipIntent) matches(p notify.RecoveryOwnership) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if i.Version != 1 || p.Operation != i.Operation || p.Before != i.Binding.Identity ||
		p.After != i.After || p.Producer != i.Binding.Producer || p.SummaryHash != i.SummaryHash {
		return fmt.Errorf("backup ownership preparation binding changed")
	}
	return nil
}

func backupRecoveryOwnershipAuthority(tx *sql.Tx, c WorkerInstallation) (intent backupRecoveryOwnershipIntent, err error) {
	j, err := readBackupRecoveryAuthority(tx, c)
	if err != nil {
		return
	}
	var summary notify.RecoverySummary
	if err = backupEvaluationMeta(tx, "notification_recovery_summary", &summary); err != nil {
		return
	}
	if err = validateBackupRecoverySummary(tx, j, summary); err != nil {
		return
	}
	intent = backupRecoveryOwnershipIntent{Version: 1, Binding: j.Binding, Operation: j.Challenge.Operation,
		After: j.Challenge.Identity, SummaryHash: notify.Hash(summary)}
	return
}

func readBackupRecoveryOwnership(tx *sql.Tx, want backupRecoveryOwnershipIntent) (record backupRecoveryOwnershipRecord, err error) {
	if err = backupNotificationMeta(tx, "notification_recovery_ownership", &record); err != nil {
		return
	}
	if record.Intent != want {
		return record, fmt.Errorf("backup ownership completion changed")
	}
	err = want.matches(record.Ownership)
	return
}

// This internal handoff requires completed reconciliation on both sides. The
// caller retains the offline worker session throughout. Neither its intent nor
// its completion changes the producer's active binding/cursor or delivery fence.
func prepareBackupRecoveryOwnership(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoveryOwnershipSession) (backupRecoveryOwnershipRecord, error) {
	return prepareBackupRecoveryOwnershipWith(ctx, s, c, session, nil)
}

func prepareBackupRecoveryOwnershipWith(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoveryOwnershipSession, fault func(string) error) (result backupRecoveryOwnershipRecord, err error) {
	if session == nil {
		return result, fmt.Errorf("backup ownership recovery session missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	check, release, err := lockBackupRecovery(ctx, s)
	if err != nil {
		return result, err
	}
	defer release()
	var want backupRecoveryOwnershipIntent
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if err := check(); err != nil {
			return err
		}
		var err error
		want, err = backupRecoveryOwnershipAuthority(tx, c)
		return err
	})
	if err != nil {
		return
	}
	// No transaction crosses a worker call. An unavailable or changed completion
	// must not be interpreted as permission to begin a different exchange.
	summary, err := session.Summary(ctx, want.Binding.Producer)
	if err != nil {
		return result, err
	}
	if notify.Hash(summary) != want.SummaryHash {
		return result, fmt.Errorf("worker ownership recovery summary changed")
	}
	validate := func(tx *sql.Tx) error {
		if err := check(); err != nil {
			return err
		}
		current, err := backupRecoveryOwnershipAuthority(tx, c)
		if err != nil {
			return err
		}
		if current != want {
			return fmt.Errorf("backup recovery authority changed during handoff")
		}
		return nil
	}
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if err := validate(tx); err != nil {
			return err
		}
		var intent backupRecoveryOwnershipIntent
		intentErr := backupNotificationMeta(tx, "notification_recovery_ownership_intent", &intent)
		if intentErr != nil && intentErr != sql.ErrNoRows {
			return intentErr
		}
		_, recordErr := readBackupRecoveryOwnership(tx, want)
		if recordErr != nil && recordErr != sql.ErrNoRows {
			return recordErr
		}
		if intentErr == nil {
			if intent != want {
				return fmt.Errorf("backup ownership intent changed")
			}
		} else {
			if recordErr == nil {
				return fmt.Errorf("backup ownership completion lost its intent")
			}
			if err := storeBackupEvaluationMeta(tx, "notification_recovery_ownership_intent", want); err != nil {
				return err
			}
		}
		if err := backupRecoveryBoundary(fault, "ownership_intent_transaction"); err != nil {
			return err
		}
		return check()
	})
	if err != nil {
		return
	}
	if err = backupRecoveryBoundary(fault, "ownership_intent_commit"); err != nil {
		return
	}
	if err = check(); err != nil {
		return
	}
	proof, err := session.PrepareRecoveryOwnership(ctx, want.Binding.Producer)
	if err != nil {
		return result, err
	}
	if err = want.matches(proof); err != nil {
		return
	}
	if err = backupRecoveryBoundary(fault, "ownership_worker_prepared"); err != nil {
		return
	}
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if err := validate(tx); err != nil {
			return err
		}
		var intent backupRecoveryOwnershipIntent
		if err := backupNotificationMeta(tx, "notification_recovery_ownership_intent", &intent); err != nil {
			return err
		}
		if intent != want {
			return fmt.Errorf("backup ownership intent changed during handoff")
		}
		previous, err := readBackupRecoveryOwnership(tx, want)
		if err == nil {
			if previous.Ownership != proof {
				return fmt.Errorf("worker ownership preparation changed")
			}
			result = previous
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		result = backupRecoveryOwnershipRecord{Intent: want, Ownership: proof}
		if err = storeBackupEvaluationMeta(tx, "notification_recovery_ownership", result); err != nil {
			return err
		}
		if err = backupRecoveryBoundary(fault, "ownership_record_transaction"); err != nil {
			return err
		}
		return check()
	})
	if err == nil {
		err = backupRecoveryBoundary(fault, "ownership_record_commit")
	}
	return
}
