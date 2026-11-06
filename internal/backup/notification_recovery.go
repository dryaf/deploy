package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// This is the original export, not the current delivery binding. Command
// reconciliation and canonical ownership migration precede the local delivery
// commit; this journal survives that commit unchanged as historical evidence.
type backupNotificationRecovery struct {
	Version                 int
	Ancestor                string `json:"ancestor,omitempty"`
	Binding                 backupNotificationBinding
	Owner                   backupNotificationOwner
	Challenge               notify.RecoveryChallenge
	Manifest                notify.RecoveryManifest
	CapturedAt              time.Time
	Clock                   backupEvaluationClock
	Records                 uint64
	Bytes                   int
	MissingTerminalReceipts int
}

type backupRecoveryPage struct {
	Records []notify.RecoveryRecord
	Next    uint64
	Done    bool
}

func requireNotificationRecoveryInactive(tx *sql.Tx) error {
	if err := requireNoPendingMigrationCycle(tx); err != nil {
		return err
	}
	return requireNotificationRecoverySettled(tx)
}

func requireNotificationRecoverySettled(tx *sql.Tx) error {
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM meta WHERE key IN ('notification_recovery','notification_recovery_commit_intent','notification_recovery_commit')").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		_, err := readRecoveryActivation(tx, true)
		return err
	}
	return nil
}

func validateBackupRecoveryChallenge(b backupNotificationBinding, ch notify.RecoveryChallenge, clock backupEvaluationClock) error {
	if !safefs.ValidID(ch.Operation, "n") || !safefs.ValidID(ch.Nonce, "n") || !ch.Identity.Valid() ||
		ch.Identity.Worker != b.Identity.Worker || ch.Identity.Recovery == b.Identity.Recovery || ch.Producer != b.Producer {
		return fmt.Errorf("backup recovery challenge binding mismatch")
	}
	if ch.IssuedAt.IsZero() || !ch.ExpiresAt.After(ch.IssuedAt) || ch.ExpiresAt.Sub(ch.IssuedAt) > 10*time.Minute ||
		clock.Now.Before(ch.IssuedAt) || !clock.Now.Before(ch.ExpiresAt) || !notify.ValidToken(ch.BootID) ||
		clock.Boot != ch.BootID || math.IsNaN(ch.Uptime) || math.IsInf(ch.Uptime, 0) || ch.Uptime < 0 ||
		math.IsNaN(clock.Uptime) || math.IsInf(clock.Uptime, 0) || clock.Uptime < ch.Uptime || clock.Uptime-ch.Uptime >= 600 {
		return fmt.Errorf("backup recovery challenge clock expired or invalid")
	}
	return nil
}

func beginBackupNotificationRecovery(ctx context.Context, s *backupState, ch notify.RecoveryChallenge) (backupNotificationRecovery, error) {
	return beginBackupNotificationRecoveryWith(ctx, s, ch, reportHostClock(), nil)
}

