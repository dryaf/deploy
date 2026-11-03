package backup

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/client"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

// A narrow client interface permits lost-response injection without credentials.
type backupNotificationAPI interface {
	Call(context.Context, notify.Request, any) (notify.Identity, error)
}

var _ backupNotificationAPI = client.Client{}

func sharedReminderCancellation(tx *sql.Tx, incident, reason string) error {
	return sharedIncidentCancellation(tx, incident, reason, false)
}
func sharedIncidentCancellation(tx *sql.Tx, incident, reason string, retired bool) error {
	installed, err := notificationTable(tx, "backup_notification_transfers")
	if err != nil || !installed {
		return err
	}
	if len(reason) == 0 || len(reason) > 256 {
		return fmt.Errorf("bounded cancellation reason required")
	}
	// Intent and policy waiver commit with the domain decision. Other members of a
	// cancelled batch stay open; they can be re-exported after terminal settlement.
	if _, err = tx.Exec("UPDATE backup_notification_transfers SET cancel_reason=? WHERE event IN (SELECT m.event FROM notification_members m JOIN backup_notification_members b ON b.obligation=m.obligation JOIN notification_obligations o ON o.id=m.obligation WHERE (b.kind='reminder' OR (? AND b.kind='failure')) AND b.incident=? AND o.disposition='open') AND cancel_reason=''", reason, retired, incident); err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE notification_obligations SET disposition='waived',reason=? WHERE disposition='open' AND id IN (SELECT obligation FROM backup_notification_members WHERE (kind='reminder' OR (? AND kind='failure')) AND incident=?)", reason, retired, incident)
	return err
}

// Resolution changes a batch's incident guard even when it contains only the
// original failure and no reminder. Keep those failure obligations open, cancel
// the obsolete envelope, and return its members to independent evaluation.
func cancelResolvedBackupBatches(tx *sql.Tx, incident string) error {
	rows, err := tx.Query("SELECT DISTINCT covered_by FROM outbox WHERE accepted IS NULL AND kind IN ('failure','reminder') AND json_extract(record,'$.incident_id')=? AND covered_by LIKE 'batch/%'", incident)
	if err != nil {
		return err
	}
	var batches []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		batches = append(batches, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range batches {
		if _, err = tx.Exec("UPDATE backup_notification_transfers SET cancel_reason='incident resolved' WHERE cancel_reason='' AND event IN (SELECT event FROM backup_notification_sources WHERE source=?)", id); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE outbox SET covered_by=NULL WHERE covered_by=? AND accepted IS NULL", id); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE outbox SET covered_by=? WHERE id=? AND accepted IS NULL", "resolved/"+incident, id); err != nil {
			return err
		}
	}
	return nil
}

type backupReceiptPage struct {
	Receipts []notify.Receipt
	Cursor   notify.Cursor
}

func importBackupNotificationPage(tx *sql.Tx, page backupReceiptPage) error {
	if _, err := readBackupNotificationBinding(tx); err != nil {
		return err
	}
	if err := importNotificationReceipts(tx, page.Cursor, page.Receipts); err != nil {
		return err
	}
	// Only eligible acceptance can cover an outbox row. A batch's historical
	// members remain explicitly unsent; their coverage is in the obligation ledger.
	for _, r := range page.Receipts {
		if r.State != "provider_accepted" || r.Acceptance == nil || !r.Acceptance.Eligible {
			continue
		}
		// Intermediate catch-up pages satisfy their periods, not the source's
		// outage. Only an accepted final page may mark that source as delivered.
		if _, err := tx.Exec(`UPDATE outbox SET accepted=COALESCE(accepted,?) WHERE id IN (SELECT source FROM backup_notification_sources WHERE event=?)
 AND (kind<>'delivery-catchup' OR EXISTS (SELECT 1 FROM notification_members m JOIN notification_obligations o ON o.id=m.obligation WHERE m.event=? AND o.id=substr(outbox.id,9) AND o.kind='catchup' AND o.disposition='satisfied'))`, r.Acceptance.ObservedAt.UTC().Format(eventTimeLayout), r.EventID, r.EventID); err != nil {
			return err
		}
		// An earlier representative may win a race with a later queued replacement.
		// Cancel redundant successors only when every member is already disposed.
		if _, err := tx.Exec("UPDATE backup_notification_transfers SET cancel_reason='all obligations settled' WHERE cancel_reason='' AND event IN (SELECT id FROM notification_exports WHERE state NOT IN ('expired','superseded','cancelled','provider_accepted')) AND event IN (SELECT event FROM notification_members) AND NOT EXISTS (SELECT 1 FROM notification_members m JOIN notification_obligations o ON o.id=m.obligation WHERE m.event=backup_notification_transfers.event AND o.disposition='open') AND event<>?", r.EventID); err != nil {
			return err
		}
	}
	return nil
}

