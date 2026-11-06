package backup

import (
	"context"
	"database/sql"
	"fmt"
)

// Shared delivery failure has its own outage and monitor. It must not manufacture
// a backup-health incident when host/storage/snapshot evidence remains healthy.
func recordSharedBackupHealthProbe(state *backupState, c WorkerInstallation, probe WorkerHealthProbe) (shared bool, err error) {
	err = backupTransferTx(context.Background(), state, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key='notification_owner')").Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return nil
		}
		owner, err := readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if owner.Mode == "legacy" {
			return nil
		}
		if owner.Mode != "shared" {
			return fmt.Errorf("notification ownership handoff is pending")
		}
		shared = true
		if err = storeBackupEvaluationMeta(tx, "health_probe", probe); err != nil {
			return err
		}
		if !probe.DeliveryHealthy {
			if err = markSharedDeliveryOutage(tx, probe.ObservedAt); err != nil {
				return err
			}
			if err = storeBackupEvaluationMeta(tx, "shared_daily_ping_pending", true); err != nil {
				return err
			}
		}
		if !probe.BackupHealthy {
			_, err = observeBackupIncident(tx, "host-health", "worker", "Independent backup host, timer or storage checks failed; inspect worker health diagnostics.", probe.ObservedAt, c.Policy.Notifications)
		} else {
			_, err = resolveBackupIncident(tx, "host-health", "worker", probe.ObservedAt, c.Policy.Notifications)
		}
		return err
	})
	return
}
