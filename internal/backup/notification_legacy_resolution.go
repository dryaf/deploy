package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const legacyResolutionPrefix = "notification_legacy_resolution/"

// Original migration dispositions never change. Old helpers therefore continue
// to hold these sources. New exporters require the independent reviewed grant.
const unresolvedLegacyPredicate = `l.disposition='held' AND NOT EXISTS
 (SELECT 1 FROM meta resolution WHERE resolution.key='notification_legacy_resolution/'||l.hash)`

type backupLegacyReview struct {
	Version    int                     `json:"version"`
	ConfigHash string                  `json:"config_hash"`
	Owner      backupNotificationOwner `json:"owner"`
	Original   backupCutoverRow        `json:"original"`
	Current    backupCutoverRow        `json:"current"`
	Journal    legacyDeliveryReview    `json:"journal"`
	ReviewedAt time.Time               `json:"reviewed_at"`
}

type backupLegacyResolution struct {
	Version          int                `json:"version"`
	Review           backupLegacyReview `json:"review"`
	ReviewHash       string             `json:"review_hash"`
	Action           string             `json:"action"`
	Reason           string             `json:"reason"`
	AckDuplicateRisk bool               `json:"ack_duplicate_risk"`
}

func legacyResolutionFile(hash string) string {
	return "notification-legacy-resolution-" + hash + ".json"
}

func readBackupLegacyReview(tx *sql.Tx, c WorkerInstallation, source string, now time.Time) (r backupLegacyReview, err error) {
	r.Version, r.ConfigHash, r.ReviewedAt = 1, notify.Hash(c), now.UTC()
	r.Owner, err = readBackupNotificationOwner(tx)
	if err != nil {
		return
	}
	if r.Owner.CutoverHash == "" || (r.Owner.Mode != "shared" && r.Owner.Mode != "fenced") {
		return r, fmt.Errorf("legacy review requires a managed cutover owner")
	}
	if err = requireBackupRecoveryConfig(tx, r.Owner.Binding, c); err != nil {
		return
	}
	if err = requireNotificationRecoveryInactive(tx); err != nil {
		return
	}
	var raw, hash, disposition string
	if err = tx.QueryRow("SELECT body,hash,disposition FROM backup_notification_legacy WHERE source=?", source).Scan(&raw, &hash, &disposition); err != nil {
		return
	}
	if err = notify.Decode([]byte(raw), &r.Original, safefs.MaxRecordBytes); err != nil {
		return
	}
	if r.Original.ID != source || notify.Hash(r.Original) != hash || disposition != "held" || r.Original.Accepted.Valid {
		return r, fmt.Errorf("source is not an unchanged held legacy row")
	}
	err = tx.QueryRow("SELECT id,kind,created,due,attempts,accepted,covered_by,record FROM outbox WHERE id=?", source).Scan(&r.Current.ID, &r.Current.Kind, &r.Current.Created, &r.Current.Due, &r.Current.Attempts, &r.Current.Accepted, &r.Current.CoveredBy, &r.Current.Record)
	if err != nil {
		return
	}
	if r.Current.Accepted.Valid {
		return r, fmt.Errorf("accepted legacy work cannot be released for retry")
	}
	journal, err := readLegacyDeliveryJournal(tx)
	if err != nil {
		return r, err
	}
	r.Journal = journal.review
	return
}

func (s backupLegacyResolution) validate() error {
	if s.Version != 1 || s.Review.Version != 1 || s.ReviewHash != notify.Hash(s.Review) ||
		!safefs.DigestPattern.MatchString(s.Review.ConfigHash) || !safefs.DigestPattern.MatchString(s.Review.Owner.CutoverHash) ||
		s.Review.ReviewedAt.IsZero() || s.Review.Original.ID == "" || s.Review.Original.ID != s.Review.Current.ID ||
		strings.TrimSpace(s.Reason) == "" || len(s.Reason) > 512 {
		return fmt.Errorf("legacy resolution requires exact review and bounded audit reason")
	}
	if s.Action != "unsent" && s.Action != "retry" {
		return fmt.Errorf("legacy resolution action must be unsent or retry")
	}
	if s.Action == "retry" && !s.AckDuplicateRisk {
		return fmt.Errorf("retry requires explicit acknowledgement of possible duplicate delivery")
	}
	return nil
}

