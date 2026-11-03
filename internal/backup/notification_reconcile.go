package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupRecoveryFinishIntent struct {
	Challenge notify.RecoveryChallenge
	Manifest  notify.RecoveryManifest
	Snapshot  notify.Snapshot
}

// This coordinator exchanges evidence with an already quarantined offline
// session. The caller owns that session. Neither completion nor replay commits a
// new active identity/cursor, changes canonical ownership, or starts dispatch.
func reconcileBackupNotifications(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoverySession) (notify.RecoverySummary, error) {
	return reconcileBackupNotificationsWith(ctx, s, c, session, reportHostClock, nil)
}

func reconcileBackupNotificationsWith(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoverySession, clock func() backupEvaluationClock, fault func(string) error) (summary notify.RecoverySummary, err error) {
	if session == nil || clock == nil {
		return summary, fmt.Errorf("backup recovery dependencies missing")
	}
	dir, err := safefs.Open(s.directory)
	if err != nil {
		return summary, err
	}
	defer dir.Close()
	lease, err := dir.Lock("notification-recovery.lock", true, true)
	if err != nil {
		return summary, err
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var owner backupNotificationOwner
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		var err error
		owner, err = readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		return requireBackupRecoveryConfig(tx, owner.Binding, c)
	})
	if err != nil {
		return summary, err
	}
	// Check completion first. In particular, a lost Finish response must not be
	// mistaken for permission to replace its already committed status evidence.
	summary, err = session.Summary(ctx, owner.Binding.Producer)
	if err == nil {
		err = recordBackupRecoverySummary(ctx, s, c, summary, fault)
		return
	}
	if !errors.Is(err, notify.ErrRecoveryIncomplete) {
		return summary, err
	}
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_summary'").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("gateway lost a locally recorded recovery completion")
		}
		return nil
	})
	if err != nil {
		return summary, err
	}
	ch, err := session.Challenge(ctx, owner.Binding.Producer)
	if err != nil {
		return summary, err
	}
	j, err := beginBackupNotificationRecoveryWith(ctx, s, ch, clock(), fault)
	if err != nil {
		return summary, err
	}
	if err = session.Begin(ctx, ch, j.Manifest); err != nil {
		return summary, err
	}
	var after uint64
	for {
		page, err := readBackupNotificationRecoveryPage(ctx, s, ch, after, clock())
		if err != nil {
			return summary, err
		}
		if len(page.Records) != 0 {
			if err = session.Import(ctx, ch, page.Records); err != nil {
				return summary, err
			}
		}
		if page.Done {
			break
		}
		after = page.Next
	}
	intent, err := prepareBackupRecoveryFinish(ctx, s, c, ch, clock(), fault)
	if err != nil {
		return summary, err
	}
	summary, err = session.Finish(ctx, ch, intent.Snapshot)
	if err != nil {
		return summary, err
	}
	err = recordBackupRecoverySummary(ctx, s, c, summary, fault)
	return
}

func requireBackupRecoveryConfig(tx *sql.Tx, b backupNotificationBinding, c WorkerInstallation) error {
	control, err := incidentControl(tx)
	if err != nil {
		return err
	}
	if c.WorkerID != b.Producer.ID || c.ArchiveID != b.Archive || c.ConfigHash != control.ConfigHash {
		return fmt.Errorf("backup recovery configuration mismatch")
	}
	return nil
}

// Match local authority even for an old completed exchange. Time expiry does not
// erase audit evidence, but callers needing current evidence separately validate
// the challenge against a fresh host clock.
func readBackupRecoveryAuthority(tx *sql.Tx, c WorkerInstallation) (j backupNotificationRecovery, err error) {
	owner, err := readBackupNotificationOwner(tx)
	if err != nil {
		return j, err
	}
	if err = requireBackupRecoveryConfig(tx, owner.Binding, c); err != nil {
		return j, err
	}
	if err = backupNotificationMeta(tx, "notification_recovery", &j); err != nil {
		return j, err
	}
	expected, _, err := backupRecoveryDeliveryBinding(tx, j)
	if err != nil {
		return j, err
	}
	if owner.Mode != "fenced" || owner.Binding != expected || owner.Socket != j.Owner.Socket {
		return j, fmt.Errorf("backup recovery ownership changed")
	}
	err = verifyBackupRecoveryExport(tx, j)
	return
}

