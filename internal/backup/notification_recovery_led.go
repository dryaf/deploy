package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const maxBackupLEDRecoveryReports = 8

type backupLEDRecoveryIntent struct {
	Version, Position         int
	PreviousHash, SummaryHash string
	Binding                   backupNotificationBinding
	Challenge                 notify.RecoveryLEDChallenge
	Report                    notify.RecoveryLEDReport
	Clock                     backupEvaluationClock
}
type backupLEDRecoveryRecord struct {
	Intent  backupLEDRecoveryIntent
	Receipt *notify.RecoveryLEDEvidence
}
type backupLEDRecoveryHead struct {
	Position int
	Hash     string
}

// Only the current recovery's completed producer reconciliation can supply
// authority. Delivery may already be mapped to the new epoch, but stays fenced.
func backupLEDRecoveryAuthority(tx *sql.Tx, c WorkerInstallation) (j backupNotificationRecovery, summary notify.RecoverySummary, err error) {
	j, err = readBackupRecoveryAuthority(tx, c)
	if err != nil {
		return
	}
	if err = backupNotificationMeta(tx, "notification_recovery_summary", &summary); err != nil {
		return
	}
	err = validateBackupRecoverySummary(tx, j, summary)
	return
}
func validateBackupLEDChallenge(j backupNotificationRecovery, ch notify.RecoveryLEDChallenge, inventory string) error {
	if ch.Version != 1 || ch.InventoryID != inventory || !safefs.ValidID(inventory, "n") || ch.Operation != j.Challenge.Operation || ch.Before != j.Binding.Identity || ch.After != j.Challenge.Identity || ch.Producer != j.Binding.Producer || !safefs.ValidID(ch.Nonce, "n") || ch.IssuedAt.IsZero() || !ch.ExpiresAt.Equal(ch.IssuedAt.Add(10*time.Minute)) || !notify.ValidToken(ch.BootID) || !(backupEvaluationClock{Now: ch.IssuedAt, Boot: ch.BootID, Uptime: ch.Uptime}).valid() {
		return fmt.Errorf("backup LED challenge authority mismatch")
	}
	for _, hash := range []string{ch.InventoryHash, ch.SourcesHash, ch.DomainHash, ch.ConfigHash, ch.PreviousHash} {
		if !safefs.DigestPattern.MatchString(hash) {
			return fmt.Errorf("invalid backup LED challenge digest")
		}
	}
	return nil
}
func validateBackupLEDClock(j backupNotificationRecovery, ch notify.RecoveryLEDChallenge, clock backupEvaluationClock) error {
	translated := notify.RecoveryChallenge{Operation: ch.Operation, Identity: ch.After, Producer: ch.Producer, Nonce: ch.Nonce, IssuedAt: ch.IssuedAt, ExpiresAt: ch.ExpiresAt, BootID: ch.BootID, Uptime: ch.Uptime}
	if err := validateBackupRecoveryChallenge(j.Binding, translated, clock); err != nil {
		return err
	}
	return validateBackupRecoveryClock(j, clock)
}
func validateBackupLEDReceipt(intent backupLEDRecoveryIntent, v notify.RecoveryLEDEvidence) error {
	if v.Challenge != intent.Challenge || v.Hash != notify.Hash(intent.Report) || notify.Hash(v.Report) != notify.Hash(intent.Report) || v.ReceivedAt.Before(intent.Clock.Now) || !v.ReceivedAt.Before(intent.Challenge.ExpiresAt) {
		return fmt.Errorf("backup LED acknowledgement differs from frozen evidence")
	}
	return nil
}

func backupLEDRecoverySchema(tx *sql.Tx) (bool, error) {
	var version, tables int
	err := backupEvaluationMeta(tx, "notification_led_recovery_schema", &version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if e := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='backup_led_recovery_reports'").Scan(&tables); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && tables == 0 {
		var head backupLEDRecoveryHead
		if e := backupEvaluationMeta(tx, "notification_led_recovery_head", &head); e != sql.ErrNoRows {
			return false, fmt.Errorf("LED recovery lost its schema")
		}
		return false, nil
	}
	if err != nil || version != 1 || tables != 1 {
		return false, fmt.Errorf("LED recovery schema missing or changed")
	}
	return true, nil
}

