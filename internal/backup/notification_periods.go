package backup

import (
	"database/sql"

	"github.com/dryaf/deploy/internal/backup/notification"
)

// Legacy daily coalescing is an unsent-history link, not proof that a later
// daily report delivered an earlier period. Preserve those periods for catch-up.
func registerBackupDailyObligations(tx *sql.Tx) error {
	if _, err := tx.Exec("INSERT INTO notification_obligations(id,kind,disposition) SELECT id,'daily','open' FROM outbox WHERE kind='daily' AND accepted IS NULL ON CONFLICT(id) DO NOTHING"); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO backup_notification_members(obligation,source,kind,incident) SELECT id,id,'daily','' FROM outbox WHERE kind='daily' AND accepted IS NULL ON CONFLICT(obligation) DO NOTHING"); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE backup_notification_transfers SET cancel_reason='older daily period requires catchup' WHERE cancel_reason='' AND event IN (SELECT m.event FROM notification_members m JOIN outbox o ON o.id=m.obligation JOIN notification_exports e ON e.id=m.event WHERE o.kind='daily' AND o.accepted IS NULL AND o.covered_by IS NOT NULL AND json_extract(e.body,'$.report_type')='daily')")
	return err
}

func backupCatchupMembers(tx *sql.Tx, event notification.NotificationEvent) (members []notification.NotificationEvent, ready bool, err error) {
	var pending int
	// Unexported live alerts/reports must also block recovery; the shared ledger
	// alone cannot prove that the source outbox has been fully drained.
	if err = tx.QueryRow("SELECT count(*) FROM outbox b WHERE "+backupPendingSourcePredicate+" AND b.id<>?", event.ID).Scan(&pending); err != nil {
		return
	}
	if pending != 0 {
		return
	}
	if err = tx.QueryRow("SELECT count(*) FROM notification_obligations o WHERE o.disposition='open' AND o.id<>? AND NOT(o.kind='daily' AND EXISTS(SELECT 1 FROM outbox b WHERE b.id=o.id AND b.kind='daily' AND b.accepted IS NULL AND b.covered_by IS NOT NULL))", event.ID[len("catchup/"):]).Scan(&pending); err != nil {
		return
	}
	if pending != 0 {
		return
	}
	rows, err := tx.Query("SELECT b.id FROM outbox b JOIN notification_obligations o ON o.id=b.id WHERE b.kind='daily' AND b.accepted IS NULL AND b.covered_by IS NOT NULL AND o.disposition='open' ORDER BY b.id LIMIT 128")
	if err != nil {
		return
	}
	// A full page contains only historical periods. It cannot settle the outage.
	// The final page reserves one membership slot for the outage obligation.
	// Select and freeze the exact page before assigning its submission sequence.
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		members = append(members, notification.NotificationEvent{ID: id, Kind: "daily"})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	if len(members) < 128 {
		members = append([]notification.NotificationEvent{event}, members...)
	}
	// Keep the outage visible even while intermediate pages cover only periods.
	// Its representative is assigned only to the final reconciliation message.
	if _, err = tx.Exec("INSERT INTO notification_obligations(id,kind,disposition) VALUES(?,'catchup','open') ON CONFLICT(id) DO NOTHING", event.ID[len("catchup/"):]); err != nil {
		return nil, false, err
	}
	return members, true, nil
}
