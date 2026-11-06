package backup

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const recoveryActivationFile = "notification-recovery-activation.json"
const recoveryActivationKey = "notification_recovery_activation"

type backupRecoveryActivationSpec struct {
	Version    int                            `json:"version"`
	Managed    install.ManagedProducerRequest `json:"managed"`
	HelperHash string                         `json:"helper_hash"`
	ConfigHash string                         `json:"config_hash"`
	CommitHash string                         `json:"commit_hash"`
}

type backupRecoveryActivation struct {
	Spec         backupRecoveryActivationSpec `json:"spec"`
	Delivery     backupRecoveryCommitRecord   `json:"delivery"`
	Before       backupNotificationOwner      `json:"before"`
	After        backupNotificationOwner      `json:"after"`
	EvidenceHash string                       `json:"evidence_hash"`
}

func recoveryActivationEvidence(tx *sql.Tx) (string, error) {
	rows, err := tx.Query("SELECT key,value FROM meta WHERE key LIKE 'notification_recovery%' AND key NOT LIKE 'notification_recovery_activation%' ORDER BY key")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var values [][2]string
	for rows.Next() {
		var v [2]string
		if err = rows.Scan(&v[0], &v[1]); err != nil {
			return "", err
		}
		values = append(values, v)
	}
	return notify.Hash(values), rows.Err()
}

func (r backupRecoveryActivation) validate() error {
	s := r.Spec
	if s.Version != 1 || s.Managed.Validate() != nil || s.Managed.Revoke || s.Managed.LegacyGeneration != "" || !safefs.DigestPattern.MatchString(s.HelperHash) || !safefs.DigestPattern.MatchString(s.ConfigHash) || s.CommitHash != notify.Hash(r.Delivery) || !safefs.DigestPattern.MatchString(r.EvidenceHash) || r.Before.Mode != "fenced" || r.Before.Binding != r.Delivery.After || r.After.Binding.Identity != r.Delivery.Proof.Cursor.Identity || s.Managed.Producer != r.After.Binding.Producer || s.Managed.Generation != r.After.Binding.Generation || s.Managed.Route != r.After.Binding.Route {
		return fmt.Errorf("invalid recovered backup activation")
	}
	want := r.Before
	want.Mode = "shared"
	if want != r.After {
		return fmt.Errorf("recovered activation changed owner")
	}
	return r.Delivery.Proof.Validate()
}

func readRecoveryActivation(tx *sql.Tx, complete bool) (r backupRecoveryActivation, err error) {
	if err = backupNotificationMeta(tx, recoveryActivationKey, &r); err != nil {
		return
	}
	if err = r.validate(); err != nil {
		return
	}
	var j backupNotificationRecovery
	if err = backupNotificationMeta(tx, "notification_recovery", &j); err != nil {
		return
	}
	if err = r.Delivery.validate(j); err != nil {
		return
	}
	var commit backupRecoveryCommitRecord
	if err = backupNotificationMeta(tx, "notification_recovery_commit", &commit); err != nil {
		return
	}
	if commit != r.Delivery {
		return r, fmt.Errorf("recovered delivery commit changed")
	}
	hash, e := recoveryActivationEvidence(tx)
	if e != nil {
		return r, e
	}
	if hash != r.EvidenceHash {
		return r, fmt.Errorf("retained recovery history changed")
	}
	if err = validateBackupRetainedCommitPayloads(tx, j, true); err != nil {
		return
	}
	if complete {
		var completed backupRecoveryActivation
		if err = backupNotificationMeta(tx, recoveryActivationKey+"_complete", &completed); err != nil {
			return
		}
		if completed != r {
			return r, fmt.Errorf("recovered activation completion changed")
		}
		var revoked bool
		if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key=?)", recoveryActivationKey+"_revoked").Scan(&revoked); err != nil {
			return
		}
		if revoked {
			return r, fmt.Errorf("recovered report generation revoked")
		}
		owner, e := readBackupNotificationOwner(tx)
		if e != nil {
			return r, e
		}
		if owner != r.After {
			inherited, err := recoveryOwnerDescendsThroughCycles(tx, owner, r.After)
			if err != nil {
				return r, err
			}
			if !inherited {
				inherited, err = pendingCycleOwner(tx, owner, r.After)
				if err != nil {
					return r, err
				}
			}
			if !inherited {
				return r, fmt.Errorf("recovered report owner changed")
			}
		}
	}
	return
}

func requireRecoveryActivationFiles(state *backupState) (*backupRecoveryActivation, error) {
	if err := requireBackupRecoveryHistory(state); err != nil {
		return nil, err
	}
	var journal backupNotificationRecovery
	if err := state.get("notification_recovery", &journal); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	activationFile := backupRecoveryActivationName(journal)
	d, err := safefs.Open(state.directory)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var file backupRecoveryActivation
	err = d.ReadJSON(activationFile, &file)
	var count int
	if e := state.db.QueryRow("SELECT count(*) FROM meta WHERE key LIKE 'notification_recovery%'").Scan(&count); e != nil {
		return nil, e
	}
	if os.IsNotExist(err) && count == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tx, err := state.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := readRecoveryActivation(tx, true)
	if err != nil {
		return nil, err
	}
	var complete backupRecoveryActivation
	if err = d.ReadJSON(activationFile+".complete", &complete); err != nil {
		return nil, err
	}
	if file != r || complete != r {
		return nil, fmt.Errorf("recovery activation SQL/file disagreement")
	}
	if f, e := d.Open(activationFile+".revoked", os.O_RDONLY, 0); e == nil {
		f.Close()
		return nil, fmt.Errorf("recovered report generation revoked")
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	return &r, nil
}
