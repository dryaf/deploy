package backup

import (
	"database/sql"
	"fmt"
	"github.com/dryaf/deploy/internal/backup/notification"
	"os"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const remigrationFile = "notification-remigration.json"
const remigrationKey = "notification_remigration"
const remigrationSourcePrefix = "notification_remigration_source/"

type backupRemigrationSpec struct {
	Version      int               `json:"version"`
	Cutover      backupCutoverSpec `json:"cutover"`
	RollbackHash string            `json:"rollback_hash"`
}
type backupRemigration struct {
	Spec     backupRemigrationSpec      `json:"spec"`
	Rollback backupLegacyRollbackIntent `json:"rollback"`
	Evidence backupCutoverEvidence      `json:"evidence"`
	Before   backupNotificationOwner    `json:"before"`
	After    backupNotificationOwner    `json:"after"`
}

func (r backupRemigration) validate() error {
	s := r.Spec
	if s.Version != 1 || !validCutoverVersion(s.Cutover) || s.Cutover.Version == 2 || s.Cutover.Managed.Validate() != nil || s.Cutover.Managed.Revoke || s.Cutover.Managed.LegacyGeneration != "" ||
		!safefs.DigestPattern.MatchString(s.Cutover.ConfigHash) || !safefs.DigestPattern.MatchString(s.Cutover.HelperHash) || s.RollbackHash != notify.Hash(r.Rollback) ||
		s.Cutover.EvidenceHash != notify.Hash(r.Evidence) || r.Before.Mode != "fenced" || r.Before.LegacyHash != s.RollbackHash || r.Before.LegacyGeneration != r.Rollback.Spec.Generation ||
		r.Before.Binding != r.Rollback.Owner.Binding || r.Before.CutoverHash != r.Rollback.Owner.CutoverHash || r.Before.Socket != r.Rollback.Owner.Socket ||
		s.Cutover.Managed.Generation == r.Before.Binding.Generation || s.Cutover.Managed.Generation == r.Before.LegacyGeneration || len(s.Cutover.Decisions) != len(r.Evidence.Rows) {
		return fmt.Errorf("invalid backup remigration lineage")
	}
	want := r.Before
	if want.Schema != 3 {
		want.Schema = 2
	} // Older helpers refuse new ownership schemas.
	want.Mode = "shared"
	want.Binding.Generation = s.Cutover.Managed.Generation
	want.LegacyGeneration, want.LegacyState, want.LegacyHash = "", "", ""
	if want != r.After || s.Cutover.Managed.Producer != want.Binding.Producer || s.Cutover.Managed.Route != want.Binding.Route {
		return fmt.Errorf("remigration changed producer authority")
	}
	for _, row := range r.Evidence.Rows {
		d, ok := s.Cutover.Decisions[row.ID]
		if !ok || d.Hash != notify.Hash(row) || d.Reason == "" || len(d.Reason) > 512 || (d.Disposition != "accepted" && d.Disposition != "unsent" && d.Disposition != "held") || (d.Disposition == "accepted") != row.Accepted.Valid {
			return fmt.Errorf("invalid remigration source disposition")
		}
	}
	return nil
}

func readBackupRemigration(tx *sql.Tx) (*backupRemigration, error) {
	var r backupRemigration
	err := backupNotificationMeta(tx, remigrationKey, &r)
	if err == sql.ErrNoRows {
		var orphan bool
		if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key LIKE 'notification_remigration_source/%' OR key='notification_remigration_complete')").Scan(&orphan); err != nil {
			return nil, err
		}
		if orphan {
			return nil, fmt.Errorf("remigration authority lost its intent")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = r.validate(); err != nil {
		return nil, err
	}
	var rollback, revoked string
	if err = backupNotificationMeta(tx, "notification_legacy_rollback", &rollback); err != nil {
		return nil, err
	}
	if err = backupNotificationMeta(tx, "notification_legacy_rollback_revoked", &revoked); err != nil {
		return nil, err
	}
	if rollback != r.Spec.RollbackHash || revoked != rollback {
		return nil, fmt.Errorf("remigration lost legacy revocation")
	}
	for source, want := range r.Spec.Cutover.Decisions {
		var got backupCutoverDecision
		if err = backupNotificationMeta(tx, remigrationSourcePrefix+source, &got); err != nil || got != want {
			return nil, fmt.Errorf("remigration source evidence lost or changed")
		}
	}
	return &r, nil
}

// This is historical source evidence. No shared receipt, success heartbeat or
// new acceptance time is manufactured by importing a legacy outcome.
func installRemigrationSources(tx *sql.Tx, r backupRemigration) error {
	for _, row := range r.Evidence.Rows {
		d := r.Spec.Cutover.Decisions[row.ID]
		if err := storeBackupEvaluationMeta(tx, remigrationSourcePrefix+row.ID, d); err != nil {
			return err
		}
		if d.Disposition == "accepted" {
			sources := []string{row.ID}
			var event notification.NotificationEvent
			if err := notify.Decode([]byte(row.Record), &event, notify.MaxRequest); err != nil {
				return err
			}
			if event.Kind == "batch" {
				for _, member := range event.Members {
					found := false
					for _, evidence := range r.Evidence.Rows {
						if evidence.ID == member && evidence.CoveredBy.Valid && evidence.CoveredBy.String == row.ID {
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("accepted legacy batch lost exact member coverage")
					}
					sources = append(sources, member)
				}
			}
			for _, source := range sources {
				if _, err := tx.Exec("UPDATE notification_obligations SET disposition='legacy_accepted',covered_by=?,reason=? WHERE disposition='open' AND id IN (SELECT obligation FROM backup_notification_members WHERE source=?)", "legacy/"+row.ID, "retained legacy acceptance: "+notify.Hash(r), source); err != nil {
					return err
				}
			}
		}
	}
	// Every pre-rollback export was proved accepted or unsent by that completed
	// rollback. Retain its bytes and monotonic sequence; a new pass allocates a
	// successor only for still-uncovered, explicitly reviewed source work.
	_, err := tx.Exec("UPDATE notification_exports SET state='cancelled' WHERE accepted=0 AND json_extract(body,'$.generation')=?", r.Before.Binding.Generation)
	return err
}

func remigrationPriorBinding(tx *sql.Tx, b backupNotificationBinding) (backupNotificationBinding, error) {
	return migrationBindingTo(tx, b, backupNotificationBinding{})
}

func requireRemigrationFiles(state *backupState, binding backupNotificationBinding) (backupNotificationBinding, error) {
	rollbackFile, err := currentMigrationArtifact(state, legacyRollbackFile)
	if err != nil {
		return binding, err
	}
	remigrationPath, err := currentMigrationArtifact(state, remigrationFile)
	if err != nil {
		return binding, err
	}
	d, err := safefs.Open(state.directory)
	if err != nil {
		return binding, err
	}
	defer d.Close()
	var file backupRemigration
	fileErr := d.ReadJSON(remigrationPath, &file)
	tx, err := state.db.Begin()
	if err != nil {
		return binding, err
	}
	defer tx.Rollback()
	if _, err = readBackupMigrationCycles(tx, d); err != nil {
		return binding, err
	}
	r, err := readBackupRemigration(tx)
	if err != nil {
		return binding, err
	}
	if r == nil && os.IsNotExist(fileErr) {
		var complete string
		if e := d.ReadJSON(remigrationPath+".complete", &complete); !os.IsNotExist(e) {
			return binding, fmt.Errorf("orphan remigration completion evidence")
		}
		return remigrationPriorBinding(tx, binding)
	}
	if r == nil || fileErr != nil || notify.Hash(*r) != notify.Hash(file) {
		return binding, fmt.Errorf("remigration file/SQL authority incomplete")
	}
	var complete, sqlComplete string
	if err = d.ReadJSON(remigrationPath+".complete", &complete); err != nil {
		return binding, err
	}
	if err = backupNotificationMeta(tx, remigrationKey+"_complete", &sqlComplete); err != nil {
		return binding, err
	}
	if complete != notify.Hash(*r) || sqlComplete != complete {
		return binding, fmt.Errorf("remigration completion changed")
	}
	var rollback backupLegacyRollbackIntent
	if err = d.ReadJSON(rollbackFile, &rollback); err != nil {
		return binding, err
	}
	if notify.Hash(rollback) != r.Spec.RollbackHash {
		return binding, fmt.Errorf("remigration lost original rollback")
	}
	for source, want := range r.Spec.Cutover.Decisions {
		var got backupCutoverDecision
		if err = backupNotificationMeta(tx, remigrationSourcePrefix+source, &got); err != nil || got != want {
			return binding, fmt.Errorf("remigration source evidence lost or changed")
		}
	}
	return remigrationPriorBinding(tx, binding)
}
