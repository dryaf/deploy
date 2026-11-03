package backup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/safefs"
)

func retirementReason(control ArchiveControl, scope string) string {
	if reason := control.RetiredSources[scope]; reason != "" {
		return reason
	}
	for key, instance := range control.Instances {
		if scope != key && !strings.HasPrefix(scope, "maintenance/full/"+key+"/") && !strings.HasPrefix(scope, "maintenance/application/"+key+"/") {
			continue
		}
		if reason := control.RetiredSources[instance.SourceID]; reason != "" {
			return reason
		}
		if instance.Retired {
			return instance.RetirementReason
		}
	}
	return ""
}

func incidentControl(tx *sql.Tx) (ArchiveControl, error) {
	var control ArchiveControl
	var raw string
	err := tx.QueryRow("SELECT value FROM meta WHERE key='control'").Scan(&raw)
	if err == sql.ErrNoRows {
		return control, nil
	}
	if err == nil {
		err = safefs.DecodeJSON([]byte(raw), &control, safefs.MaxRecordBytes)
	}
	return control, err
}

// The archive-backed retirement is the durable intent. Retrying this local
// transaction (also before notification delivery) repairs a crash after that
// intent without claiming that a successful backup resolved the incident.
func (s *backupState) reconcileRetiredIncidents(now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed, err := reconcileRetiredBackupIncidents(tx, now)
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

func reconcileRetiredBackupIncidents(tx *sql.Tx, now time.Time) (bool, error) {
	control, err := incidentControl(tx)
	if err != nil {
		return false, err
	}
	rows, err := tx.Query("SELECT id,instance FROM incidents WHERE resolved IS NULL")
	if err != nil {
		return false, err
	}
	type retired struct{ id, scope, reason string }
	var incidents []retired
	for rows.Next() {
		var id, scope string
		if err = rows.Scan(&id, &scope); err != nil {
			rows.Close()
			return false, err
		}
		if reason := retirementReason(control, scope); reason != "" {
			incidents = append(incidents, retired{id, scope, reason})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, incident := range incidents {
		event := notification.NotificationEvent{ID: incident.id + "/retired", Kind: "retirement", IncidentID: incident.id, Message: fmt.Sprintf("Backup protection retired by operator for %s: %s. Incident %s closed by retirement; no successful backup recovery is claimed.", incident.scope, incident.reason, incident.id)}
		if _, err = tx.Exec("UPDATE incidents SET resolved=? WHERE id=?", now.UTC().Format(eventTimeLayout), incident.id); err != nil {
			return false, err
		}
		disposition, _ := json.Marshal(map[string]any{"disposition": "retired-by-operator", "reason": incident.reason, "at": now.UTC()})
		if _, err = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", "incident_resolution/"+incident.id, string(disposition)); err != nil {
			return false, err
		}
		if err = enqueueNotification(tx, event, now); err != nil {
			return false, err
		}
		if err = sharedIncidentCancellation(tx, incident.id, "retired by operator", true); err != nil {
			return false, err
		}
		// Invalidate unsent batches containing these alerts. Requeue their other
		// members so unrelated failures are not lost along with obsolete text.
		rows, err := tx.Query(`SELECT DISTINCT covered_by FROM outbox WHERE accepted IS NULL AND kind IN ('failure','reminder') AND json_extract(record,'$.incident_id')=? AND covered_by LIKE 'batch/%'`, incident.id)
		if err != nil {
			return false, err
		}
		var batches []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
			batches = append(batches, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		if _, err = tx.Exec(`UPDATE outbox SET covered_by=? WHERE accepted IS NULL AND kind IN ('failure','reminder') AND json_extract(record,'$.incident_id')=?`, event.ID, incident.id); err != nil {
			return false, err
		}
		for _, batch := range batches {
			result, err := tx.Exec("UPDATE outbox SET covered_by=? WHERE id=? AND accepted IS NULL AND covered_by IS NULL", event.ID, batch)
			if err != nil {
				return false, err
			}
			if changed, err := result.RowsAffected(); err != nil {
				return false, err
			} else if changed != 0 {
				if _, err = tx.Exec("UPDATE outbox SET covered_by=NULL WHERE covered_by=? AND accepted IS NULL", batch); err != nil {
					return false, err
				}
			}
		}
	}
	return len(incidents) != 0, nil
}
