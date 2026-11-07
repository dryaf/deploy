package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

const legacyDeliveryJournalKey = "legacy_delivery_journal"
const maxLegacyDeliveryRecords = 8192

type legacyDeliveryHead struct {
	Sequence int    `json:"sequence"`
	Hash     string `json:"hash"`
	Lease    string `json:"lease"`
}

// The exact envelope and authority survive changes to the outbox's current
// report text, batching, retirement and its bounded provider-attempt summary.
type legacyDeliveryIntent struct {
	Generation string                         `json:"generation,omitempty"`
	ID         string                         `json:"id"`
	Archive    string                         `json:"archive"`
	Writer     string                         `json:"writer"`
	Epoch      uint64                         `json:"epoch"`
	ConfigHash string                         `json:"config_hash"`
	PolicyHash string                         `json:"policy_hash"`
	Provider   string                         `json:"provider"`
	PreparedAt time.Time                      `json:"prepared_at"`
	Event      notification.NotificationEvent `json:"event"`
}

type legacyDeliveryOutcome struct {
	IntentHash string                       `json:"intent_hash"`
	State      string                       `json:"state"`
	RecordedAt time.Time                    `json:"recorded_at"`
	Result     notification.DeliveryAttempt `json:"result"`
}

type legacyDeliveryRecord struct {
	Version  int                    `json:"version"`
	Sequence int                    `json:"sequence"`
	Previous string                 `json:"previous"`
	Intent   *legacyDeliveryIntent  `json:"intent,omitempty"`
	Outcome  *legacyDeliveryOutcome `json:"outcome,omitempty"`
}

type legacyDeliveryReview struct {
	Version        int                `json:"version"`
	JournalPresent bool               `json:"journal_present"`
	Head           legacyDeliveryHead `json:"head"`
	Attempts       int                `json:"attempts"`
	Accepted       int                `json:"accepted"`
	Rejected       int                `json:"rejected"`
	Ambiguous      int                `json:"ambiguous"`
	Unsettled      int                `json:"unsettled"`
	RecordLimit    int                `json:"record_limit"`
	// Old evidence cannot be made complete retroactively by installing this log.
	HistoricalEvidenceComplete bool `json:"historical_evidence_complete"`
	CutoverAllowed             bool `json:"cutover_allowed"`
}

type legacyDeliveryJournal struct {
	head    legacyDeliveryHead
	pending *legacyDeliveryIntent
	review  legacyDeliveryReview
}

func readLegacyDeliveryJournal(tx *sql.Tx) (j legacyDeliveryJournal, err error) {
	j.review.Version = 1
	j.review.RecordLimit = maxLegacyDeliveryRecords
	exists, err := notificationTable(tx, "backup_legacy_delivery_journal")
	if err != nil {
		return j, err
	}
	err = backupNotificationMeta(tx, legacyDeliveryJournalKey, &j.head)
	if err == sql.ErrNoRows && !exists {
		return j, nil
	}
	if err != nil || !exists || !safefs.DigestPattern.MatchString(j.head.Lease) || j.head.Sequence < 0 || j.head.Sequence > maxLegacyDeliveryRecords || (j.head.Sequence == 0) != (j.head.Hash == "") {
		return j, fmt.Errorf("legacy delivery journal authority is missing or invalid")
	}
	j.review.JournalPresent = true
	rows, err := tx.Query("SELECT sequence,body,hash FROM backup_legacy_delivery_journal ORDER BY sequence LIMIT ?", maxLegacyDeliveryRecords+1)
	if err != nil {
		return j, err
	}
	defer rows.Close()
	head := legacyDeliveryHead{Lease: j.head.Lease}
	for rows.Next() {
		var sequence int
		var raw, hash string
		if err = rows.Scan(&sequence, &raw, &hash); err != nil {
			return j, err
		}
		var record legacyDeliveryRecord
		if notify.Decode([]byte(raw), &record, 128<<10) != nil || record.Version != 1 || sequence != head.Sequence+1 || record.Sequence != sequence || sequence > j.head.Sequence || record.Previous != head.Hash || notify.Hash(record) != hash || (record.Intent == nil) == (record.Outcome == nil) {
			return j, fmt.Errorf("legacy delivery journal history changed")
		}
		if in := record.Intent; in != nil {
			if j.pending != nil || !safefs.ValidID(in.ID, "n") || in.Archive == "" || in.Writer == "" || in.Epoch == 0 || in.Event.ID == "" || in.Event.Kind == "" || in.PreparedAt.IsZero() || !safefs.DigestPattern.MatchString(in.PolicyHash) || (in.Provider != "signal" && in.Provider != "telegram") {
				return j, fmt.Errorf("invalid legacy delivery reservation")
			}
			j.pending = in
			j.review.Attempts++
		} else {
			o := record.Outcome
			if j.pending == nil || o.IntentHash != notify.Hash(j.pending) || o.RecordedAt.IsZero() || o.Result.Provider != j.pending.Provider || o.Result.At.IsZero() {
				return j, fmt.Errorf("legacy delivery outcome lost its reservation")
			}
			switch o.State {
			case "accepted":
				if !o.Result.Accepted || o.Result.Ambiguous {
					return j, fmt.Errorf("invalid accepted delivery outcome")
				}
				j.review.Accepted++
			case "rejected":
				if o.Result.Accepted || o.Result.Ambiguous {
					return j, fmt.Errorf("invalid rejected delivery outcome")
				}
				j.review.Rejected++
			case "ambiguous":
				if o.Result.Accepted || !o.Result.Ambiguous {
					return j, fmt.Errorf("invalid ambiguous delivery outcome")
				}
				j.review.Ambiguous++
			default:
				return j, fmt.Errorf("invalid legacy delivery outcome")
			}
			j.pending = nil
		}
		head = legacyDeliveryHead{sequence, hash, head.Lease}
	}
	if err = rows.Err(); err != nil {
		return j, err
	}
	if head != j.head {
		return j, fmt.Errorf("legacy delivery journal tail was lost")
	}
	j.review.Head = head
	if j.pending != nil {
		j.review.Unsettled = 1
	}
	return j, nil
}

