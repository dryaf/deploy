package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/notification/client"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupReportHealth struct {
	Binding   backupNotificationBinding
	At        time.Time
	Available bool
}
type backupReportDependencies struct {
	API       backupNotificationAPI
	Clock     func() backupEvaluationClock
	Heartbeat func(context.Context, string, bool) error
}

func reportHostClock() backupEvaluationClock {
	boot, uptime, err := process.HostClock()
	if err != nil {
		boot = ""
		uptime = 0
	}
	return backupEvaluationClock{time.Now().UTC(), boot, uptime}
}

func ensureSharedDeliveryCatchup(tx *sql.Tx, now time.Time) error {
	outage, err := backupNotificationOutage(tx)
	if err != nil || outage.ID == "" {
		return err
	}
	if outage.CatchupID == "" {
		outage.CatchupID = "catchup/" + outage.ID
		if err = storeBackupEvaluationMeta(tx, "delivery_outage", outage); err != nil {
			return err
		}
	}
	return enqueueNotification(tx, notification.NotificationEvent{ID: outage.CatchupID, Kind: "delivery-catchup", Message: "Awaiting a current delivery catch-up evaluation."}, now)
}

func markSharedDeliveryOutage(tx *sql.Tx, now time.Time) error {
	outage, err := backupNotificationOutage(tx)
	if err != nil || outage.ID != "" {
		return err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	return storeBackupEvaluationMeta(tx, "delivery_outage", deliveryOutage{ID: id, StartedAt: now})
}

// Runs only after a journaled handoff has installed a matching shared owner.
// The canonical gateway still grants every messenger attempt independently.
func runSharedBackupReport(ctx context.Context, c WorkerInstallation, state *backupState) error {
	return runSharedBackupReportWith(ctx, c, state, backupReportDependencies{Clock: reportHostClock, Heartbeat: notification.Heartbeat})
}

func runSharedBackupReportWith(ctx context.Context, c WorkerInstallation, state *backupState, deps backupReportDependencies) error {
	if deps.Clock == nil || deps.Heartbeat == nil {
		return fmt.Errorf("report dependencies missing")
	}
	if err := notification.ValidateMonitoringChecks(c.Policy.Monitoring); err != nil {
		return err
	}
	lease, err := backupNotifierLease(state)
	if err != nil {
		return err
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	var owner backupNotificationOwner
	err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		var err error
		owner, err = readBackupNotificationOwner(tx)
		if err == nil && owner.Mode != "shared" {
			err = fmt.Errorf("shared notification handoff is not committed")
		}
		return err
	})
	if err != nil {
		return err
	}
	if deps.API == nil {
		deps.API = client.Client{Socket: owner.Socket, Identity: owner.Binding.Identity}
	}
	evaluate := func() (evaluation backupNotificationEvaluation, err error) {
		clock := deps.Clock()
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			current, err := readBackupNotificationOwner(tx)
			if err != nil {
				return err
			}
			if current != owner {
				return fmt.Errorf("notification owner changed during report")
			}
			if err = coalesceBackupBacklog(tx, clock.Now, true); err != nil {
				return err
			}
			if err = enqueueBackupDaily(tx, c, clock.Now, "Awaiting current backup evaluation."); err != nil {
				return err
			}
			if err = ensureSharedDeliveryCatchup(tx, clock.Now); err != nil {
				return err
			}
			evaluation, _, err = evaluateBackupNotificationPass(tx, c, clock)
			return err
		})
		return
	}
	_, evaluationErr := evaluate()
	var transferErr error
	if evaluationErr == nil {
		// Reserve time for independent monitoring if the local API hangs or fails.
		transferCtx, stop := context.WithTimeout(ctx, 20*time.Second)
		var ownership notify.OwnershipStatus
		var identity notify.Identity
		identity, transferErr = deps.API.Call(transferCtx, notify.Request{Operation: "producer.ownership", Expected: owner.Binding.Identity, Producer: owner.Binding.Producer, Generation: owner.Binding.Generation}, &ownership)
		if transferErr == nil && (identity != owner.Binding.Identity || ownership.Producer != owner.Binding.Producer || ownership.Generation != owner.Binding.Generation || ownership.State != "active" || !ownership.Active) {
			transferErr = fmt.Errorf("canonical notification ownership is not active")
		}
		if transferErr == nil {
			transferErr = transferBackupNotifications(transferCtx, state, deps.API)
		}
		stop()
	}
	now := deps.Clock().Now
	recordErr := backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		current, err := readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if current != owner {
			return fmt.Errorf("notification owner changed during report")
		}
		var failed bool
		if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM notification_obligations o JOIN notification_exports e ON e.id=o.representative WHERE o.disposition='open' AND e.state IN ('retry_wait','blocked','ambiguous','fenced','expired'))").Scan(&failed); err != nil {
			return err
		}
		if failed || transferErr != nil {
			if err = markSharedDeliveryOutage(tx, now); err != nil {
				return err
			}
		}
		if evaluationErr != nil {
			if err = storeBackupEvaluationMeta(tx, "shared_daily_ping_pending", true); err != nil {
				return err
			}
		}
		return storeBackupEvaluationMeta(tx, "shared_report_health", backupReportHealth{owner.Binding, now, evaluationErr == nil && transferErr == nil && !failed})
	})
	if recordErr != nil {
		return errors.Join(evaluationErr, transferErr, recordErr)
	}
	// Serialize pause/resume and coverage publication as in the legacy notifier.
	schedule, err := schedulerLockContext(ctx, state)
	if err != nil {
		return errors.Join(evaluationErr, transferErr, err)
	}
	defer schedule.Close()
	evaluation, refreshErr := evaluate()
	if refreshErr != nil {
		failureErr := backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			current, err := readBackupNotificationOwner(tx)
			if err != nil {
				return err
			}
			if current != owner {
				return fmt.Errorf("notification owner changed during report")
			}
			if err = storeBackupEvaluationMeta(tx, "shared_report_health", backupReportHealth{owner.Binding, deps.Clock().Now, false}); err != nil {
				return err
			}
			return storeBackupEvaluationMeta(tx, "shared_daily_ping_pending", true)
		})
		var monitorErr error
		for _, path := range []string{c.Policy.Monitoring.BackupPingURLFile, c.Policy.Monitoring.DailyDeliveryPingURLFile} {
			if path != "" {
				monitorErr = errors.Join(monitorErr, deps.Heartbeat(ctx, path, true))
			}
		}
		return errors.Join(evaluationErr, transferErr, refreshErr, failureErr, monitorErr)
	}
	backupErr := publishSharedBackupHeartbeat(ctx, state, c, evaluation, deps.Heartbeat)
	deliveryErr := publishSharedDailyDeliveryHeartbeat(ctx, state, c, deps.Clock().Now, deps.Heartbeat)
	if err = state.put("shared_report_last_pass", deps.Clock().Now); err != nil {
		return errors.Join(evaluationErr, transferErr, backupErr, deliveryErr, err)
	}
	return errors.Join(evaluationErr, transferErr, backupErr, deliveryErr)
}

