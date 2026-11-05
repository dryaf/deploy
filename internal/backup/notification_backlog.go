package backup

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func coalesceNotificationBacklog(state *backupState, now time.Time) error {
	tx, err := state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = coalesceBackupBacklog(tx, now, false); err != nil {
		return err
	}
	return tx.Commit()
}

func coalesceBackupBacklog(tx *sql.Tx, now time.Time, shared bool) error {
	filter := ""
	// Automatic shared batching only considers never-represented sources. A
	// sending/ambiguous or previously accepted attempt cannot enter a new batch.
	if shared {
		filter = " AND NOT EXISTS(SELECT 1 FROM notification_members m WHERE m.obligation=b.id) AND NOT EXISTS(SELECT 1 FROM backup_notification_legacy l WHERE l.source=b.id AND l.disposition='held') "
	}
	rows, err := tx.Query("SELECT id,kind,created,record FROM outbox b WHERE accepted IS NULL AND covered_by IS NULL AND kind NOT IN ('daily','delivery-catchup','batch') AND due<=? "+filter+" ORDER BY created,id LIMIT 128", now.UTC().Format(eventTimeLayout))
	if err != nil {
		return err
	}
	var ids []string
	counts := map[string]int{}
	var oldest string
	var messages []string
	for rows.Next() {
		var id, kind, created, raw string
		if err = rows.Scan(&id, &kind, &created, &raw); err != nil {
			rows.Close()
			return err
		}
		var event notification.NotificationEvent
		if err = protocol.StrictBackupJSON([]byte(raw), &event); err != nil {
			rows.Close()
			return err
		}
		// Keep attempted envelopes under their original identities. In particular,
		// an ambiguous send must not become an apparently unsent batch member.
		if len(event.Attempts) != 0 {
			continue
		}
		if oldest == "" {
			oldest = created
		}
		ids = append(ids, id)
		counts[kind]++
		if len(messages) < 8 {
			messages = append(messages, event.Message)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) < 8 {
		return nil
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	event := notification.NotificationEvent{ID: "batch/" + id, Kind: "batch", Members: ids, Message: notification.NotificationEnvelope(fmt.Sprintf("Backup alert backlog: %d events; failures %d, reminders %d, recoveries %d. Oldest queued: %s. Historical recovery events do not establish current protection.\n%s", len(ids), counts["failure"], counts["reminder"], counts["recovery"], oldest, strings.Join(messages, "\n")))}
	if err = enqueueNotification(tx, event, now); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.Exec("UPDATE outbox SET covered_by=? WHERE id=? AND accepted IS NULL AND covered_by IS NULL", event.ID, id); err != nil {
			return err
		}
	}
	return nil
}

func publishDailyDeliveryHeartbeat(ctx context.Context, state *backupState, c WorkerInstallation, now time.Time) error {
	if c.Policy.Monitoring.DailyDeliveryPingURLFile == "" {
		return nil
	}
	zone, err := time.LoadLocation(c.Policy.Timezone)
	if err != nil {
		return err
	}
	local := now.In(zone)
	if local.Format("15:04") < c.Policy.Notifications.DailyAt {
		local = local.AddDate(0, 0, -1)
	}
	var started time.Time
	if state.get("daily_started", &started) == nil {
		first := started.In(zone)
		firstDay := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, zone)
		if local.Before(firstDay) {
			return nil
		}
	}
	id := "daily/" + c.WorkerID + "/" + local.Format("2006-01-02")
	var accepted int
	if err = state.db.QueryRow("SELECT count(*) FROM outbox WHERE id=? AND accepted IS NOT NULL AND covered_by IS NULL", id).Scan(&accepted); err != nil {
		return err
	}
	if accepted == 0 {
		return notification.Heartbeat(ctx, c.Policy.Monitoring.DailyDeliveryPingURLFile, true)
	}
	var outage deliveryOutage
	_ = state.get("delivery_outage", &outage)
	if outage.ID != "" {
		var acceptedCatchup, pending int
		if err = state.db.QueryRow("SELECT count(*) FROM outbox WHERE id=? AND accepted IS NOT NULL", outage.CatchupID).Scan(&acceptedCatchup); err != nil {
			return err
		}
		if err = state.db.QueryRow("SELECT count(*) FROM outbox WHERE accepted IS NULL AND covered_by IS NULL").Scan(&pending); err != nil {
			return err
		}
		if acceptedCatchup == 0 || pending > 0 {
			return notification.Heartbeat(ctx, c.Policy.Monitoring.DailyDeliveryPingURLFile, true)
		}
	}
	var previous string
	_ = state.get("daily_ping_period", &previous)
	if previous == id && outage.ID == "" {
		return nil
	}
	if err = notification.Heartbeat(ctx, c.Policy.Monitoring.DailyDeliveryPingURLFile, false); err != nil {
		return err
	}
	if err = state.put("daily_ping_period", id); err != nil {
		return err
	}
	return state.put("delivery_outage", deliveryOutage{})
}

type deliveryOutage struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	CatchupID string    `json:"catchup_id,omitempty"`
}

func markDeliveryOutage(state *backupState, now time.Time) error {
	var outage deliveryOutage
	_ = state.get("delivery_outage", &outage)
	if outage.ID != "" {
		var accepted int
		if outage.CatchupID == "" {
			return nil
		}
		if err := state.db.QueryRow("SELECT count(*) FROM outbox WHERE id=? AND accepted IS NOT NULL", outage.CatchupID).Scan(&accepted); err != nil {
			return err
		}
		if accepted == 0 {
			return nil
		}
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	return state.put("delivery_outage", deliveryOutage{ID: id, StartedAt: now})
}

func enqueueDeliveryCatchup(state *backupState, now time.Time, summary string) error {
	var outage deliveryOutage
	_ = state.get("delivery_outage", &outage)
	if outage.ID == "" {
		return nil
	}
	if outage.CatchupID != "" {
		// Repair legacy catch-ups suppressed by daily coalescing. Preserve their
		// provider attempts and retry schedule; never manufacture acceptance.
		_, err := state.db.Exec("UPDATE outbox SET kind='delivery-catchup',covered_by=NULL,record=json_set(record,'$.kind','delivery-catchup') WHERE id=? AND accepted IS NULL", outage.CatchupID)
		return err
	}
	var existing int
	if err := state.db.QueryRow("SELECT count(*) FROM outbox WHERE id=?", "catchup/"+outage.ID).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		outage.CatchupID = "catchup/" + outage.ID
		return state.put("delivery_outage", outage)
	}
	var pending int
	if err := state.db.QueryRow("SELECT count(*) FROM outbox WHERE accepted IS NULL AND covered_by IS NULL").Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return nil
	}
	// A stable ID makes the enqueue/metadata acknowledgement crash window safe.
	outage.CatchupID = "catchup/" + outage.ID
	event := notification.NotificationEvent{ID: outage.CatchupID, Kind: "delivery-catchup", Message: "Current backup and delivery recovery report\n" + summary}
	tx, err := state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = enqueueNotification(tx, event, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return state.put("delivery_outage", outage)
}
