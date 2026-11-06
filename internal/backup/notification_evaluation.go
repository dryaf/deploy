package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
)

type backupEvaluationState struct {
	Sequence uint64
	Hash     string
	Clock    backupEvaluationClock
}

func storeBackupEvaluationMeta(tx *sql.Tx, key string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(body))
	return err
}

// Host acquisition is outside the transaction. The service must hold its notifier
// lease, refresh the independent host probe, and schedule this pass regularly.
// Production service selection remains gated by the ownership migration.
func evaluateAndFreezeBackupNotifications(ctx context.Context, state *backupState, c WorkerInstallation, now time.Time) (evaluation backupNotificationEvaluation, messages []notify.Message, err error) {
	boot, uptime, err := process.HostClock()
	if err != nil {
		boot = ""
		uptime = 0
	}
	err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		var err error
		evaluation, messages, err = evaluateBackupNotificationPass(tx, c, backupEvaluationClock{now.UTC(), boot, uptime})
		return err
	})
	return
}

func backupConditionRank(condition string) int {
	switch condition {
	case "critical":
		return 4
	case "warning":
		return 3
	case "paused":
		return 2
	case "unknown":
		return 1
	}
	return 0
}
func worsenBackupSubject(s *notify.Subject, condition, reason string) {
	if backupConditionRank(condition) > backupConditionRank(s.Condition) {
		s.Condition = condition
		s.Reason = reason
	}
}

// All domain reads, incident transitions, semantic revisions and outgoing
// envelopes share the caller's transaction. Any error requires its rollback.
func evaluateBackupNotificationPass(tx *sql.Tx, c WorkerInstallation, clock backupEvaluationClock) (e backupNotificationEvaluation, messages []notify.Message, err error) {
	if err = requireNotificationRecoveryInactive(tx); err != nil {
		return
	}
	if e, err = evaluateBackupNotificationState(tx, c, clock); err != nil {
		return
	}
	messages, err = freezeBackupNotificationPass(tx, e)
	return
}

