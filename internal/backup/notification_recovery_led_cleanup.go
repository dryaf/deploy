package backup

import (
	"context"
	"fmt"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

// clearBackupRecoveryLEDFault keeps both source leases across daemon observation,
// worker journaling and the bounded clear. The worker owns exact ID authorization;
// this adapter owns fresh backup-domain revalidation, without granting delivery.
func clearBackupRecoveryLEDFault(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoveryLEDCleanupSession, inventory, binding, id, evidence string) (notify.RecoveryLEDClearResult, error) {
	return clearBackupRecoveryLEDFaultWith(ctx, s, c, session, inventory, binding, id, evidence, reportHostClock)
}

func clearBackupRecoveryLEDFaultWith(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoveryLEDCleanupSession, inventory, binding, id, evidence string, clock func() backupEvaluationClock) (out notify.RecoveryLEDClearResult, err error) {
	if session == nil || clock == nil {
		return out, fmt.Errorf("backup LED cleanup dependencies missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	check, release, err := lockBackupRecovery(ctx, s)
	if err != nil {
		return out, err
	}
	defer release()
	guard := func(ctx context.Context, proof notify.RecoveryLEDEvidence) error {
		if err := check(); err != nil {
			return err
		}
		// The evaluator may allocate sequence numbers and incident transitions.
		// This is a speculative read of domain truth: ALWAYS roll back, including
		// success, so cleanup cannot export notifications or renew source evidence.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		j, summary, err := backupLEDRecoveryAuthority(tx, c)
		if err != nil {
			return err
		}
		records, err := readBackupLEDRecoveryHistory(tx, j, summary)
		if err != nil {
			return err
		}
		var frozen *backupLEDRecoveryRecord
		for i := range records {
			if records[i].Intent.Challenge.InventoryID == inventory {
				frozen = &records[i]
			}
		}
		if frozen == nil || frozen.Receipt == nil || notify.Hash(frozen.Receipt) != notify.Hash(proof) {
			return fmt.Errorf("backup LED cleanup lacks local acknowledged evidence")
		}
		if err = validateBackupLEDReceipt(frozen.Intent, proof); err != nil {
			return err
		}
		now := clock()
		if err = validateBackupLEDClock(j, frozen.Intent.Challenge, now); err != nil {
			return err
		}
		oldClock := frozen.Intent.Clock
		if now.Boot != oldClock.Boot || now.Uptime < oldClock.Uptime || now.Now.Before(oldClock.Now) {
			return fmt.Errorf("backup LED cleanup clock changed")
		}
		elapsed := now.Uptime - oldClock.Uptime
		if elapsed >= float64((1<<63-1)/int64(time.Second)) {
			return fmt.Errorf("backup LED cleanup evidence expired")
		}
		floor := oldClock.Now.Add(time.Duration(elapsed * float64(time.Second)))
		deadlineFloor := now.Now
		if floor.After(deadlineFloor) {
			deadlineFloor = floor
		}
		deadlineFloor = deadlineFloor.Add(2500 * time.Millisecond)
		for _, subject := range proof.Report.Snapshot.Subjects {
			if !subject.ValidUntil.After(deadlineFloor) {
				return fmt.Errorf("backup LED cleanup source lease expired")
			}
		}
		current, err := evaluateBackupNotificationState(tx, c, now)
		if err != nil {
			return err
		}
		want, got := proof.Report.Snapshot, current.Snapshot
		if want.Producer != got.Producer || want.ConfigRevision != got.ConfigRevision || want.BasisRevision != got.BasisRevision || len(want.Subjects) != len(got.Subjects) {
			return fmt.Errorf("backup LED cleanup domain changed; refresh evidence")
		}
		for i, subject := range got.Subjects {
			prior := want.Subjects[i]
			if subject.Key != prior.Key || subject.Condition != prior.Condition || subject.Reason != prior.Reason || subject.FaultRevision != prior.FaultRevision || !subject.ValidUntil.After(deadlineFloor) {
				return fmt.Errorf("backup LED cleanup subject changed or expired")
			}
		}
		return check()
	}
	out, err = session.ClearLED(ctx, notify.RecoveryLEDClearRequest{InventoryID: inventory, Binding: binding, FaultID: id, EvidenceHash: evidence}, guard)
	if err != nil {
		return out, err
	}
	if out.InventoryID != inventory || out.Binding != binding || out.FaultID != id || out.EvidenceHash != evidence {
		return out, fmt.Errorf("backup LED cleanup result identity mismatch")
	}
	return out, check()
}
