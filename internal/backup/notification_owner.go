package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Written by the journaled handoff while holding notifier.lock. Presence fences
// compatible legacy senders, including test sends. Revocation retains the fence.
type backupNotificationOwner struct {
	Schema           int
	Mode             string
	Binding          backupNotificationBinding
	CutoverHash      string
	Socket           string
	LegacyGeneration string
	LegacyState      string
	LegacyHash       string
}

func readBackupNotificationOwner(tx *sql.Tx) (owner backupNotificationOwner, err error) {
	if err = backupNotificationMeta(tx, "notification_owner", &owner); err != nil {
		return
	}
	if owner.Schema != 1 || (owner.Mode != "fenced" && owner.Mode != "shared" && owner.Mode != "legacy") || !filepath.IsAbs(owner.Socket) || filepath.Clean(owner.Socket) != owner.Socket || len(owner.Socket) > 103 || strings.ContainsAny(owner.Socket, "\x00\r\n") {
		return owner, fmt.Errorf("invalid backup notification ownership record")
	}
	if owner.Mode == "legacy" && (!notify.ValidToken(owner.LegacyGeneration) || owner.LegacyGeneration == owner.Binding.Generation || !backupRecoveryPath(owner.LegacyState) || !safefs.DigestPattern.MatchString(owner.LegacyHash)) {
		return owner, fmt.Errorf("invalid restored legacy authority")
	}
	var cutover bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key='notification_cutover')").Scan(&cutover); err != nil {
		return owner, err
	}
	if cutover {
		var hash string
		if err = backupNotificationMeta(tx, "notification_cutover", &hash); err != nil {
			return owner, err
		}
		if hash != owner.CutoverHash || !safefs.DigestPattern.MatchString(hash) {
			return owner, fmt.Errorf("cutover ownership anchor changed")
		}
	}
	binding, err := readBackupNotificationBinding(tx)
	if err != nil {
		return owner, err
	}
	if binding != owner.Binding {
		return owner, fmt.Errorf("backup notification ownership binding changed")
	}
	return owner, nil
}

func requireLegacyNotificationOwner(state *backupState) error {
	dir, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	for _, name := range []string{backupCutoverFile, legacyRollbackFile} {
		file, err := dir.Open(name, os.O_RDONLY, 0)
		if err == nil {
			file.Close()
			return fmt.Errorf("legacy delivery is fenced by retained managed ownership intent")
		}
		if !os.IsNotExist(err) {
			return err
		}
	}

	var exists int
	if err := state.db.QueryRow("SELECT count(*) FROM meta WHERE key IN ('notification_owner','notification_cutover','notification_legacy_rollback','notification_legacy_rollback_revoked')").Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return fmt.Errorf("legacy notification delivery is fenced; complete notification ownership reconciliation")
	}
	return nil
}

// The caller holds backup-worker-setup.lock, which a notification migration must
// also hold. Installation recovery may inspect state behind installation.pending,
// but must not restore a legacy helper or activity while delivery is fenced.
// Until shared lifecycle reconciliation exists, reject the whole legacy update.
func requireLegacyWorkerSetup(home string, journal *WorkerSetupJournal) error {
	if err := requireNoSignalMaintenance(home); err != nil {
		return err
	}
	if !journal.StateInitialized && !journal.RecoveryNeeded && !journal.Updating && journal.Stage != "complete" {
		return nil // Initial state creation is checked after opening the database.
	}
	state, err := openBackupStateWithLease(filepath.Join(home, ".local/state/deploy/backup-worker/state"), false, false, true)
	if err != nil {
		return err
	}
	defer state.Close()
	return requireLegacyNotificationOwner(state)
}

func backupNotifierLease(state *backupState) (*safefs.Lock, error) {
	dir, err := safefs.Open(state.directory)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.Lock("notifier.lock", true, true)
}

func requireBackupCutoverOwner(state *backupState, owner backupNotificationOwner) error {
	if owner.CutoverHash == "" {
		return nil
	} // Pre-migration fixture/recovery owners.
	d, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer d.Close()
	var intent backupCutoverIntent
	if err = d.ReadJSON(backupCutoverFile, &intent); err != nil {
		return err
	}
	var hash string
	if err = state.get("notification_cutover", &hash); err != nil {
		return err
	}
	if hash != owner.CutoverHash || notify.Hash(intent) != hash || intent.Spec.Managed.Producer != owner.Binding.Producer || intent.Spec.Managed.Generation != owner.Binding.Generation || intent.Spec.Managed.Route != owner.Binding.Route {
		return fmt.Errorf("shared report cutover authority changed")
	}
	return requireBackupLegacyResolutionFiles(state, d)
}