func backupTransferTx(ctx context.Context, s *backupState, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// The future shared report service holds notifier.lock across this bounded pass.
// Normal notifier dispatch never calls it: production ownership migration is not
// yet enabled. SQLite transactions are always closed before local socket I/O.
func transferBackupNotifications(ctx context.Context, s *backupState, api backupNotificationAPI) error {
	var b backupNotificationBinding
	var snap *notify.Snapshot
	err := backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if err := requireNotificationRecoveryInactive(tx); err != nil {
			return err
		}
		var err error
		b, err = readBackupNotificationBinding(tx)
		if err != nil {
			return err
		}
		var body string
		err = tx.QueryRow("SELECT body FROM backup_notification_snapshots ORDER BY sequence DESC LIMIT 1").Scan(&body)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		snap = &notify.Snapshot{}
		return notify.Decode([]byte(body), snap, notify.MaxRequest)
	})
	if err != nil {
		return err
	}
	call := func(r notify.Request, out any) error {
		r.Expected = b.Identity
		id, err := api.Call(ctx, r, out)
		if err != nil {
			return err
		}
		if id != b.Identity {
			return fmt.Errorf("notification identity changed")
		}
		return nil
	}
	receive := func() error {
		cursor := notify.Cursor{Identity: b.Identity, Producer: b.Producer}
		if err := s.db.QueryRowContext(ctx, "SELECT cursor FROM notification_producer WHERE singleton=1").Scan(&cursor.Position); err != nil {
			return err
		}
		// Repeat the last committed ack before asking for another page. A lost ack
		// must not cause any already-imported effects to be applied again.
		if err := call(notify.Request{Operation: "receipt.ack", Producer: b.Producer, Cursor: &cursor}, nil); err != nil {
			return err
		}
		var page backupReceiptPage
		if err := call(notify.Request{Operation: "receipt.list", Producer: b.Producer, Cursor: &cursor, Limit: 128}, &page); err != nil {
			return err
		}
		if err := backupTransferTx(ctx, s, func(tx *sql.Tx) error { return importBackupNotificationPage(tx, page) }); err != nil {
			return err
		}
		return call(notify.Request{Operation: "receipt.ack", Producer: b.Producer, Cursor: &page.Cursor}, nil)
	}
	if err = receive(); err != nil {
		return err
	}
	if snap != nil {
		if err = call(notify.Request{Operation: "status.replace", Producer: b.Producer, Snapshot: snap}, nil); err != nil {
			return err
		}
	}
	type transfer struct {
		m      notify.Message
		cancel string
	}
	var items []transfer
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if _, err := readBackupNotificationBinding(tx); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT e.body,t.cancel_reason FROM notification_exports e JOIN backup_notification_transfers t ON t.event=e.id WHERE t.submitted=0 OR (t.cancel_reason<>'' AND t.cancel_sent=0) ORDER BY e.seq LIMIT 32")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item transfer
			var body string
			if err = rows.Scan(&body, &item.cancel); err != nil {
				return err
			}
			if err = notify.Decode([]byte(body), &item.m, notify.MaxRequest); err != nil {
				return err
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, item := range items {
		// Recheck cancellation immediately before handoff. It can be committed while
		// the bounded candidate list is being processed. An already-sent enqueue is
		// followed by its durable cancellation on this or the next pass.
		if err = s.db.QueryRowContext(ctx, "SELECT cancel_reason FROM backup_notification_transfers WHERE event=?", item.m.ID).Scan(&item.cancel); err != nil {
			return err
		}
		r := notify.Request{Operation: "message.enqueue", Producer: b.Producer, Message: &item.m}
		if item.cancel != "" {
			r = notify.Request{Operation: "message.cancel", Producer: b.Producer, EventID: item.m.ID, Sequence: item.m.Sequence, PayloadHash: notify.Hash(item.m)}
		}
		if err = call(r, nil); err != nil {
			return err
		}
		if err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
			if _, err := readBackupNotificationBinding(tx); err != nil {
				return err
			}
			_, err := tx.Exec("UPDATE backup_notification_transfers SET submitted=1,cancel_sent=max(cancel_sent,?) WHERE event=?", item.cancel != "", item.m.ID)
			return err
		}); err != nil {
			return err
		}
	}
	return receive()
}