// A grant supplements evidence, never rewrites it. In particular an acknowledged
// retry is not a claim that the previous attempt failed, or that delivery occurred.
func readBackupLegacyResolution(tx *sql.Tx, source string) (s backupLegacyResolution, present bool, err error) {
	var raw, hash, disposition string
	err = tx.QueryRow("SELECT body,hash,disposition FROM backup_notification_legacy WHERE source=?", source).Scan(&raw, &hash, &disposition)
	if err == sql.ErrNoRows {
		return s, false, nil
	}
	if err != nil {
		return
	}
	err = backupNotificationMeta(tx, legacyResolutionPrefix+hash, &s)
	if err == sql.ErrNoRows {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if s.validate() != nil || disposition != "held" || notify.Hash(s.Review.Original) != hash || mustCutoverJSON(s.Review.Original) != raw || s.Review.Original.ID != source {
		return s, false, fmt.Errorf("legacy resolution lost its original evidence")
	}
	owner, err := readBackupNotificationOwner(tx)
	if err != nil {
		return s, false, err
	}
	// Deactivation/revocation can change mode, but cannot grant another binding.
	if owner.Binding != s.Review.Owner.Binding || owner.Socket != s.Review.Owner.Socket || owner.CutoverHash != s.Review.Owner.CutoverHash {
		return s, false, fmt.Errorf("legacy resolution authority changed")
	}
	return s, true, nil
}

func requireBackupLegacyRetry(tx *sql.Tx, event notification.NotificationEvent) (bool, error) {
	s, present, err := readBackupLegacyResolution(tx, event.ID)
	if err != nil || !present {
		return false, err
	}
	var before notification.NotificationEvent
	if err = notify.Decode([]byte(s.Review.Current.Record), &before, notify.MaxRequest); err != nil {
		return false, err
	}
	var attempts int
	var accepted sql.NullString
	var raw string
	if err = tx.QueryRow("SELECT attempts,accepted,record FROM outbox WHERE id=?", event.ID).Scan(&attempts, &accepted, &raw); err != nil {
		return false, err
	}
	var current notification.NotificationEvent
	if err = notify.Decode([]byte(raw), &current, notify.MaxRequest); err != nil {
		return false, err
	}
	// Report text may be freshly evaluated. Attempts and batch membership cannot
	// be replaced with a different previously sent envelope behind this grant.
	if attempts != s.Review.Current.Attempts || accepted.Valid || current.ID != event.ID || current.Kind != before.Kind || event.Kind != before.Kind ||
		notify.Hash(current.Attempts) != notify.Hash(before.Attempts) || notify.Hash(current.Members) != notify.Hash(before.Members) {
		return false, fmt.Errorf("released legacy source delivery evidence changed")
	}
	j, err := readLegacyDeliveryJournal(tx)
	if err != nil {
		return false, err
	}
	if j.review != s.Review.Journal {
		return false, fmt.Errorf("legacy attempt journal changed after resolution")
	}
	return true, nil
}

func resolveBackupLegacy(ctx context.Context, state *backupState, c WorkerInstallation, spec backupLegacyResolution, now time.Time, fault func(string) error) error {
	if err := spec.validate(); err != nil {
		return err
	}
	lease, err := backupNotifierLease(state)
	if err != nil {
		return err
	}
	defer lease.Close()
	check, release, err := lockLegacyDelivery(ctx, state)
	if err != nil {
		return err
	}
	defer release()
	dir, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	boundary := func(at string) error {
		if err := backupRecoveryBoundary(fault, at); err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
		if err := backupCutoverLease(dir, "notifier.lock", lease); err != nil {
			return err
		}
		return backupCutoverLease(dir, "schema.lock", state.schemaLease)
	}
	if err = requireBackupCutoverOwner(state, spec.Review.Owner); err != nil {
		return err
	}
	key := legacyResolutionPrefix + notify.Hash(spec.Review.Original)
	name := legacyResolutionFile(notify.Hash(spec.Review.Original))
	var retained backupLegacyResolution
	err = dir.ReadJSON(name, &retained)
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		return err
	}
	if !fresh && notify.Hash(retained) != notify.Hash(spec) {
		return fmt.Errorf("legacy resolution retry changed retained intent")
	}
	return backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		if err := check(); err != nil {
			return err
		}
		if err := backupCutoverLease(dir, "notifier.lock", lease); err != nil {
			return err
		}
		var prior backupLegacyResolution
		err := backupNotificationMeta(tx, key, &prior)
		if err == nil {
			if fresh || notify.Hash(prior) != notify.Hash(spec) {
				return fmt.Errorf("legacy resolution SQL/file disagreement")
			}
			_, _, err = readBackupLegacyResolution(tx, spec.Review.Original.ID)
			return err // A committed decision never reopens delivered work.
		}
		if err != sql.ErrNoRows {
			return err
		}
		current, err := readBackupLegacyReview(tx, c, spec.Review.Original.ID, spec.Review.ReviewedAt)
		if err != nil {
			return err
		}
		comparison := current
		if !fresh {
			// Publication pins the decision. Domain report refreshes may continue
			// while the SQL transaction rolls back, without changing send evidence.
			var oldEvent, newEvent notification.NotificationEvent
			if err = notify.Decode([]byte(spec.Review.Current.Record), &oldEvent, notify.MaxRequest); err != nil {
				return err
			}
			if err = notify.Decode([]byte(current.Current.Record), &newEvent, notify.MaxRequest); err != nil {
				return err
			}
			newEvent.Message = oldEvent.Message
			if notify.Hash(newEvent) == notify.Hash(oldEvent) {
				comparison.Current.Record = spec.Review.Current.Record
				comparison.Current.Due = spec.Review.Current.Due
			}
		}
		if notify.Hash(comparison) != spec.ReviewHash {
			return fmt.Errorf("legacy review is stale; source or ownership evidence changed")
		}
		if fresh && (now.Before(spec.Review.ReviewedAt) || now.Sub(spec.Review.ReviewedAt) > 5*time.Minute) {
			return fmt.Errorf("legacy review expired; review again")
		}
		var event notification.NotificationEvent
		if err = notify.Decode([]byte(current.Current.Record), &event, notify.MaxRequest); err != nil || event.ID != current.Current.ID {
			return fmt.Errorf("invalid legacy source envelope")
		}
		for _, attempt := range event.Attempts {
			if attempt.Accepted {
				return fmt.Errorf("accepted legacy attempt cannot be retried")
			}
		}
		if current.Journal.JournalPresent {
			var accepted bool
			if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM backup_legacy_delivery_journal i
 JOIN backup_legacy_delivery_journal o ON o.sequence=i.sequence+1
 WHERE json_extract(i.body,'$.intent.event.id')=? AND json_extract(o.body,'$.outcome.state')='accepted')`, event.ID).Scan(&accepted); err != nil {
				return err
			}
			if accepted {
				return fmt.Errorf("retained journal proves prior acceptance")
			}
		}
		if spec.Action == "unsent" {
			if err = requireBackupUnattemptedSource(tx, event); err != nil {
				return err
			}
		}
		if fresh {
			if err = dir.WriteJSON(name, spec, false); err != nil {
				return err
			}
		}
		if err = boundary("legacy-resolution-intent"); err != nil {
			return err
		}
		if err = storeBackupEvaluationMeta(tx, key, spec); err != nil {
			return err
		}
		return boundary("legacy-resolution-recorded")
	})
}

// File evidence is checked before report evaluation/transfer. A lost SQL grant
// leaves the original row held; a lost file cannot leave an effective SQL grant.
func requireBackupLegacyResolutionFiles(state *backupState, dir *safefs.Directory) error {
	rows, err := state.db.Query("SELECT key,value FROM meta WHERE key LIKE ? ORDER BY key LIMIT 10001", legacyResolutionPrefix+"%")
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	var sources []string
	for rows.Next() {
		count++
		var key, raw string
		var spec, retained backupLegacyResolution
		if err = rows.Scan(&key, &raw); err != nil {
			return err
		}
		if count > 10000 || notify.Decode([]byte(raw), &spec, safefs.MaxRecordBytes) != nil || spec.validate() != nil || key != legacyResolutionPrefix+notify.Hash(spec.Review.Original) {
			return fmt.Errorf("invalid or excessive legacy resolution history")
		}
		if err = dir.ReadJSON(legacyResolutionFile(notify.Hash(spec.Review.Original)), &retained); err != nil {
			return err
		}
		if notify.Hash(retained) != notify.Hash(spec) {
			return fmt.Errorf("legacy resolution file changed")
		}
		sources = append(sources, spec.Review.Original.ID)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(sources) == 0 {
		return nil
	}
	return backupTransferTx(context.Background(), state, func(tx *sql.Tx) error {
		for _, source := range sources {
			if _, present, err := readBackupLegacyResolution(tx, source); err != nil || !present {
				return fmt.Errorf("legacy resolution lost source authority: %s", source)
			}
		}
		return nil
	})
}