// The exclusive notifier lease drains the shared report pump before the atomic
// fence/export commit. Failure rolls back both; a lost commit response is replayed
// from the journal. No provider call, monitoring ping, or active identity change.
func beginBackupNotificationRecoveryWith(ctx context.Context, s *backupState, ch notify.RecoveryChallenge, clock backupEvaluationClock, fault func(string) error) (journal backupNotificationRecovery, err error) {
	lease, err := backupNotifierLease(s)
	if err != nil {
		return journal, err
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		owner, err := readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if err = validateBackupRecoveryChallenge(owner.Binding, ch, clock); err != nil {
			return err
		}
		err = backupNotificationMeta(tx, "notification_recovery", &journal)
		ancestor := ""
		if err == nil && journal.Challenge.Operation != ch.Operation && owner.Mode == "shared" {
			ancestor, err = retainBackupRecoveryAncestor(tx, s, journal, fault)
			if err != nil {
				return err
			}
			err = sql.ErrNoRows
		}
		if err == nil {
			if !backupRecoveryJournalValid(journal) || journal.Binding != owner.Binding || owner.Mode != "fenced" || owner.Socket != journal.Owner.Socket ||
				journal.Challenge.Operation != ch.Operation || journal.Challenge.Identity != ch.Identity || journal.Challenge.Producer != ch.Producer {
				return fmt.Errorf("another backup recovery is already prepared")
			}
			if ch.IssuedAt.Before(journal.Challenge.IssuedAt) || (ch.Nonce == journal.Challenge.Nonce && notify.Hash(ch) != notify.Hash(journal.Challenge)) {
				return fmt.Errorf("backup recovery challenge regressed or changed")
			}
			if err = verifyBackupRecoveryExport(tx, journal); err != nil {
				return err
			}
			if err = validateBackupRecoveryClock(journal, clock); err != nil {
				return err
			}
			journal.Challenge = ch // Renew the lease without recapturing mutable evidence.
			return storeBackupEvaluationMeta(tx, "notification_recovery", journal)
		}
		if err != sql.ErrNoRows {
			return err
		}
		if owner.Mode != "shared" {
			return fmt.Errorf("backup ownership handoff is not committed")
		}
		if err = installNotificationReceiptHistory(tx); err != nil {
			return err
		}
		exists, err := notificationTable(tx, "backup_notification_recovery_records")
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("unowned backup recovery export")
		}
		if _, err = tx.Exec("CREATE TABLE backup_notification_recovery_records(seq INTEGER PRIMARY KEY,hash TEXT NOT NULL,body TEXT NOT NULL)"); err != nil {
			return err
		}
		journal = backupNotificationRecovery{Version: 1, Ancestor: ancestor, Binding: owner.Binding, Owner: owner, Challenge: ch, CapturedAt: clock.Now, Clock: clock}
		if ancestor != "" {
			journal.Version = 2
		}
		_, inherited, err := readBackupRecoveryAncestors(tx, journal, nil)
		if err != nil {
			return err
		}
		manifest := &journal.Manifest
		manifest.ImportedReceipts = notify.Cursor{Identity: owner.Binding.Identity, Producer: owner.Binding.Producer}
		var rawIdentity, rawProducer string
		var version int
		if err = tx.QueryRow("SELECT version,identity,producer,submitted,cursor FROM notification_producer WHERE singleton=1").Scan(&version, &rawIdentity, &rawProducer, &manifest.HighestIssued, &manifest.ImportedReceipts.Position); err != nil {
			return err
		}
		var identity notify.Identity
		var producer notify.Producer
		if err = notify.Decode([]byte(rawIdentity), &identity, 4096); err != nil {
			return err
		}
		if err = notify.Decode([]byte(rawProducer), &producer, 4096); err != nil {
			return err
		}
		if version != 1 || identity != owner.Binding.Identity || producer != owner.Binding.Producer || manifest.ImportedReceipts.Position < 0 ||
			(manifest.HighestIssued != 0 && !notify.Sequence(manifest.HighestIssued)) {
			return fmt.Errorf("backup recovery ledger binding mismatch")
		}
		identityHash := notify.Hash(identity)
		if err = tx.QueryRow(`SELECT COALESCE(max(e.seq),0) FROM notification_exports e
JOIN backup_notification_transfers t ON t.event=e.id
WHERE t.submitted=1 OR e.state<>'frozen'`).Scan(&manifest.HighestAcknowledged); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT e.seq,e.id,e.body,e.state,e.acceptance,
COALESCE((SELECT h.body FROM notification_receipt_history h WHERE h.identity=? AND h.event=e.id
AND h.position<=? AND h.state=e.state AND h.state IN ('provider_accepted','expired','cancelled','superseded')
ORDER BY h.position DESC LIMIT 1),'')
FROM notification_exports e JOIN backup_notification_transfers t ON t.event=e.id ORDER BY e.seq`, identityHash, manifest.ImportedReceipts.Position)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var seq uint64
			var id, body, state, acceptance, proof string
			if err = rows.Scan(&seq, &id, &body, &state, &acceptance, &proof); err != nil {
				return err
			}
			var record notify.RecoveryRecord
			if err = notify.Decode([]byte(body), &record.Message, notify.MaxRequest); err != nil {
				return err
			}
			m := record.Message
			if seq != journal.Records+1 || seq > manifest.HighestIssued || m.Sequence != seq || m.ID != id || m.Producer != producer || (m.Generation != owner.Binding.Generation && notify.Hash(inherited[m.ID]) != notify.Hash(m)) || m.Route != owner.Binding.Route {
				return fmt.Errorf("backup recovery export sequence or binding mismatch")
			}
			if err = m.Validate(); err != nil {
				return err
			}
			switch state {
			case "frozen", "queued", "waiting_status", "sending", "retry_wait", "fenced", "blocked", "ambiguous", "expired", "superseded", "cancelled", "provider_accepted":
			default:
				return fmt.Errorf("backup recovery outcome invalid")
			}
			if proof != "" {
				var retained notify.RetainedReceipt
				if err = notify.Decode([]byte(proof), &retained, 16384); err != nil {
					return err
				}
				r := retained.Receipt
				var receiptHash string
				if err = tx.QueryRow("SELECT hash FROM notification_receipt_history WHERE identity=? AND position=?", identityHash, r.Position).Scan(&receiptHash); err != nil {
					return err
				}
				if receiptHash != notify.Hash(retained) {
					return fmt.Errorf("backup recovery receipt damaged")
				}
				if retained.Identity != identity || retained.Producer != producer || r.EventID != id || r.Sequence != seq ||
					r.Position <= 0 || r.Position > manifest.ImportedReceipts.Position || r.State != state {
					return fmt.Errorf("backup recovery receipt mismatch")
				}
				if state == "provider_accepted" {
					raw, _ := json.Marshal(r.Acceptance)
					if r.Acceptance == nil || string(raw) != acceptance {
						return fmt.Errorf("backup recovery acceptance mismatch")
					}
				}
				record.Receipt = &retained
			} else if state == "provider_accepted" || state == "expired" || state == "cancelled" || state == "superseded" {
				journal.MissingTerminalReceipts++ // Old acceptance text is never made into a receipt.
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			if len(raw)+4098 > notify.MaxRequest {
				return fmt.Errorf("backup recovery record exceeds page bound")
			}
			journal.Bytes += len(raw)
			if journal.Bytes > 32<<20 {
				return fmt.Errorf("backup recovery export exceeds 32 MiB bound")
			}
			if _, err = tx.Exec("INSERT INTO backup_notification_recovery_records VALUES(?,?,?)", seq, notify.Hash(record), string(raw)); err != nil {
				return err
			}
			journal.Records++
		}
		if err = rows.Err(); err != nil {
			return err
		}
		var exports uint64
		if err = tx.QueryRow("SELECT count(*) FROM notification_exports").Scan(&exports); err != nil {
			return err
		}
		if journal.Records != manifest.HighestIssued || exports != journal.Records {
			return fmt.Errorf("backup recovery payload history is incomplete")
		}
		// This producer does not compact exports. A hole must never become an invented
		// compacted floor or an implicit cancellation of a delivery obligation.
		if err = verifyBackupRecoveryExport(tx, journal); err != nil {
			return err
		}
		owner.Mode = "fenced"
		if err = storeBackupEvaluationMeta(tx, "notification_owner", owner); err != nil {
			return err
		}
		if err = storeBackupEvaluationMeta(tx, "notification_recovery", journal); err != nil {
			return err
		}
		if fault != nil {
			return fault("backup_recovery_transaction")
		}
		return nil
	})
	if err == nil && fault != nil {
		err = fault("backup_recovery_commit")
	}
	return
}

func verifyBackupRecoveryExport(tx *sql.Tx, j backupNotificationRecovery) error {
	if _, _, err := readBackupRecoveryAncestors(tx, j, nil); err != nil {
		return err
	}
	var rawIdentity, rawProducer string
	var version int
	var issued uint64
	var cursor int64
	if err := tx.QueryRow("SELECT version,identity,producer,submitted,cursor FROM notification_producer WHERE singleton=1").Scan(&version, &rawIdentity, &rawProducer, &issued, &cursor); err != nil {
		return err
	}
	var identity notify.Identity
	var producer notify.Producer
	if err := notify.Decode([]byte(rawIdentity), &identity, 4096); err != nil {
		return err
	}
	if err := notify.Decode([]byte(rawProducer), &producer, 4096); err != nil {
		return err
	}
	binding, expectedCursor, err := backupRecoveryDeliveryBinding(tx, j)
	if err != nil {
		return err
	}
	if version != 1 || identity != binding.Identity || producer != j.Binding.Producer || issued != j.Manifest.HighestIssued || cursor != expectedCursor {
		return fmt.Errorf("backup delivery ledger changed during recovery")
	}
	var records uint64
	var bytes int
	if err := tx.QueryRow("SELECT count(*),COALESCE(sum(length(CAST(body AS BLOB))),0) FROM backup_notification_recovery_records").Scan(&records, &bytes); err != nil {
		return err
	}
	if !backupRecoveryJournalValid(j) || records != j.Records || records != j.Manifest.HighestIssued || bytes != j.Bytes ||
		j.Manifest.HighestAcknowledged > j.Manifest.HighestIssued || j.Manifest.CompactedThrough != 0 || j.Manifest.OutcomeCheckpoint != "" ||
		j.Manifest.ImportedReceipts.Identity != j.Binding.Identity || j.Manifest.ImportedReceipts.Producer != j.Binding.Producer {
		return fmt.Errorf("backup recovery export is incomplete or inconsistent")
	}
	return nil
}

func validateBackupRecoveryClock(j backupNotificationRecovery, clock backupEvaluationClock) error {
	if clock.Now.Before(j.CapturedAt) || (clock.Boot == j.Clock.Boot && clock.Uptime < j.Clock.Uptime) {
		return fmt.Errorf("backup recovery clock moved backwards since capture")
	}
	return nil
}

func readBackupNotificationRecoveryPage(ctx context.Context, s *backupState, ch notify.RecoveryChallenge, after uint64, clock backupEvaluationClock) (page backupRecoveryPage, err error) {
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		owner, err := readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if err = validateBackupRecoveryChallenge(owner.Binding, ch, clock); err != nil {
			return err
		}
		var j backupNotificationRecovery
		if err = backupNotificationMeta(tx, "notification_recovery", &j); err != nil {
			return err
		}
		if owner.Mode != "fenced" || j.Binding != owner.Binding || owner.Socket != j.Owner.Socket || notify.Hash(j.Challenge) != notify.Hash(ch) || after > j.Manifest.HighestIssued {
			return fmt.Errorf("backup recovery page binding/cursor mismatch")
		}
		if err = verifyBackupRecoveryExport(tx, j); err != nil {
			return err
		}
		if err = validateBackupRecoveryClock(j, clock); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT seq,hash,body FROM backup_notification_recovery_records WHERE seq>? ORDER BY seq LIMIT 128", after)
		if err != nil {
			return err
		}
		defer rows.Close()
		page.Next = after
		size := 2 // JSON array brackets; retain the reconciler's request overhead.
		for rows.Next() {
			var seq uint64
			var hash, body string
			if err = rows.Scan(&seq, &hash, &body); err != nil {
				return err
			}
			var record notify.RecoveryRecord
			if err = notify.Decode([]byte(body), &record, notify.MaxRequest); err != nil {
				return err
			}
			if seq != page.Next+1 || record.Message.Sequence != seq || record.Message.Producer != ch.Producer || notify.Hash(record) != hash {
				return fmt.Errorf("backup recovery record damaged")
			}
			extra := len(body)
			if len(page.Records) > 0 {
				extra++
			}
			if size+extra+4096 > notify.MaxRequest {
				if len(page.Records) == 0 {
					return fmt.Errorf("backup recovery record exceeds page bound")
				}
				break
			}
			size += extra
			page.Records = append(page.Records, record)
			page.Next = seq
		}
		if err = rows.Err(); err != nil {
			return err
		}
		page.Done = page.Next == j.Manifest.HighestIssued
		if !page.Done && len(page.Records) == 0 {
			return fmt.Errorf("backup recovery page missing")
		}
		return nil
	})
	return
}
