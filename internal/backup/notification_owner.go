package backup

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/safefs"
)

// Written only by the future journaled handoff while holding notifier.lock.
// Presence fences every compatible legacy sender, including test sends. Clearing
// it requires canonical revocation and reconciliation; no CLI writes it today.
type backupNotificationOwner struct {
	Schema  int
	Mode    string
	Binding backupNotificationBinding
	Socket  string
}

func readBackupNotificationOwner(tx *sql.Tx) (owner backupNotificationOwner, err error) {
	if err = backupNotificationMeta(tx, "notification_owner", &owner); err != nil {
		return
	}
	if owner.Schema != 1 || (owner.Mode != "fenced" && owner.Mode != "shared") || !filepath.IsAbs(owner.Socket) || filepath.Clean(owner.Socket) != owner.Socket || len(owner.Socket) > 103 || strings.ContainsAny(owner.Socket, "\x00\r\n") {
		return owner, fmt.Errorf("invalid backup notification ownership record")
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
	var exists int
	if err := state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_owner'").Scan(&exists); err != nil {
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