// Records current domain evidence and its sequence in the caller's transaction.
// Recovery uses this without assigning new delivery sequences or sending status.
func evaluateBackupNotificationState(tx *sql.Tx, c WorkerInstallation, clock backupEvaluationClock) (e backupNotificationEvaluation, err error) {
	b, err := readBackupNotificationBinding(tx)
	if err != nil {
		return
	}
	control, err := incidentControl(tx)
	if err != nil {
		return
	}
	if c.WorkerID != b.Producer.ID || c.ArchiveID != b.Archive || c.ConfigHash != control.ConfigHash || clock.Now.IsZero() {
		return e, fmt.Errorf("backup evaluator authority/configuration mismatch")
	}
	fresh, runLimit, err := backupEvaluationDurations(c.Policy.Freshness, c.Policy.MaxRunTime, c.Policy.Notifications.RepeatAfter)
	if err != nil {
		return
	}
	if len(control.Instances)+1 > notify.MaxSubjects {
		return e, fmt.Errorf("backup subjects exceed snapshot capacity")
	}
	var sequence, basis uint64
	if err = tx.QueryRow("SELECT COALESCE(max(sequence),0),COALESCE(max(basis),0) FROM backup_notification_snapshots").Scan(&sequence, &basis); err != nil {
		return
	}
	var previous backupEvaluationState
	if err = backupEvaluationMeta(tx, "notification_evaluation", &previous); err != nil && err != sql.ErrNoRows {
		return
	}
	if err == nil && previous.Sequence != sequence {
		return e, fmt.Errorf("backup evaluator state requires reconciliation")
	}
	var suspect bool
	if err = backupEvaluationMeta(tx, "clock_suspect", &suspect); err != nil && err != sql.ErrNoRows {
		return
	}
	if previous.Clock.valid() && clock.valid() && previous.Clock.Boot == clock.Boot && (clock.Uptime < previous.Clock.Uptime || clock.Now.Before(previous.Clock.Now) || absFloat(clock.Now.Sub(previous.Clock.Now).Seconds()-(clock.Uptime-previous.Clock.Uptime)) >= 30) {
		suspect = true
		if err = storeBackupEvaluationMeta(tx, "clock_suspect", true); err != nil {
			return
		}
	}
	if _, err = reconcileRetiredBackupIncidents(tx, clock.Now); err != nil {
		return
	}
	now := clock.Now
	until := now.Add(120 * time.Second)
	global := notify.Subject{Key: "backup", Condition: "ok", Reason: "Snapshot freshness and worker checks passed; application recovery and delivery are separate checks.", ValidUntil: until}
	var probe WorkerHealthProbe
	if err = backupEvaluationMeta(tx, "health_probe", &probe); err != nil && err != sql.ErrNoRows {
		return
	}
	probeDeadline, probeFresh := backupProbeDeadline(probe, clock, c.ConfigHash)
	if !probeFresh || !probe.BackupHealthy {
		worsenBackupSubject(&global, "unknown", "Independent host, timer and storage evidence is unavailable or unhealthy.")
	}
	if probeFresh && probeDeadline.Before(global.ValidUntil) {
		global.ValidUntil = probeDeadline
	}
	if !clock.valid() || suspect {
		worsenBackupSubject(&global, "unknown", "Host clock evidence is uncertain; reconciliation is required.")
	}
	var mode string
	if err = backupEvaluationMeta(tx, "mode", &mode); err != nil && err != sql.ErrNoRows {
		return
	}
	if mode != "writer" {
		worsenBackupSubject(&global, "unknown", "Worker is in maintenance or read mode.")
	}
	var paused time.Time
	if err = backupEvaluationMeta(tx, "paused_until", &paused); err != nil && err != sql.ErrNoRows {
		return
	}
	if !paused.IsZero() {
		worsenBackupSubject(&global, "paused", "Backup scheduling is paused; resume or expiry awaits host admission.")
	}
	var catchup backupCatchup
	if err = backupEvaluationMeta(tx, "backup_catchup", &catchup); err != nil && err != sql.ErrNoRows {
		return
	}
	if catchup.ID != "" {
		worsenBackupSubject(&global, "warning", "A catch-up backup is pending.")
	}
	var run WorkerRun
	if err = backupEvaluationMeta(tx, "run", &run); err != nil && err != sql.ErrNoRows {
		return
	}
	if run.ID == "" || run.ConfigHash != c.ConfigHash || run.StartedAt.IsZero() || run.StartedAt.After(now) || run.FinishedAt.After(now) {
		worsenBackupSubject(&global, "unknown", "Current backup run evidence is unavailable or inconsistent.")
	} else if len(run.Failures) > 0 || !run.Complete && (!run.FinishedAt.IsZero() || !now.Before(run.StartedAt.Add(runLimit))) {
		worsenBackupSubject(&global, "critical", "Latest backup run is incomplete or failed.")
	} else if !run.Complete && run.StartedAt.Add(runLimit).Before(global.ValidUntil) {
		global.ValidUntil = run.StartedAt.Add(runLimit)
	}
	qualification := c.Qualification
	var proof WorkerQualification
	if err = backupEvaluationMeta(tx, "qualification", &proof); err != nil && err != sql.ErrNoRows {
		return
	}
	if err == nil && proof.Schema == 1 && proof.WorkerID == c.WorkerID && proof.ArchiveID == c.ArchiveID && proof.ConfigHash == c.ConfigHash {
		qualification = copyQualification(qualification)
		qualification["storage_durability"] = proof.Checks["storage_crash_recovery"].Passed
		qualification["notification_delivery"] = proof.Checks["notification_receipt"].Passed
		qualification["external_monitor"] = proof.Checks["missed_worker_route"].Passed && proof.Checks["daily_delivery_route"].Passed
	}
	for _, gate := range []string{"source_restrictions", "rootless_runtime", "storage_durability", "notification_delivery", "external_monitor"} {
		if !qualification[gate] {
			worsenBackupSubject(&global, "unknown", "Required backup worker qualification is incomplete.")
		}
	}
	subjects := []notify.Subject{}
	keys := make([]string, 0, len(control.Instances))
	for key := range control.Instances {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	active, healthy := 0, 0
	facts := []any{control, c.Policy, run, qualification, probe.BootID, probe.BackupHealthy, mode, paused, catchup, suspect, clock.Boot}
	for _, key := range keys {
		instance := control.Instances[key]
		if key == "backup" || !notify.ValidToken(key) {
			return e, fmt.Errorf("invalid backup subject identity")
		}
		sub := global
		sub.Key = key
		if instance.Retired || control.RetiredSources[instance.SourceID] != "" {
			if retirementReason(control, key) == "" {
				return e, fmt.Errorf("retirement requires its operator reason")
			}
			sub.Condition = "retired"
			sub.Reason = "Backup protection retired by operator; no recovery is claimed."
			subjects = append(subjects, sub)
			continue
		}
		active++
		receipt, problem, readErr := backupSnapshotEvidence(tx, control, instance)
		if readErr != nil {
			return e, readErr
		}
		facts = append(facts, receipt.BackupID, receipt.Header.SHA256, problem)
		var verification BackupVerificationStatus
		if receipt.BackupID != "" {
			if err = backupEvaluationMeta(tx, "verification/"+receipt.BackupID, &verification); err != nil && err != sql.ErrNoRows {
				return
			}
			facts = append(facts, verification)
		}
		if problem != "" {
			worsenBackupSubject(&sub, "critical", problem)
			if _, err = observeBackupIncident(tx, "verification", key, problem, now, c.Policy.Notifications); err != nil {
				return
			}
		} else if probeFresh && probe.BackupHealthy && clock.valid() && !suspect {
			if _, err = resolveBackupIncident(tx, "verification", key, now, c.Policy.Notifications); err != nil {
				return
			}
		}
		stale := instance.LastSuccess.IsZero() || instance.LastSuccess.After(now) || !now.Before(instance.LastSuccess.Add(fresh))
		// LastSuccess must refer to the selected capture, not unrelated newer metadata.
		if !instance.LastSuccess.IsZero() && !receipt.Header.Attempt.RequestedAt.IsZero() && !instance.LastSuccess.Equal(receipt.Header.Attempt.RequestedAt) {
			stale = true
		}
		if clock.valid() && !suspect {
			if stale {
				worsenBackupSubject(&sub, "critical", "Snapshot freshness threshold exceeded or capture identity is inconsistent.")
				if _, err = observeBackupIncident(tx, "freshness", key, "snapshot freshness threshold exceeded or capture identity is inconsistent", now, c.Policy.Notifications); err != nil {
					return
				}
			} else {
				if instance.LastSuccess.Add(fresh).Before(sub.ValidUntil) {
					sub.ValidUntil = instance.LastSuccess.Add(fresh)
				}
				if probeFresh && probe.BackupHealthy {
					if _, err = resolveBackupIncident(tx, "freshness", key, now, c.Policy.Notifications); err != nil {
						return
					}
				}
			}
		}
		subjects = append(subjects, sub)
	}
	// Never mask another unresolved failure class merely because snapshots are fresh.
	rows, err := tx.Query("SELECT id,instance,class,message FROM incidents WHERE resolved IS NULL ORDER BY id LIMIT 4097")
	if err != nil {
		return
	}
	open := 0
	for rows.Next() {
		var id, scope, class, message string
		if err = rows.Scan(&id, &scope, &class, &message); err != nil {
			rows.Close()
			return
		}
		open++
		if open > 4096 {
			rows.Close()
			return e, fmt.Errorf("backup incident evaluation exceeds bounded capacity")
		}
		facts = append(facts, []string{id, scope, class, message})
		worsenBackupSubject(&global, "critical", "Unresolved backup incidents require attention.")
		for i := range subjects {
			if subjects[i].Key == scope && subjects[i].Condition != "retired" {
				worsenBackupSubject(&subjects[i], "critical", "Unresolved backup incidents require attention.")
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	if active == 0 {
		worsenBackupSubject(&global, "unknown", "No active backup instances are enrolled.")
	}
	for _, sub := range subjects {
		if sub.Condition == "retired" {
			continue
		}
		if sub.Condition == "ok" {
			healthy++
		}
		worsenBackupSubject(&global, sub.Condition, "One or more backup subjects require attention.")
		if sub.ValidUntil.Before(global.ValidUntil) {
			global.ValidUntil = sub.ValidUntil
		}
	}

	// Explicit retirement of every configured source also retires the aggregate
	// coverage subject. Empty discovery or retiring only known instances cannot
	// make this assertion: a still-enrolled source may discover new instances.
	allSourcesRetired := len(c.Sources) > 0 && active == 0 && open == 0
	for _, source := range c.Sources {
		if source.SourceID == "" || control.RetiredSources[source.SourceID] == "" {
			allSourcesRetired = false
		}
	}
	if allSourcesRetired {
		global.Condition = "retired"
		global.Reason = "Every configured backup source was explicitly retired by operator; no recovery is claimed."
	}
	subjects = append([]notify.Subject{global}, subjects...)
	for _, sub := range subjects {
		facts = append(facts, []string{sub.Key, sub.Condition, sub.Reason})
	}
	hash := notify.Hash(facts)
	if hash != previous.Hash {
		basis++
	}
	sequence++
	if !notify.Sequence(sequence) || !notify.Sequence(basis) {
		return e, fmt.Errorf("backup evaluation revision exhausted")
	}
	e.Snapshot = notify.Snapshot{Producer: b.Producer, Sequence: sequence, BasisRevision: basis, ConfigRevision: c.ConfigHash, EvaluatedAt: now, Subjects: subjects}
	e.ReportText = notification.NotificationEnvelope(fmt.Sprintf("Current backup evaluation: %s. %s\nSnapshots within freshness and worker policy: %d/%d. Open incidents: %d. SQLite snapshot checks do not establish application recovery; delivery is evaluated separately.", global.Condition, global.Reason, healthy, active, open))
	if err = storeBackupNotificationSnapshot(tx, b, e); err != nil {
		return
	}
	err = storeBackupEvaluationMeta(tx, "notification_evaluation", backupEvaluationState{sequence, hash, clock})
	return
}