func prepareBackupRecoveryFinish(ctx context.Context, s *backupState, c WorkerInstallation, ch notify.RecoveryChallenge, clock backupEvaluationClock, fault func(string) error) (intent backupRecoveryFinishIntent, err error) {
	lease, err := backupNotifierLease(s)
	if err != nil {
		return intent, err
	}
	defer lease.Close()
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		j, err := readBackupRecoveryAuthority(tx, c)
		if err != nil {
			return err
		}
		if notify.Hash(ch) != notify.Hash(j.Challenge) {
			return fmt.Errorf("backup recovery finish challenge mismatch")
		}
		if err = validateBackupRecoveryChallenge(j.Binding, ch, clock); err != nil {
			return err
		}
		if err = validateBackupRecoveryClock(j, clock); err != nil {
			return err
		}
		var complete int
		if err = tx.QueryRow("SELECT count(*) FROM meta WHERE key='notification_recovery_summary'").Scan(&complete); err != nil {
			return err
		}
		if complete != 0 {
			return fmt.Errorf("backup recovery already completed")
		}
		e, err := evaluateBackupNotificationState(tx, c, clock)
		if err != nil {
			return err
		}
		intent = backupRecoveryFinishIntent{Challenge: ch, Manifest: j.Manifest, Snapshot: e.Snapshot}
		if err = storeBackupEvaluationMeta(tx, "notification_recovery_finish", intent); err != nil {
			return err
		}
		return backupRecoveryBoundary(fault, "recovery_finish_intent_transaction")
	})
	if err == nil {
		err = backupRecoveryBoundary(fault, "recovery_finish_intent_commit")
	}
	return
}

func recordBackupRecoverySummary(ctx context.Context, s *backupState, c WorkerInstallation, summary notify.RecoverySummary, fault func(string) error) error {
	lease, err := backupNotifierLease(s)
	if err != nil {
		return err
	}
	defer lease.Close()
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		j, err := readBackupRecoveryAuthority(tx, c)
		if err != nil {
			return err
		}
		if err = validateBackupRecoverySummary(tx, j, summary); err != nil {
			return err
		}
		var previous notify.RecoverySummary
		err = backupEvaluationMeta(tx, "notification_recovery_summary", &previous)
		if err == nil {
			if notify.Hash(previous) != notify.Hash(summary) {
				return fmt.Errorf("backup recovery completion changed")
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err = storeBackupEvaluationMeta(tx, "notification_recovery_summary", summary); err != nil {
			return err
		}
		return backupRecoveryBoundary(fault, "recovery_summary_transaction")
	})
	if err == nil {
		err = backupRecoveryBoundary(fault, "recovery_summary_commit")
	}
	return err
}

func validateBackupRecoverySummary(tx *sql.Tx, j backupNotificationRecovery, summary notify.RecoverySummary) error {
	var intent backupRecoveryFinishIntent
	if err := backupEvaluationMeta(tx, "notification_recovery_finish", &intent); err != nil {
		return err
	}
	if notify.Hash(summary.Challenge) != notify.Hash(j.Challenge) || notify.Hash(summary.Manifest) != notify.Hash(j.Manifest) ||
		notify.Hash(intent.Challenge) != notify.Hash(summary.Challenge) || notify.Hash(intent.Manifest) != notify.Hash(summary.Manifest) ||
		notify.Hash(intent.Snapshot) != notify.Hash(summary.Snapshot) || summary.DeliveryEnabled || summary.Ambiguous < 0 ||
		uint64(summary.Ambiguous) > j.Manifest.HighestIssued || summary.UnknownFirst != 0 || summary.UnknownLast != 0 {
		return fmt.Errorf("backup recovery completion conflicts with durable intent")
	}
	if err := summary.Snapshot.Validate(); err != nil {
		return err
	}
	if summary.Snapshot.Producer != j.Binding.Producer || summary.Snapshot.EvaluatedAt.Before(j.Challenge.IssuedAt) {
		return fmt.Errorf("backup recovery completion snapshot mismatch")
	}
	return nil
}

func backupRecoveryBoundary(fault func(string) error, stage string) error {
	if fault != nil {
		return fault(stage)
	}
	return nil
}
