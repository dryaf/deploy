package backup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/dryaf/deploy/internal/backup/notification"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

func notificationTable(tx *sql.Tx, name string) (bool, error) {
	var exists int
	err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&exists)
	return exists == 1, err
}

// Freeze at source-event creation, not later when evidence may have changed.
// The additive table leaves the legacy outbox wire format unchanged. Old events
// without retained context cannot acquire mutation authority retrospectively.
func freezeBackupEventContext(tx *sql.Tx, event notification.NotificationEvent) error {
	installed, err := notificationTable(tx, "backup_command_event_context")
	if err != nil || !installed {
		return err
	}
	var prior int
	if err = tx.QueryRow("SELECT count(*) FROM outbox WHERE id=?", event.ID).Scan(&prior); err != nil {
		return err
	}
	if prior != 0 {
		return nil
	}
	var targets []notify.Target
	if event.IncidentID != "" {
		var subject, class, opened, message string
		if err = tx.QueryRow("SELECT instance,class,opened,message FROM incidents WHERE id=?", event.IncidentID).Scan(&subject, &class, &opened, &message); err != nil {
			return err
		}
		evidence, err := syncBackupCommandEvidence(tx, event.IncidentID, class, opened, message)
		if err != nil {
			return err
		}
		decision := uint64(1)
		err = tx.QueryRow("SELECT revision FROM backup_command_suppressions WHERE incident=?", event.IncidentID).Scan(&decision)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		targets = []notify.Target{{Subject: subject, Kind: "incident", ID: event.IncidentID, Occurrence: event.IncidentID, EvidenceRevision: evidence, DecisionRevision: decision}}
	} else if event.Kind == "batch" {
		byOccurrence := map[string]notify.Target{}
		for _, member := range event.Members {
			c, err := backupEventContext(tx, member)
			if err != nil {
				return err
			}
			if c == nil {
				return nil
			}
			for _, t := range c.Targets {
				key := notify.Hash([]string{t.Subject, t.ID, t.Occurrence})
				old, ok := byOccurrence[key]
				if !ok || t.EvidenceRevision > old.EvidenceRevision || (t.EvidenceRevision == old.EvidenceRevision && t.DecisionRevision > old.DecisionRevision) {
					byOccurrence[key] = t
				}
			}
		}
		var keys []string
		for key := range byOccurrence {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			targets = append(targets, byOccurrence[key])
		}
	}
	if len(targets) == 0 {
		return nil
	}
	kind := "single"
	if event.Kind == "batch" {
		kind = "summary"
	}
	c := notify.InteractionContext{Version: 1, ID: "backup-event/" + notify.Hash(event.ID), Revision: 1, Kind: kind, Targets: targets}
	if err = c.Validate(); err != nil {
		return fmt.Errorf("invalid frozen backup context: %w", err)
	}
	body, _ := json.Marshal(c)
	_, err = tx.Exec("INSERT INTO backup_command_event_context VALUES(?,?)", event.ID, string(body))
	return err
}

func backupEventContext(tx *sql.Tx, id string) (*notify.InteractionContext, error) {
	var body string
	err := tx.QueryRow("SELECT body FROM backup_command_event_context WHERE event=?", id).Scan(&body)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c notify.InteractionContext
	if err = notify.Decode([]byte(body), &c, notify.MaxRequest); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}