func appendLegacyDeliveryRecord(tx *sql.Tx, head legacyDeliveryHead, record legacyDeliveryRecord) (legacyDeliveryHead, error) {
	if err := checkLegacyDeliveryHead(tx, head); err != nil {
		return head, err
	}
	if head.Sequence >= maxLegacyDeliveryRecords {
		return head, fmt.Errorf("legacy delivery journal is full; offline reconciliation required")
	}
	record.Version, record.Sequence, record.Previous = 1, head.Sequence+1, head.Hash
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 128<<10 {
		return head, fmt.Errorf("legacy delivery evidence exceeds bounds")
	}
	next := legacyDeliveryHead{record.Sequence, notify.Hash(record), head.Lease}
	if _, err = tx.Exec("INSERT INTO backup_legacy_delivery_journal VALUES(?,?,?)", next.Sequence, string(raw), next.Hash); err != nil {
		return head, err
	}
	return next, storeBackupEvaluationMeta(tx, legacyDeliveryJournalKey, next)
}

func checkLegacyDeliveryHead(tx *sql.Tx, head legacyDeliveryHead) error {
	var current legacyDeliveryHead
	if err := backupNotificationMeta(tx, legacyDeliveryJournalKey, &current); err != nil {
		return err
	}
	if current != head {
		return fmt.Errorf("legacy delivery journal head changed")
	}
	if head.Sequence == 0 {
		return nil
	}
	var raw, hash string
	if err := tx.QueryRow("SELECT body,hash FROM backup_legacy_delivery_journal WHERE sequence=?", head.Sequence).Scan(&raw, &hash); err != nil {
		return err
	}
	var record legacyDeliveryRecord
	if notify.Decode([]byte(raw), &record, 128<<10) != nil || record.Version != 1 || record.Sequence != head.Sequence || hash != head.Hash || notify.Hash(record) != hash {
		return fmt.Errorf("legacy delivery journal tail changed")
	}
	return nil
}

func openLegacyDeliveryJournal(ctx context.Context, s *backupState) (j legacyDeliveryJournal, err error) {
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		var err error
		j, err = readLegacyDeliveryJournal(tx)
		if err != nil || j.review.JournalPresent {
			return err
		}
		d, err := safefs.Open(s.directory)
		if err != nil {
			return err
		}
		defer d.Close()
		f, err := d.Open("legacy-delivery.lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		j.head.Lease, err = legacyDeliveryLeaseHash(f)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("CREATE TABLE backup_legacy_delivery_journal(sequence INTEGER PRIMARY KEY,body TEXT NOT NULL,hash TEXT NOT NULL)"); err != nil {
			return err
		}
		return storeBackupEvaluationMeta(tx, legacyDeliveryJournalKey, j.head)
	})
	return
}

