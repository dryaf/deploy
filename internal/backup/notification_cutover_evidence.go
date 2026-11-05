package backup

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/backup/notification"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

type backupCutoverRow struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Created   string         `json:"created"`
	Due       string         `json:"due"`
	Attempts  int            `json:"attempts"`
	Accepted  sql.NullString `json:"accepted"`
	CoveredBy sql.NullString `json:"covered_by"`
	Record    string         `json:"record"`
}
type backupCutoverEvidence struct {
	Version int                  `json:"version"`
	Control ArchiveControl       `json:"control"`
	Journal legacyDeliveryReview `json:"journal"`
	Rows    []backupCutoverRow   `json:"rows"`
}
type backupCutoverDecision struct {
	Hash        string `json:"hash"`
	Disposition string `json:"disposition"` // accepted, unsent, held
	Reason      string `json:"reason"`
}

func readBackupCutoverEvidence(tx *sql.Tx) (e backupCutoverEvidence, err error) {
	e.Version = 1
	e.Control, err = incidentControl(tx)
	if err != nil {
		return
	}
	journal, err := readLegacyDeliveryJournal(tx)
	if err != nil {
		return e, err
	}
	e.Journal = journal.review
	rows, err := tx.Query("SELECT id,kind,created,due,attempts,accepted,covered_by,record FROM outbox ORDER BY id LIMIT 10001")
	if err != nil {
		return e, err
	}
	defer rows.Close()
	for rows.Next() {
		var r backupCutoverRow
		if err = rows.Scan(&r.ID, &r.Kind, &r.Created, &r.Due, &r.Attempts, &r.Accepted, &r.CoveredBy, &r.Record); err != nil {
			return
		}
		e.Rows = append(e.Rows, r)
	}
	if len(e.Rows) > 10000 {
		return e, fmt.Errorf("legacy outbox exceeds cutover review bound")
	}
	err = rows.Err()
	if err == nil && len(mustCutoverJSON(e)) > 4<<20 {
		err = fmt.Errorf("legacy evidence exceeds cutover record bound")
	}
	return
}
func validateBackupCutoverDecisions(tx *sql.Tx, e backupCutoverEvidence, decisions map[string]backupCutoverDecision) error {
	if len(e.Rows) != len(decisions) {
		return fmt.Errorf("every legacy row needs an exact reviewed disposition")
	}
	for _, r := range e.Rows {
		d, ok := decisions[r.ID]
		if !ok || d.Hash != notify.Hash(r) || len(d.Reason) == 0 || len(d.Reason) > 512 {
			return fmt.Errorf("missing or changed review for %s", r.ID)
		}
		if r.Accepted.Valid {
			if d.Disposition != "accepted" {
				return fmt.Errorf("accepted legacy evidence cannot become unsent")
			}
			continue
		}
		if d.Disposition == "held" {
			continue
		}
		if d.Disposition != "unsent" {
			return fmt.Errorf("unaccepted legacy row requires unsent proof or held disposition")
		}
		var event notification.NotificationEvent
		if err := notify.Decode([]byte(r.Record), &event, notify.MaxRequest); err != nil {
			return err
		}
		// An operator assertion supplements incomplete old helper history; it cannot
		// override contradictory durable attempts, counters or reservations.
		if err := requireBackupUnsentSource(tx, event); err != nil {
			return err
		}
	}
	return nil
}
func installBackupCutoverEvidence(tx *sql.Tx, e backupCutoverEvidence, decisions map[string]backupCutoverDecision) error {
	for _, r := range e.Rows {
		d := decisions[r.ID]
		if _, err := tx.Exec("INSERT INTO backup_notification_legacy(source,body,hash,disposition,reason) VALUES(?,?,?,?,?)", r.ID, mustCutoverJSON(r), d.Hash, d.Disposition, d.Reason); err != nil {
			return err
		}
	}
	return nil
}
func backupSourceHeld(tx *sql.Tx, event notification.NotificationEvent) (bool, error) {
	exists, err := notificationTable(tx, "backup_notification_legacy")
	if err != nil || !exists {
		return false, err
	}
	for _, id := range append([]string{event.ID}, event.Members...) {
		var held bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM backup_notification_legacy WHERE source=? AND disposition='held')", id).Scan(&held); err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

// The immutable intent was written while both legacy sender leases were held.
// Compatible senders honor that file even before SQL publication. Capture may
// continue to change domain state and enqueue new work after this boundary; those
// changes must not invalidate the reviewed historical evidence or require pausing
// backup capture. Changes to delivery outcomes or the attempt log still refuse.
func validateBackupCutoverFence(tx *sql.Tx, intent backupCutoverIntent) error {
	current, err := readBackupCutoverEvidence(tx)
	if err != nil {
		return err
	}
	before := intent.Evidence
	if current.Control.ArchiveID != before.Control.ArchiveID || current.Control.WriterID != before.Control.WriterID || current.Control.Epoch != before.Control.Epoch || current.Control.ConfigHash != before.Control.ConfigHash || current.Journal.Head != before.Journal.Head || current.Journal.JournalPresent != before.Journal.JournalPresent {
		return fmt.Errorf("legacy authority or attempt evidence changed behind cutover fence")
	}
	old := map[string]backupCutoverRow{}
	for _, row := range before.Rows {
		old[row.ID] = row
	}
	for _, row := range current.Rows {
		var event notification.NotificationEvent
		if err = notify.Decode([]byte(row.Record), &event, notify.MaxRequest); err != nil {
			return err
		}
		previous, existed := old[row.ID]
		if !existed {
			if row.Accepted.Valid || row.Attempts != 0 || len(event.Attempts) != 0 {
				return fmt.Errorf("new source has delivery evidence behind cutover fence")
			}
			continue
		}
		var prior notification.NotificationEvent
		if err = notify.Decode([]byte(previous.Record), &prior, notify.MaxRequest); err != nil {
			return err
		}
		if row.Accepted != previous.Accepted || row.Attempts != previous.Attempts || notify.Hash(event.Attempts) != notify.Hash(prior.Attempts) {
			return fmt.Errorf("legacy source outcome changed behind cutover fence")
		}
	}
	return nil
}
