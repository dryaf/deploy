package backup

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/dryaf/deploy/internal/config"
)

func nextBackupOccurrence(policy config.BackupWorkerConfig, now time.Time) (time.Time, error) {
	zone, err := time.LoadLocation(policy.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	if policy.Schedule == "hourly" {
		return now.UTC().Truncate(time.Hour).Add(time.Hour), nil
	}
	local := now.In(zone)
	next := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone).AddDate(0, 0, 1)
	switch policy.Schedule {
	case "daily":
		return next, nil
	case "weekly":
		for next.Weekday() != time.Monday {
			next = next.AddDate(0, 0, 1)
		}
		return next, nil
	}
	return time.Time{}, fmt.Errorf("unsupported schedule")
}

func maintenanceAdmission(c WorkerInstallation, state *backupState, required time.Duration, now time.Time) error {
	var pending backupCatchup
	if state.get("backup_catchup", &pending) == nil && pending.ID != "" {
		return fmt.Errorf("maintenance deferred for pending backup catch-up")
	}
	var probe WorkerHealthProbe
	if state.get("health_probe", &probe) != nil || !probe.Fresh(now) || probe.ConfigHash != c.ConfigHash {
		return fmt.Errorf("maintenance needs fresh host evidence")
	}
	var paused time.Time
	_ = state.get("paused_until", &paused)
	if paused.After(now.Add(required + time.Minute)) {
		return nil
	}
	next, err := nextBackupOccurrence(c.Policy, now)
	if err != nil {
		return err
	}
	if next.Sub(now) < required+time.Minute {
		return fmt.Errorf("maintenance deferred to protect the next snapshot deadline")
	}
	return nil
}

type maintenanceAttempt struct {
	At       time.Time `json:"at"`
	Passed   bool      `json:"passed"`
	BackupID string    `json:"backup_id"`
}

func runScheduledMaintenance(ctx context.Context, c WorkerInstallation, state *backupState, a *backupArchive) error {
	writer, err := a.dir.Lock(".locks/writer.lock", true, false)
	if err != nil {
		return state.put("maintenance_deferred", "archive writer busy")
	}
	defer writer.Close()
	if err = a.commitControl(state); err != nil {
		return err
	}
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	keys := make([]string, 0, len(control.Instances))
	for key := range control.Instances {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	now := time.Now().UTC()
	for _, key := range keys {
		instance := control.Instances[key]
		if instance.Retired || control.RetiredSources[instance.SourceID] != "" {
			continue
		}
		for _, kind := range []string{"full", "application"} {
			token := "maintenance/" + kind + "/" + key + "/" + instance.Generation
			var previous maintenanceAttempt
			_ = state.get(token, &previous)
			interval := 7 * 24 * time.Hour
			if !previous.Passed {
				interval = 6 * time.Hour
			}
			if !previous.At.IsZero() && now.Sub(previous.At) < interval {
				continue
			}
			var check *config.WorkerRestoreCheck
			required := 25 * time.Minute
			if kind == "application" {
				for _, candidate := range c.Policy.RestoreChecks {
					if (candidate.Source == instance.Alias || candidate.Source == instance.SourceID) && candidate.Instance == instance.Service {
						v := candidate
						check = &v
					}
				}
				if check == nil {
					continue
				}
				token += "/" + restoreCheckContract(*check)
				previous = maintenanceAttempt{}
				_ = state.get(token, &previous)
				interval = 7 * 24 * time.Hour
				if !previous.Passed {
					interval = 6 * time.Hour
				}
				if !previous.At.IsZero() && now.Sub(previous.At) < interval {
					continue
				}
				required = 25 * time.Minute
			}
			if err = maintenanceAdmission(c, state, required, now); err != nil {
				return state.put("maintenance_deferred", err.Error())
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			receipt, err := a.selectReceipt("", true, ArchiveFilter{Source: instance.SourceID, Instance: instance.Service})
			if err != nil {
				return err
			}
			if kind == "full" {
				_, err = a.verify(state, receipt, true)
			} else {
				_, err = a.restoreCheck(state, receipt, *check)
			}
			record := maintenanceAttempt{now, err == nil, receipt.BackupID}
			if writeErr := state.put(token, record); writeErr != nil {
				return writeErr
			}
			if err != nil {
				_ = state.observeIncident("maintenance-check", token, "scheduled "+kind+" verification failed or was inconclusive", now, c.Policy.Notifications)
				return err
			}
			return state.resolveIncident("maintenance-check", token, now, c.Policy.Notifications)
		}
	}
	if c.Policy.Retention != nil {
		if err = state.retentionClockSafe(); err != nil {
			return state.put("maintenance_deferred", err.Error())
		}
		if err = maintenanceAdmission(c, state, 25*time.Minute, now); err != nil {
			return state.put("maintenance_deferred", err.Error())
		}
		err = runAutomaticRetention(ctx, c, state, a, now)
		if err != nil {
			_ = state.observeIncident("retention", "worker", "retention stopped; inspect archive/control/verification evidence", now, c.Policy.Notifications)
		} else {
			err = state.resolveIncident("retention", "worker", now, c.Policy.Notifications)
		}
	}
	return err
}