func (j *legacyDeliveryJournal) reserve(ctx context.Context, s *backupState, c WorkerInstallation, provider string, event notification.NotificationEvent) (in legacyDeliveryIntent, eligible bool, err error) {
	if (provider != "signal" && provider != "telegram") || event.ID == "" || event.Kind == "" {
		return in, false, fmt.Errorf("invalid legacy delivery reservation inputs")
	}
	if j.pending != nil || j.head.Sequence+2 > maxLegacyDeliveryRecords {
		return in, false, fmt.Errorf("unsettled or full legacy delivery journal; reconciliation required")
	}
	var next legacyDeliveryHead
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		var fenced bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key='notification_owner')").Scan(&fenced); err != nil {
			return err
		}
		generation := ""
		if fenced {
			owner, err := readBackupNotificationOwner(tx)
			if err != nil || owner.Mode != "legacy" {
				return fmt.Errorf("legacy notification delivery is fenced")
			}
			generation = owner.LegacyGeneration
		}
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox WHERE id=? AND accepted IS NULL AND covered_by IS NULL)", event.ID).Scan(&eligible); err != nil || !eligible {
			return err
		}
		control, err := incidentControl(tx)
		if err != nil {
			return err
		}
		if control.SharedRequired || control.ReportingDisabled {
			return fmt.Errorf("direct delivery is disabled by retained archive policy")
		}
		if control.ArchiveID == "" || control.WriterID == "" || control.Epoch == 0 {
			return fmt.Errorf("legacy delivery source authority is missing")
		}
		id, err := safefs.NewID("n")
		if err != nil {
			return err
		}
		event.Message = notification.NotificationEnvelope(event.Message)
		in = legacyDeliveryIntent{ID: id, Archive: control.ArchiveID, Writer: control.WriterID, Epoch: control.Epoch, ConfigHash: control.ConfigHash, PolicyHash: notify.Hash(c.Policy.Notifications), Provider: provider, PreparedAt: time.Now().UTC(), Event: event}
		in.Generation = generation
		next, err = appendLegacyDeliveryRecord(tx, j.head, legacyDeliveryRecord{Intent: &in})
		return err
	})
	if err == nil && eligible {
		j.head, j.pending = next, &in
	}
	return
}

// Settlement and acceptance are one transaction. A crash can leave only a
// reservation (unknown), or a complete outcome and matching outbox acceptance.
func (j *legacyDeliveryJournal) settle(ctx context.Context, s *backupState, result notification.DeliveryAttempt, sendErr error, recovered bool, now time.Time) (accepted bool, err error) {
	if j.pending == nil {
		return false, fmt.Errorf("legacy delivery has no reservation")
	}
	in := *j.pending
	if result.Provider != "" && result.Provider != in.Provider || result.Accepted && (sendErr != nil || result.Ambiguous) || recovered {
		result.Accepted, result.Ambiguous = false, true
	}
	result.Provider = in.Provider
	if result.At.IsZero() {
		result.At = in.PreparedAt
	}
	state := "rejected"
	if result.Accepted {
		state, accepted = "accepted", true
	} else if result.Ambiguous {
		state = "ambiguous"
	}
	outcome := legacyDeliveryOutcome{notify.Hash(in), state, now.UTC(), result}
	var next legacyDeliveryHead
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		var err error
		next, err = appendLegacyDeliveryRecord(tx, j.head, legacyDeliveryRecord{Outcome: &outcome})
		if err != nil {
			return err
		}
		var raw string
		err = tx.QueryRow("SELECT record FROM outbox WHERE id=?", in.Event.ID).Scan(&raw)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			var event notification.NotificationEvent
			if err = notify.Decode([]byte(raw), &event, 128<<10); err != nil || event.ID != in.Event.ID {
				return fmt.Errorf("legacy delivery source changed")
			}
			event.Attempts = append(event.Attempts, result)
			if len(event.Attempts) > 32 {
				event.Attempts = event.Attempts[len(event.Attempts)-32:]
			}
			body, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE outbox SET record=? WHERE id=?", string(body), event.ID); err != nil {
				return err
			}
			if accepted {
				_, err = tx.Exec("UPDATE outbox SET accepted=?,attempts=attempts+1 WHERE id=? AND accepted IS NULL", now.UTC().Format(eventTimeLayout), event.ID)
			} else if recovered {
				_, err = tx.Exec("UPDATE outbox SET due=max(due,?),attempts=attempts+1 WHERE id=? AND accepted IS NULL", now.Add(time.Minute).UTC().Format(eventTimeLayout), event.ID)
			}
			if err != nil {
				return err
			}
		}
		if recovered {
			if err = markSharedDeliveryOutage(tx, now); err != nil {
				return err
			}
		}
		return storeBackupEvaluationMeta(tx, "delivery/"+in.Provider, result)
	})
	if err == nil {
		j.head, j.pending = next, nil
	}
	return
}