func publishSharedBackupHeartbeat(ctx context.Context, state *backupState, c WorkerInstallation, e backupNotificationEvaluation, heartbeat func(context.Context, string, bool) error) error {
	endpoint := c.Policy.Monitoring.BackupPingURLFile
	if endpoint == "" {
		return nil
	}
	if len(e.Snapshot.Subjects) == 0 || e.Snapshot.Subjects[0].Key != "backup" || e.Snapshot.Subjects[0].Condition != "ok" {
		return heartbeat(ctx, endpoint, true)
	}
	var run WorkerRun
	if err := state.get("run", &run); err != nil {
		return err
	}
	var previous string
	if err := state.get("backup_ping_run", &previous); err != nil && err != sql.ErrNoRows {
		return err
	}
	if !run.Complete || run.ID == previous {
		return nil
	}
	if err := heartbeat(ctx, endpoint, false); err != nil {
		return err
	}
	return backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		owner, err := readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if owner.Mode != "shared" || owner.Binding.Producer != e.Snapshot.Producer || c.ConfigHash != e.Snapshot.ConfigRevision {
			return fmt.Errorf("backup heartbeat authority changed")
		}
		var paused time.Time
		var suspect bool
		if err = backupEvaluationMeta(tx, "paused_until", &paused); err != nil && err != sql.ErrNoRows {
			return err
		}
		if err = backupEvaluationMeta(tx, "clock_suspect", &suspect); err != nil && err != sql.ErrNoRows {
			return err
		}
		if !paused.IsZero() || suspect {
			return fmt.Errorf("backup health changed during heartbeat")
		}
		var current WorkerRun
		if err = backupEvaluationMeta(tx, "run", &current); err != nil {
			return err
		}
		if current.ID != run.ID || !current.Complete || current.ConfigHash != c.ConfigHash {
			return fmt.Errorf("backup run changed during heartbeat")
		}
		return storeBackupEvaluationMeta(tx, "backup_ping_run", run.ID)
	})
}
