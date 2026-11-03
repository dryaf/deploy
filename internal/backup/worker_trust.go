package backup

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	deployconfig "github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

const workerTrustDirectory = ".config/deploy/backup-worker/ssh"

const workerRuntimeConfig = ".config/deploy/backup-worker/runtime.json"

func stagedWorkerTrustPath(id, alias string) string {
	return filepath.Join(".local/state/deploy/backup-worker", id, "ssh", alias+".known_hosts")
}

// Preparation and SSH qualification use a private candidate, never live pins.
// Older installations did not hash these files in OwnedFiles. Accept that legacy
// evidence only when the unchanged, owned runtime configuration references the
// exact managed path; subsequent publications use ordinary journal before-images.
func stageWorkerTrust(home string, root, stateRoot *safefs.Directory, journal *WorkerSetupJournal, alias string, body []byte) error {
	if !safefs.ValidID(journal.ID, "n") || !deployconfig.ValidInventoryName(alias) {
		return fmt.Errorf("invalid trust staging identity")
	}
	live := filepath.Join(workerTrustDirectory, alias+".known_hosts")
	old, err := safefs.ReadFile(root, live, 16<<10)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	expected, owned := journal.OwnedFiles[live]
	if err == nil {
		digest := fmt.Sprintf("%x", sha256.Sum256(old))
		if owned && expected != digest {
			return fmt.Errorf("worker source trust file changed outside setup")
		}
		if !owned {
			if !journal.Updating {
				return fmt.Errorf("refusing to replace an unowned worker trust file")
			}
			config, err := safefs.ReadFile(root, workerRuntimeConfig, 8<<20)
			if err != nil || journal.OwnedFiles[workerRuntimeConfig] != fmt.Sprintf("%x", sha256.Sum256(config)) {
				return fmt.Errorf("legacy trust ownership requires unchanged owned runtime configuration")
			}
			var installed WorkerInstallation
			if err = safefs.DecodeJSON(config, &installed, safefs.MaxRecordBytes); err != nil {
				return err
			}
			referenced := false
			for _, source := range installed.Sources {
				referenced = referenced || source.Alias == alias && source.KnownHosts == filepath.Join(home, live)
			}
			if !referenced {
				return fmt.Errorf("trust file is not referenced by the installed worker")
			}
			journal.OwnedFiles[live] = digest
			if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
				return err
			}
		}
	} else if owned {
		return fmt.Errorf("owned worker trust file is missing")
	}
	relative := stagedWorkerTrustPath(journal.ID, alias)
	if err = root.Mkdir(filepath.Dir(relative)); err != nil {
		return err
	}
	parent, err := root.Sub(filepath.Dir(relative))
	if err != nil {
		return err
	}
	defer parent.Close()
	previous, err := safefs.ReadFile(parent, filepath.Base(relative), 16<<10)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return safefs.ReplaceFile(parent, filepath.Base(relative), previous, body, 0600)
}

// Caller holds the exclusive installation leases and has armed recovery.
func installWorkerTrust(root, stateRoot *safefs.Directory, journal *WorkerSetupJournal, sources []SetupSource) error {
	if !journal.RecoveryNeeded {
		return fmt.Errorf("trust publication requires armed setup recovery")
	}
	for _, source := range sources {
		staged, err := safefs.ReadFile(root, stagedWorkerTrustPath(journal.ID, source.Alias), 16<<10)
		if err != nil {
			return err
		}
		if !bytesEqual(staged, []byte(source.KnownHosts)) {
			return fmt.Errorf("qualified source trust candidate changed before installation")
		}
		if err = installSetupFile(root, stateRoot, journal, filepath.Join(workerTrustDirectory, source.Alias+".known_hosts"), staged); err != nil {
			return err
		}
	}
	return nil
}