// Keep the legacy retry/fallback policy, but never infer a lost response was
// unsent. Future cutover consumes the retained history, not only Attempts[-32:].
func (j *legacyDeliveryJournal) recover(ctx context.Context, s *backupState, now time.Time) error {
	if j.pending == nil {
		return nil
	}
	_, err := j.settle(ctx, s, notification.DeliveryAttempt{Ambiguous: true}, nil, true, now)
	return err
}

// A future migration must explicitly reconcile these outcomes. Neither a new
// generation nor a new report revision proves previously attempted work unsent.
func requireBackupUnsentSource(tx *sql.Tx, event notification.NotificationEvent) error {
	held, err := backupSourceHeld(tx, event)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("backup source %s is held by legacy outcome reconciliation", event.ID)
	}
	var remigrated backupCutoverDecision
	if e := backupNotificationMeta(tx, remigrationSourcePrefix+event.ID, &remigrated); e == nil {
		if remigrated.Disposition == "held" {
			released, err := requireBackupLegacyRetry(tx, event)
			if err != nil {
				return err
			}
			if released {
				return nil
			}
		}
		if remigrated.Disposition != "unsent" {
			return fmt.Errorf("remigrated source is not proven unsent")
		}
		return requireBackupUnattemptedSource(tx, event)
	} else if e != sql.ErrNoRows {
		return e
	}
	exists, err := notificationTable(tx, "backup_notification_legacy")
	if err != nil {
		return err
	}
	if exists {
		released, err := requireBackupLegacyRetry(tx, event)
		if err != nil || released {
			return err
		}
	}
	return requireBackupUnattemptedSource(tx, event)
}

func requireBackupUnattemptedSource(tx *sql.Tx, event notification.NotificationEvent) error {
	if len(event.Attempts) != 0 {
		return fmt.Errorf("backup source %s has legacy attempts; explicit delivery reconciliation required", event.ID)
	}
	var attempts int
	if err := tx.QueryRow("SELECT attempts FROM outbox WHERE id=?", event.ID).Scan(&attempts); err != nil {
		return err
	}
	if attempts != 0 {
		return fmt.Errorf("backup source %s has historical delivery attempts", event.ID)
	}
	exists, err := notificationTable(tx, "backup_legacy_delivery_journal")
	if err != nil {
		return err
	}
	if exists {
		var attempted bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM backup_legacy_delivery_journal WHERE json_extract(body,'$.intent.event.id')=?)", event.ID).Scan(&attempted); err != nil {
			return err
		}
		if attempted {
			return fmt.Errorf("backup source %s has retained legacy delivery evidence", event.ID)
		}
	}
	return nil
}

// Notifier callers retain notifier.lock. This additional lease also serializes
// direct dispatch callers and proves an abandoned reservation has no live sender.
func lockLegacyDelivery(ctx context.Context, s *backupState) (check func() error, release func(), err error) {
	d, err := safefs.Open(s.directory)
	if err != nil {
		return nil, nil, err
	}
	var enrolled bool
	if err = s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key=?)", legacyDeliveryJournalKey).Scan(&enrolled); err != nil {
		d.Close()
		return nil, nil, err
	}
	l, err := d.Lock("legacy-delivery.lock", true, !enrolled)
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	release = func() { l.Close(); d.Close() }
	if enrolled {
		var raw string
		var head legacyDeliveryHead
		hash, e := legacyDeliveryLeaseHash(l.File())
		if err = s.db.QueryRow("SELECT value FROM meta WHERE key=?", legacyDeliveryJournalKey).Scan(&raw); err != nil || e != nil || notify.Decode([]byte(raw), &head, 8192) != nil || head.Lease != hash {
			release()
			return nil, nil, fmt.Errorf("legacy delivery lease no longer matches its enrolled inode")
		}
	}
	check = func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		held, err := d.File().Stat()
		if err != nil {
			return err
		}
		current, err := os.Lstat(s.directory)
		if err != nil || !os.SameFile(held, current) {
			return fmt.Errorf("legacy delivery state directory changed")
		}
		f, err := d.Open("legacy-delivery.lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		held, err = l.File().Stat()
		if err != nil {
			return err
		}
		current, err = f.Stat()
		if err != nil || !os.SameFile(held, current) {
			return fmt.Errorf("legacy delivery lease changed")
		}
		return nil
	}
	return
}

func legacyDeliveryLeaseHash(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("unsafe legacy delivery lease")
	}
	return notify.Hash([]uint64{st.Dev, st.Ino}), nil
}
