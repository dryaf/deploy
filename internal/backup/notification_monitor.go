package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func backupNotificationMeta(tx *sql.Tx, key string, dest any) error {
	var raw string
	if err := tx.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&raw); err != nil {
		return err
	}
	limit := 8192
	if key == remigrationKey || key == migrationCyclePendingKey {
		limit = safefs.MaxRecordBytes
	}
	if err := notify.Decode([]byte(raw), dest, limit); err != nil {
		return fmt.Errorf("backup metadata %s: %w", key, err)
	}
	return nil
}

func backupNotificationOutage(tx *sql.Tx) (outage deliveryOutage, err error) {
	err = backupNotificationMeta(tx, "delivery_outage", &outage)
	if err == sql.ErrNoRows {
		err = nil
	}
	if err == nil && outage.ID != "" && (!notify.ValidToken(outage.ID) || outage.CatchupID != "" && outage.CatchupID != "catchup/"+outage.ID) {
		err = fmt.Errorf("invalid delivery outage identity")
	}
	if err == nil && outage.ID == "" && (outage.CatchupID != "" || !outage.StartedAt.IsZero()) {
		err = fmt.Errorf("incomplete delivery outage identity")
	}
	return
}

// The eventual shared service calls this under the notifier lease. The heartbeat
// is separate from messenger acceptance and backup health. No production path
// selects this publisher yet. Tests inject a local recorder, never a real URL.
func publishSharedDailyDeliveryHeartbeat(ctx context.Context, state *backupState, c WorkerInstallation, now time.Time, heartbeat func(context.Context, string, bool) error) error {
	endpoint := c.Policy.Monitoring.DailyDeliveryPingURLFile
	if endpoint == "" {
		return nil
	}
	if heartbeat == nil {
		return fmt.Errorf("delivery heartbeat transport required")
	}
	zone, err := time.LoadLocation(c.Policy.Timezone)
	if err != nil {
		return err
	}
	local := now.In(zone)
	if local.Format("15:04") < c.Policy.Notifications.DailyAt {
		local = local.AddDate(0, 0, -1)
	}
	period := "daily/" + c.WorkerID + "/" + local.Format("2006-01-02")
	var outage deliveryOutage
	var binding backupNotificationBinding
	var coverage backupDeliveryCoverage
	var previous string
	var monitorPending bool
	var notDue bool
	err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		var err error
		binding, err = readBackupNotificationBinding(tx)
		if err != nil {
			return err
		}
		var started time.Time
		if err = backupNotificationMeta(tx, "daily_started", &started); err != nil && err != sql.ErrNoRows {
			return err
		}
		if !started.IsZero() {
			first := started.In(zone)
			firstDay := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, zone)
			if local.Before(firstDay) {
				notDue = true
				return nil
			}
		}
		outage, err = backupNotificationOutage(tx)
		if err != nil {
			return err
		}
		coverage, err = readBackupDeliveryCoverage(tx, period, outage.ID)
		if err != nil {
			return err
		}
		if err = backupNotificationMeta(tx, "shared_daily_ping_pending", &monitorPending); err != nil && err != sql.ErrNoRows {
			return err
		}
		if !coverage.complete() {
			// Persist before sending: an uncertain failure heartbeat also needs a
			// later success, even when this period was already acknowledged.
			if _, err = tx.Exec("INSERT INTO meta(key,value) VALUES('shared_daily_ping_pending','true') ON CONFLICT(key) DO UPDATE SET value='true'"); err != nil {
				return err
			}
			monitorPending = true
		}
		err = backupNotificationMeta(tx, "daily_ping_period", &previous)
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	})
	if err != nil || notDue {
		return err
	}
	if !coverage.complete() {
		return heartbeat(ctx, endpoint, true)
	}
	if previous == period && outage.ID == "" && !monitorPending {
		return nil
	}
	// Socket/HTTP work occurs outside the SQLite transaction. A lost success reply
	// leaves the local acknowledgement pending and can safely repeat the heartbeat.
	if err = heartbeat(ctx, endpoint, false); err != nil {
		return err
	}
	return backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		currentBinding, err := readBackupNotificationBinding(tx)
		if err != nil {
			return err
		}
		if currentBinding != binding {
			return fmt.Errorf("notification binding changed during heartbeat")
		}
		current, err := backupNotificationOutage(tx)
		if err != nil {
			return err
		}
		if current != outage {
			return fmt.Errorf("delivery outage changed during heartbeat")
		}
		currentCoverage, err := readBackupDeliveryCoverage(tx, period, current.ID)
		if err != nil {
			return err
		}
		if !currentCoverage.complete() {
			return fmt.Errorf("delivery coverage changed during heartbeat")
		}
		// A receipt for an earlier outage, or a heartbeat from an earlier publisher
		// pass, must not clear a newer outage. Both metadata updates share this commit.
		for _, v := range []struct {
			key   string
			value any
		}{{"daily_ping_period", period}, {"delivery_outage", deliveryOutage{}}, {"shared_daily_ping_pending", false}} {
			body, err := json.Marshal(v.value)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", v.key, string(body)); err != nil {
				return err
			}
		}
		return nil
	})
}