func readBackupLEDRecoveryHistory(tx *sql.Tx, j backupNotificationRecovery, summary notify.RecoverySummary) (records []backupLEDRecoveryRecord, err error) {
	exists, err := backupLEDRecoverySchema(tx)
	if err != nil || !exists {
		return nil, err
	}
	var head backupLEDRecoveryHead
	if err = backupEvaluationMeta(tx, "notification_led_recovery_head", &head); err != nil {
		return
	}
	rows, err := tx.Query("SELECT position,inventory,hash,body,receipt FROM backup_led_recovery_reports ORDER BY position LIMIT 9")
	if err != nil {
		return nil, err
	}
	previous := ""
	var sequence, basis uint64
	for rows.Next() {
		var position int
		var inventory, hash, body, receipt string
		var record backupLEDRecoveryRecord
		if err = rows.Scan(&position, &inventory, &hash, &body, &receipt); err != nil {
			break
		}
		if err = notify.Decode([]byte(body), &record.Intent, 2*notify.MaxRequest); err != nil {
			break
		}
		v := record.Intent
		if err = validateBackupLEDChallenge(j, v.Challenge, inventory); err != nil {
			break
		}
		if err = validateBackupLEDClock(j, v.Challenge, v.Clock); err != nil {
			break
		}
		if err = v.Report.Snapshot.Validate(); err != nil {
			break
		}
		snap := v.Report.Snapshot
		if v.Version != 1 || position != len(records)+1 || position > maxBackupLEDRecoveryReports || v.Position != position || v.PreviousHash != previous || v.SummaryHash != notify.Hash(summary) || v.Binding != j.Binding || hash != notify.Hash(v) || snap.Producer != j.Binding.Producer || !snap.EvaluatedAt.Equal(v.Clock.Now) || snap.Sequence <= summary.Snapshot.Sequence || snap.Sequence <= sequence || snap.BasisRevision < summary.Snapshot.BasisRevision || snap.BasisRevision < basis || snap.ConfigRevision != summary.Snapshot.ConfigRevision || len(v.Report.Transitions) != 0 || len(snap.Support) != 0 {
			err = fmt.Errorf("backup LED recovery history mismatch")
			break
		}
		for _, sub := range snap.Subjects {
			if sub.FaultRevision != 0 {
				err = fmt.Errorf("backup LED report invented policy revision")
				break
			}
		}
		if err != nil {
			break
		}
		if receipt != "" {
			record.Receipt = &notify.RecoveryLEDEvidence{}
			if err = notify.Decode([]byte(receipt), record.Receipt, 2*notify.MaxRequest); err != nil {
				break
			}
			if err = validateBackupLEDReceipt(v, *record.Receipt); err != nil {
				break
			}
		}
		sequence, basis, previous = snap.Sequence, snap.BasisRevision, hash
		records = append(records, record)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return
	}
	if head.Position != len(records) || head.Hash != previous || len(records) == 0 {
		return nil, fmt.Errorf("backup LED recovery head lost history")
	}
	// Retain the exact snapshots that allocated the status sequences. A receipt
	// cannot reconstruct a lost local evaluation or advance its counter.
	for _, record := range records {
		var body string
		var savedBasis uint64
		if err = tx.QueryRow("SELECT basis,body FROM backup_notification_snapshots WHERE sequence=?", record.Intent.Report.Snapshot.Sequence).Scan(&savedBasis, &body); err != nil {
			return
		}
		var snapshot notify.Snapshot
		if err = notify.Decode([]byte(body), &snapshot, notify.MaxRequest); err != nil {
			return
		}
		if notify.Hash(snapshot) != notify.Hash(record.Intent.Report.Snapshot) || savedBasis != snapshot.BasisRevision {
			return nil, fmt.Errorf("backup LED recovery lost its frozen evaluation")
		}
	}
	return
}

