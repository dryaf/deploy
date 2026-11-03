package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

const eventTimeLayout = "2006-01-02T15:04:05.000000000Z"

func enqueueNotification(tx *sql.Tx, event notification.NotificationEvent, now time.Time) error {
	if err := freezeBackupEventContext(tx, event); err != nil {
		return err
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO outbox(id,kind,created,due,record) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING", event.ID, event.Kind, now.UTC().Format(eventTimeLayout), now.UTC().Format(eventTimeLayout), string(body))
	return err
}

func (s *backupState) observeIncident(class, scope, message string, now time.Time, policy config.WorkerNotifications) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := observeBackupIncident(tx, class, scope, message, now, policy)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if changed {
		return s.wakeNotifier()
	}
	return nil
}

func observeBackupIncident(tx *sql.Tx, class, scope, message string, now time.Time, policy config.WorkerNotifications) (bool, error) {
	control, err := incidentControl(tx)
	if err != nil {
		return false, err
	}
	if retirementReason(control, scope) != "" {
		return false, nil
	}
	var id, opened string
	err = tx.QueryRow("SELECT id,opened FROM incidents WHERE class=? AND instance=? AND resolved IS NULL", class, scope).Scan(&id, &opened)
	if err == sql.ErrNoRows {
		id, err = safefs.NewID("n")
		if err != nil {
			return false, err
		}
		when := now.UTC().Format(eventTimeLayout)
		opened = when
		if _, err = tx.Exec("INSERT INTO incidents(id,class,instance,opened,last_observed,message) VALUES(?,?,?,?,?,?)", id, class, scope, when, when, message); err != nil {
			return false, err
		}
		if err = enqueueNotification(tx, notification.NotificationEvent{ID: id + "/failure", Kind: "failure", IncidentID: id, Message: fmt.Sprintf("Backup failure [%s] %s: %s. Incident %s", class, scope, message, id)}, now); err != nil {
			return false, err
		}
		if _, err = syncBackupCommandEvidence(tx, id, class, opened, message); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	} else {
		if _, err = tx.Exec("UPDATE incidents SET last_observed=?,message=? WHERE id=?", now.UTC().Format(eventTimeLayout), message, id); err != nil {
			return false, err
		}
		evidence, e := syncBackupCommandEvidence(tx, id, class, opened, message)
		if e != nil {
			return false, e
		}
		suppressed, e := incidentReminderSuppressed(tx, id, evidence, now)
		if e != nil {
			return false, e
		}
		started, err := time.Parse(eventTimeLayout, opened)
		if err != nil {
			return false, err
		}
		interval := config.Budget(policy.RepeatAfter, "6h")
		slot := int64(now.Sub(started) / interval)
		suffix, changed, e := reminderPolicySuffix(tx, id, evidence)
		if e != nil {
			return false, e
		}
		if (slot > 0 || changed) && !suppressed {
			if err = enqueueNotification(tx, notification.NotificationEvent{ID: fmt.Sprintf("%s/reminder/%d%s", id, slot, suffix), Kind: "reminder", IncidentID: id, Message: fmt.Sprintf("Backup failure remains unresolved [%s] %s: %s. Incident %s", class, scope, message, id)}, now); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func (s *backupState) resolveIncident(class, scope string, now time.Time, policy config.WorkerNotifications) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := resolveBackupIncident(tx, class, scope, now, policy)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if changed {
		return s.wakeNotifier()
	}
	return nil
}

func resolveBackupIncident(tx *sql.Tx, class, scope string, now time.Time, policy config.WorkerNotifications) (bool, error) {
	control, err := incidentControl(tx)
	if err != nil {
		return false, err
	}
	if retirementReason(control, scope) != "" {
		return false, nil // Retirement reconciliation supplies the distinct disposition.
	}
	var id string
	err = tx.QueryRow("SELECT id FROM incidents WHERE class=? AND instance=? AND resolved IS NULL", class, scope).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec("UPDATE incidents SET resolved=? WHERE id=?", now.UTC().Format(eventTimeLayout), id); err != nil {
		return false, err
	}
	shared, err := notificationTable(tx, "backup_notification_transfers")
	if err != nil {
		return false, err
	}
	if shared {
		if err = cancelResolvedBackupBatches(tx, id); err != nil {
			return false, err
		}
		if err = cancelIncidentReminders(tx, id, "resolved/"+id); err != nil {
			return false, err
		}
	} else {
		if _, err = tx.Exec("DELETE FROM outbox WHERE kind='reminder' AND accepted IS NULL AND id LIKE ?", id+"/reminder/%"); err != nil {
			return false, err
		}
	}
	if policy.Recovery == nil || *policy.Recovery {
		if err = enqueueNotification(tx, notification.NotificationEvent{ID: id + "/recovery", Kind: "recovery", IncidentID: id, Message: fmt.Sprintf("Backup incident recovered [%s] %s. Incident %s", class, scope, id)}, now); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *backupState) enqueueDaily(c WorkerInstallation, now time.Time, summary string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = enqueueBackupDaily(tx, c, now, summary); err != nil {
		return err
	}
	return tx.Commit()
}

func enqueueBackupDaily(tx *sql.Tx, c WorkerInstallation, now time.Time, summary string) error {
	zone, err := time.LoadLocation(c.Policy.Timezone)
	if err != nil {
		return err
	}
	var started time.Time
	if e := backupEvaluationMeta(tx, "daily_started", &started); e == sql.ErrNoRows {
		started = now
		if e = storeBackupEvaluationMeta(tx, "daily_started", started); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	local := now.In(zone)
	if local.Format("15:04") < c.Policy.Notifications.DailyAt {
		local = local.AddDate(0, 0, -1)
	}
	startedLocal := started.In(zone)
	firstDay := time.Date(startedLocal.Year(), startedLocal.Month(), startedLocal.Day(), 0, 0, 0, 0, zone)
	if local.Before(firstDay) {
		return nil
	}
	event := notification.NotificationEvent{ID: "daily/" + c.WorkerID + "/" + local.Format("2006-01-02"), Kind: "daily", Message: "Current backup report; covers daily period " + local.Format("2006-01-02") + " (" + c.Policy.Timezone + ")\n" + summary}
	if err = enqueueNotification(tx, event, now); err != nil {
		return err
	}
	// Preserve historical attempts; covered periods remain explicitly unsent.
	body, _ := json.Marshal(event)
	if _, err = tx.Exec("UPDATE outbox SET record=json_set(record,'$.message',json_extract(?,'$.message')) WHERE id=? AND accepted IS NULL", string(body), event.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE outbox SET covered_by=? WHERE kind='daily' AND accepted IS NULL AND id LIKE ? AND id<? AND id<>?", event.ID, "daily/"+c.WorkerID+"/%", event.ID, event.ID); err != nil {
		return err
	}
	return nil
}

func drainNotificationOutbox(ctx context.Context, state *backupState, c WorkerInstallation, send notification.Sender, now time.Time) (err error) {
	if err = requireLegacyNotificationOwner(state); err != nil {
		return err
	}
	release, err := backupCommandDeliveryGuard(state)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	if err := state.reconcileRetiredIncidents(now); err != nil {
		return err
	}
	if err := coalesceNotificationBacklog(state, now); err != nil {
		return err
	}
	rows, err := state.db.Query("SELECT id,record,attempts FROM outbox WHERE accepted IS NULL AND covered_by IS NULL AND due<=? ORDER BY CASE WHEN kind='batch' THEN 0 WHEN kind IN ('daily','delivery-catchup') THEN 2 ELSE 1 END,created,id LIMIT 32", now.UTC().Format(eventTimeLayout))
	if err != nil {
		return err
	}
	type pending struct {
		id       string
		event    notification.NotificationEvent
		attempts int
	}
	var events []pending
	for rows.Next() {
		var p pending
		var raw string
		if err = rows.Scan(&p.id, &raw, &p.attempts); err != nil {
			rows.Close()
			return err
		}
		if err = protocol.StrictBackupJSON([]byte(raw), &p.event); err != nil {
			rows.Close()
			return err
		}
		events = append(events, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The bounded candidate list can outlive a cancellation transaction. Treat
		// this eligibility check as the start of an in-flight legacy send.
		var eligible int
		if err = state.db.QueryRow("SELECT count(*) FROM outbox WHERE id=? AND accepted IS NULL AND covered_by IS NULL", p.id).Scan(&eligible); err != nil {
			return err
		}
		if eligible == 0 {
			continue
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 11*time.Second {
			return nil
		}
		if p.event.Kind == "daily" || p.event.Kind == "delivery-catchup" {
			summary, _, e := evaluateWorkerHealth(c, state, time.Now().UTC())
			if e != nil {
				return e
			}
			prefix, _, _ := strings.Cut(p.event.Message, "\n")
			p.event.Message = prefix + "\n" + summary
		}
		providers := []string{c.Policy.Notifications.Primary}
		if c.Policy.Notifications.Fallback != "" {
			providers = append(providers, c.Policy.Notifications.Fallback)
		}
		accepted := false
		for _, provider := range providers {
			if err = requireLegacyNotificationOwner(state); err != nil {
				return err
			}
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 11*time.Second {
				break
			}
			attempt, deliveryErr := send(ctx, provider, c.Policy.Notifications, p.event)
			p.event.Attempts = append(p.event.Attempts, attempt)
			if len(p.event.Attempts) > 32 {
				p.event.Attempts = p.event.Attempts[len(p.event.Attempts)-32:]
			}
			raw, _ := json.Marshal(p.event)
			if _, err = state.db.Exec("UPDATE outbox SET record=? WHERE id=?", string(raw), p.id); err != nil {
				return err
			}
			if err = state.put("delivery/"+provider, attempt); err != nil {
				return err
			}
			if deliveryErr == nil && attempt.Accepted {
				accepted = true
				break
			}
		}
		raw, _ := json.Marshal(p.event)
		if accepted {
			if _, err = state.db.Exec("UPDATE outbox SET accepted=?,attempts=attempts+1,record=? WHERE id=?", time.Now().UTC().Format(eventTimeLayout), string(raw), p.id); err != nil {
				return err
			}

		} else {
			if err = markDeliveryOutage(state, now); err != nil {
				return err
			}
			retry := time.Minute * time.Duration(1<<min(p.attempts, 8))
			if retry > 6*time.Hour {
				retry = 6 * time.Hour
			}
			if _, err = state.db.Exec("UPDATE outbox SET due=?,attempts=attempts+1,record=? WHERE id=?", now.Add(retry).UTC().Format(eventTimeLayout), string(raw), p.id); err != nil {
				return err
			}
			if c.Policy.Monitoring.DailyDeliveryPingURLFile != "" {
				_ = notification.Heartbeat(ctx, c.Policy.Monitoring.DailyDeliveryPingURLFile, true)
			}
		}
	}
	return nil
}

func evaluateWorkerHealth(c WorkerInstallation, state *backupState, now time.Time) (string, bool, error) {
	if err := state.reconcileRetiredIncidents(now); err != nil {
		return "Retirement incident reconciliation unavailable", false, err
	}
	var control ArchiveControl
	if err := state.get("control", &control); err != nil {
		return "Worker control state unavailable", false, err
	}
	var run WorkerRun
	_ = state.get("run", &run)
	problems := []string{}
	var paused time.Time
	if err := state.get("paused_until", &paused); err != nil && err != sql.ErrNoRows {
		return "", false, err
	}
	if !paused.IsZero() {
		problems = append(problems, "backups paused until "+paused.Format(time.RFC3339)+"; resume/expiry awaits host admission")
	}
	var pending backupCatchup
	if err := state.get("backup_catchup", &pending); err != nil && err != sql.ErrNoRows {
		return "", false, err
	}
	if pending.ID != "" {
		problems = append(problems, "catch-up backup pending")
	}
	var mode string
	if state.get("mode", &mode) != nil || mode != "writer" {
		problems = append(problems, "worker is in maintenance or read mode")
	}
	active, fresh := 0, 0
	for key, instance := range control.Instances {
		if instance.Retired || control.RetiredSources[instance.SourceID] != "" {
			continue
		}
		active++
		problem, err := state.latestVerificationProblem(instance)
		if err != nil {
			return "", false, err
		}
		if problem != "" {
			problems = append(problems, instance.Alias+"/"+instance.Service+": "+problem)
			if err := state.observeIncident("verification", key, problem, now, c.Policy.Notifications); err != nil {
				return "", false, err
			}
		} else {
			if err := state.resolveIncident("verification", key, now, c.Policy.Notifications); err != nil {
				return "", false, err
			}
		}
		if instance.LastSuccess.IsZero() || now.Before(instance.LastSuccess) || now.Sub(instance.LastSuccess) > config.Budget(c.Policy.Freshness, "2h") {
			problems = append(problems, "stale snapshot: "+instance.Alias+"/"+instance.Service)
			if err := state.observeIncident("freshness", key, "snapshot freshness threshold exceeded", now, c.Policy.Notifications); err != nil {
				return "", false, err
			}
		} else {
			if problem == "" {
				fresh++
			}
			if err := state.resolveIncident("freshness", key, now, c.Policy.Notifications); err != nil {
				return "", false, err
			}
		}
	}
	if active == 0 {
		problems = append(problems, "no enrolled active instances")
	}
	if !run.Complete && (!run.FinishedAt.IsZero() || now.Sub(run.StartedAt) > config.Budget(c.Policy.MaxRunTime, "40m")) {
		problems = append(problems, "latest run incomplete")
	}
	var probe WorkerHealthProbe
	if err := state.get("health_probe", &probe); err != nil || !probe.Fresh(now) || !probe.BackupHealthy || probe.ConfigHash != c.ConfigHash {
		problems = append(problems, "independent host/timer/storage evidence unavailable or unhealthy")
	}
	for _, gate := range []string{"source_restrictions", "rootless_runtime", "storage_durability", "notification_delivery", "external_monitor"} {
		if !c.Qualification[gate] {
			problems = append(problems, "qualification pending: "+gate)
		}
	}
	summary := fmt.Sprintf("Snapshots within freshness policy: %d/%d. Latest completed captures: %d. Stored checks are SQLite snapshot validation; application recovery remains separately unverified.", fresh, active, run.Captured)
	if len(problems) > 0 {
		summary += "\nAttention: " + strings.Join(problems, "; ")
	} else {
		summary += "\nSnapshot freshness and worker checks passed."
	}
	var incidents int
	if err := state.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL").Scan(&incidents); err != nil {
		return summary, false, err
	}
	summary += fmt.Sprintf("\nOpen incidents: %d; application checks and delivery have separate outcomes.", incidents)
	return summary, len(problems) == 0, nil
}

func runNotifier(ctx context.Context, c WorkerInstallation, state *backupState) error {
	if err := notification.ValidateMonitoringChecks(c.Policy.Monitoring); err != nil {
		return err
	}
	dir, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	lock, err := dir.Lock("notifier.lock", true, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = requireLegacyNotificationOwner(state); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	now := time.Now().UTC()
	summary, healthy, err := evaluateWorkerHealth(c, state, now)
	if err != nil {
		return err
	}
	if err = state.enqueueDaily(c, now, summary); err != nil {
		return err
	}
	if err = enqueueDeliveryCatchup(state, now, summary); err != nil {
		return err
	}
	if err = drainNotificationOutbox(ctx, state, c, notification.Send, now); err != nil {
		return err
	}
	// Serialize pause/resume with the bounded coverage heartbeat.
	schedule, err := schedulerLockContext(ctx, state)
	if err != nil {
		return err
	}
	defer schedule.Close()
	// Delivery can consume most of a pass. Re-evaluate before sending coverage.
	_, healthy, err = evaluateWorkerHealth(c, state, time.Now().UTC())
	if err != nil {
		return err
	}
	if c.Policy.Monitoring.BackupPingURLFile != "" {
		var run WorkerRun
		_ = state.get("run", &run)
		var last string
		_ = state.get("backup_ping_run", &last)
		if !healthy {
			_ = notification.Heartbeat(ctx, c.Policy.Monitoring.BackupPingURLFile, true)
		} else if run.Complete && run.ID != last {
			if err = notification.Heartbeat(ctx, c.Policy.Monitoring.BackupPingURLFile, false); err == nil {
				err = state.put("backup_ping_run", run.ID)
			}
			if err != nil {
				return err
			}
		}
	}
	if err = publishDailyDeliveryHeartbeat(ctx, state, c, now); err != nil {
		return err
	}
	return state.put("notifier_last_pass", now)
}

func (s *backupState) wakeNotifier() error {
	dir, err := safefs.Open(s.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := dir.Open("urgent", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write([]byte(time.Now().UTC().Format(eventTimeLayout)))
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return dir.File().Sync()
}