// The caller retains both source leases and the worker's offline session across
// the exchange. Worker calls never run inside a backup SQLite transaction.
func reconcileBackupLEDRecovery(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoveryLEDEvidenceSession, inventory string) (notify.RecoveryLEDEvidence, error) {
	return reconcileBackupLEDRecoveryWith(ctx, s, c, session, inventory, reportHostClock, nil)
}
func reconcileBackupLEDRecoveryWith(ctx context.Context, s *backupState, c WorkerInstallation, session notify.RecoveryLEDEvidenceSession, inventory string, clock func() backupEvaluationClock, fault func(string) error) (out notify.RecoveryLEDEvidence, err error) {
	if session == nil || clock == nil || !safefs.ValidID(inventory, "n") {
		return out, fmt.Errorf("backup LED recovery dependencies or inventory missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	check, release, err := lockBackupRecovery(ctx, s)
	if err != nil {
		return out, err
	}
	defer release()
	var j backupNotificationRecovery
	var summary notify.RecoverySummary
	var local *backupLEDRecoveryRecord
	inspect := func(tx *sql.Tx) error {
		if err := check(); err != nil {
			return err
		}
		var err error
		j, summary, err = backupLEDRecoveryAuthority(tx, c)
		if err != nil {
			return err
		}
		records, err := readBackupLEDRecoveryHistory(tx, j, summary)
		if err != nil {
			return err
		}
		local = nil
		for _, record := range records {
			if record.Intent.Challenge.InventoryID == inventory {
				copy := record
				local = &copy
				break
			}
		}
		return nil
	}
	if err = backupTransferTx(ctx, s, inspect); err != nil {
		return
	}
	received, e := session.LEDEvidence(ctx, inventory, j.Binding.Producer)
	if e == nil {
		if local == nil {
			return out, fmt.Errorf("gateway LED evidence has no local frozen report")
		}
		if err = validateBackupLEDReceipt(local.Intent, received); err != nil {
			return
		}
	} else {
		if !errors.Is(e, notify.ErrRecoveryIncomplete) {
			return out, e
		}
		if local != nil && local.Receipt != nil {
			return out, fmt.Errorf("gateway lost acknowledged LED evidence")
		}
		if local == nil {
			ch, e := session.ChallengeLED(ctx, inventory, j.Binding.Producer)
			if e != nil {
				return out, e
			}
			if err = backupRecoveryBoundary(fault, "led_source_challenge"); err != nil {
				return
			}
			err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
				if err := inspect(tx); err != nil {
					return err
				}
				if local != nil {
					return fmt.Errorf("LED report appeared during preparation")
				}
				if err := validateBackupLEDChallenge(j, ch, inventory); err != nil {
					return err
				}
				now := clock()
				if err := validateBackupLEDClock(j, ch, now); err != nil {
					return err
				}
				records, err := readBackupLEDRecoveryHistory(tx, j, summary)
				if err != nil {
					return err
				}
				if len(records) >= maxBackupLEDRecoveryReports {
					return fmt.Errorf("backup LED recovery report budget exhausted")
				}
				evaluation, err := evaluateBackupNotificationState(tx, c, now)
				if err != nil {
					return err
				}
				intent := backupLEDRecoveryIntent{Version: 1, Position: len(records) + 1, SummaryHash: notify.Hash(summary), Binding: j.Binding, Challenge: ch, Report: notify.RecoveryLEDReport{Snapshot: evaluation.Snapshot}, Clock: now}
				if len(records) > 0 {
					intent.PreviousHash = notify.Hash(records[len(records)-1].Intent)
				}
				raw, err := json.Marshal(intent)
				if err != nil {
					return err
				}
				if len(raw) > 2*notify.MaxRequest {
					return fmt.Errorf("backup LED report exceeds payload bound")
				}
				if len(records) == 0 {
					if _, err = tx.Exec("CREATE TABLE backup_led_recovery_reports(position INTEGER PRIMARY KEY,inventory TEXT NOT NULL UNIQUE,hash TEXT NOT NULL,body TEXT NOT NULL,receipt TEXT NOT NULL DEFAULT '')"); err != nil {
						return err
					}
					if err = storeBackupEvaluationMeta(tx, "notification_led_recovery_schema", 1); err != nil {
						return err
					}
				}
				if _, err = tx.Exec("INSERT INTO backup_led_recovery_reports(position,inventory,hash,body) VALUES(?,?,?,?)", intent.Position, inventory, notify.Hash(intent), string(raw)); err != nil {
					return err
				}
				if err = storeBackupEvaluationMeta(tx, "notification_led_recovery_head", backupLEDRecoveryHead{intent.Position, notify.Hash(intent)}); err != nil {
					return err
				}
				// Validate before publication, so a source behind known recovery evidence
				// rolls back its evaluator, incident changes and sequence allocation together.
				if _, err = readBackupLEDRecoveryHistory(tx, j, summary); err != nil {
					return err
				}
				local = &backupLEDRecoveryRecord{Intent: intent}
				if err = backupRecoveryBoundary(fault, "led_source_report_transaction"); err != nil {
					return err
				}
				return check()
			})
			if err != nil {
				return
			}
			if err = backupRecoveryBoundary(fault, "led_source_report_commit"); err != nil {
				return
			}
		}
		if err = check(); err != nil {
			return
		}
		received, err = session.SubmitLED(ctx, local.Intent.Challenge, local.Intent.Report)
		if err != nil {
			return
		}
		if err = validateBackupLEDReceipt(local.Intent, received); err != nil {
			return
		}
	}
	if err = backupRecoveryBoundary(fault, "led_source_worker_accepted"); err != nil {
		return
	}
	want := notify.Hash(local.Intent)
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if err := inspect(tx); err != nil {
			return err
		}
		if local == nil || notify.Hash(local.Intent) != want {
			return fmt.Errorf("backup LED report changed during exchange")
		}
		if err := validateBackupLEDReceipt(local.Intent, received); err != nil {
			return err
		}
		if local.Receipt != nil {
			if notify.Hash(*local.Receipt) != notify.Hash(received) {
				return fmt.Errorf("backup LED receipt changed")
			}
			return check()
		}
		raw, err := json.Marshal(received)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE backup_led_recovery_reports SET receipt=? WHERE inventory=?", string(raw), inventory); err != nil {
			return err
		}
		if err = backupRecoveryBoundary(fault, "led_source_receipt_transaction"); err != nil {
			return err
		}
		return check()
	})
	if err == nil {
		err = backupRecoveryBoundary(fault, "led_source_receipt_commit")
	}
	if err == nil {
		out = received
	}
	return
}
