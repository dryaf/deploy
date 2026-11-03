package backup

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	deployhelper "github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

const recoveryTimer = "deploy-backup-recovery.timer"

func installRecoverySupervisor(home string, root, stateRoot *safefs.Directory, journal *WorkerSetupJournal) error {
	return installRecoverySupervisorFrom(home, root, stateRoot, journal, "/proc/self/exe")
}

func installRecoverySupervisorFrom(home string, root, stateRoot *safefs.Directory, journal *WorkerSetupJournal, executable string) error {
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	digest, err := safefs.DigestFile(source)
	if err != nil {
		return err
	}
	release := ".local/lib/deploy/backup-worker/recovery/" + digest
	if err = root.Mkdir(release); err != nil {
		return err
	}
	dir, err := root.Sub(release)
	if err != nil {
		return err
	}
	defer dir.Close()
	old, err := dir.Open("deploy", unix.O_RDONLY, 0)
	if err == nil {
		got, e := safefs.DigestFile(old)
		old.Close()
		if e != nil || got != digest {
			return fmt.Errorf("recovery helper ownership conflict")
		}
	} else if os.IsNotExist(err) {
		if err = deployhelper.CopyHelperFile(source, dir, "deploy", false); err != nil {
			return err
		}
	} else {
		return err
	}
	command := unitQuote(filepath.Join(home, release, "deploy")) + " backup-worker __recover_setup"
	units := map[string]string{
		"deploy-backup-recovery.service": "[Unit]\nDescription=Recover interrupted deploy backup setup\n\n[Service]\nType=oneshot\nExecStart=" + command + "\nTimeoutStartSec=5min\nUMask=0077\n",
		recoveryTimer:                    "[Unit]\nDescription=Supervise deploy backup setup across disconnect and reboot\n\n[Timer]\nOnBootSec=5s\nOnUnitInactiveSec=15s\nAccuracySec=1s\nUnit=deploy-backup-recovery.service\n\n[Install]\nWantedBy=timers.target\n",
	}
	if err = root.Mkdir(".config/systemd/user"); err != nil {
		return err
	}
	for name, body := range units {
		path := filepath.Join(".config/systemd/user", name)
		// Recover a lost acknowledgement of this supervisor file before staging again.
		if change, ok := journal.Files[path]; ok {
			actual, e := safefs.ReadFile(root, path, 8<<20)
			if e == nil && fmt.Sprintf("%x", sha256.Sum256(actual)) == change.AfterHash {
				journal.OwnedFiles[path] = change.AfterHash
				delete(journal.Files, path)
				if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
					return err
				}
			}
		}
		if err = installSetupFile(root, stateRoot, journal, path, []byte(body)); err != nil {
			return err
		}
		journal.OwnedFiles[path] = journal.Files[path].AfterHash
		delete(journal.Files, path) // Supervisor persists through application rollback.
		if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
			return err
		}
	}
	if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "daemon-reload"), 15*time.Second, 4096); err != nil {
		return err
	}
	_, err = process.LimitedOutput(exec.Command("systemctl", "--user", "enable", "--now", recoveryTimer), 15*time.Second, 4096)
	return err
}

// All normal state users hold schema.lock shared for the lifetime of their DB
// handle. Drain services first, then take schema, archive maintenance, and writer
// locks in that order. Recovery follows exactly the same order.
func lockWorkerInstallation(state *backupState, a *backupArchive) (func(), error) {
	dir, err := safefs.Open(state.directory)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	schema, err := dir.Lock("schema.lock", true, true)
	if err != nil {
		return nil, err
	}
	if a.maintenance != nil {
		a.maintenance.Close()
	}
	a.maintenance = nil
	maintenance, err := a.dir.Lock(".locks/maintenance.lock", true, false)
	if err != nil {
		schema.Close()
		return nil, err
	}
	writer, err := a.dir.Lock(".locks/writer.lock", true, false)
	if err != nil {
		maintenance.Close()
		schema.Close()
		return nil, err
	}
	return func() { writer.Close(); maintenance.Close(); schema.Close() }, nil
}

func restoreWorkerHelper(root *safefs.Directory, journal *WorkerSetupJournal) error {
	if journal.BeforeHelper == nil {
		return nil
	}
	dir, err := root.Sub(".local/lib/deploy/backup-worker")
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := dir.Open("deploy", unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	actual, err := safefs.DigestFile(file)
	file.Close()
	if err != nil {
		return err
	}
	old := *journal.BeforeHelper
	if !safefs.DigestPattern.MatchString(old.Current) || old.Schema != 1 || old.Role != "backup-worker" {
		return fmt.Errorf("invalid rollback helper evidence")
	}
	if actual != old.Current && actual != journal.CandidateHelper {
		return fmt.Errorf("helper changed outside setup; refusing rollback")
	}
	previous, err := dir.Open("releases/"+old.Current+"/deploy", unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer previous.Close()
	got, err := safefs.DigestFile(previous)
	if err != nil || got != old.Current {
		return fmt.Errorf("previous helper artifact is unavailable or changed")
	}
	if actual != old.Current {
		if err = deployhelper.CopyHelperFile(previous, dir, "deploy", true); err != nil {
			return err
		}
	}
	return dir.WriteJSON("artifact.json", old, true)
}

func rollbackWorkerInstallation(root, stateRoot *safefs.Directory, journal *WorkerSetupJournal, state *backupState, a *backupArchive) error {
	if err := requireLegacyNotificationOwner(state); err != nil {
		return err
	}
	if err := rollbackWorkerFiles(root, journal); err != nil {
		return err
	}
	if err := restoreWorkerHelper(root, journal); err != nil {
		return err
	}
	if journal.BeforeControl != nil {
		if err := a.updateControl(state, func(current *ArchiveControl) error {
			current.ConfigHash = journal.BeforeControl.ConfigHash
			current.Retention = journal.BeforeControl.Retention
			return nil
		}); err != nil {
			return err
		}
	}
	if journal.BeforeMinFreeBytes != nil {
		if err := state.put("min_free_bytes", *journal.BeforeMinFreeBytes); err != nil {
			return err
		}
	}
	if journal.BeforeInstallation != "" {
		if err := state.put("installation", journal.BeforeInstallation); err != nil {
			return err
		}
	}
	journal.Stage = "restoring-activity"
	if err := stateRoot.WriteJSON("setup.json", journal, true); err != nil {
		return err
	}
	return clearInstallationGate(state.directory, journal.ID)
}

func finishWorkerRollback(stateRoot *safefs.Directory, journal *WorkerSetupJournal, state *backupState) error {
	if err := requireLegacyNotificationOwner(state); err != nil {
		return err
	}
	if _, err := process.LimitedOutput(exec.Command("systemctl", "--user", "daemon-reload"), 15*time.Second, 4096); err != nil {
		return err
	}
	for _, unit := range journal.EnabledUnits {
		allowed := false
		for _, owned := range workerActivationUnits {
			allowed = allowed || unit == owned
		}
		if !allowed {
			return fmt.Errorf("invalid recorded activation unit")
		}
		if _, err := process.LimitedOutput(exec.Command("systemctl", "--user", "enable", "--now", unit), 15*time.Second, 4096); err != nil {
			return err
		}
	}
	completed := *journal
	completed.RecoveryNeeded = false
	completed.BeforeControl = nil
	completed.BeforeMinFreeBytes = nil
	completed.BeforeHelper = nil
	completed.BeforeInstallation = ""
	completed.CandidateHelper = ""
	completed.EnabledUnits = nil
	completed.Stage = "prepared"
	if completed.Updating {
		completed.RequestHash = completed.PreviousRequestHash
		completed.PreviousRequestHash = ""
		completed.Updating = false
		completed.Stage = "complete"
	}
	if err := stateRoot.WriteJSON("setup.json", completed, true); err != nil {
		return err
	}
	*journal = completed
	return nil
}

func recoverWorkerSetupLocked(home string, root, stateRoot *safefs.Directory, journal *WorkerSetupJournal) error {
	if err := requireLegacyWorkerSetup(home, journal); err != nil {
		return err
	}
	if !journal.RecoveryNeeded {
		if journal.Stage == "complete" {
			return clearInstallationGate(filepath.Join(home, ".local/state/deploy/backup-worker/state"), journal.ID)
		}
		return nil
	}
	if journal.Schema != 1 || !config.WorkerAbsolutePath(journal.ArchivePath) {
		return fmt.Errorf("invalid recovery journal")
	}
	if journal.Updating {
		if err := stopWorkerUnits(); err != nil {
			return err
		}
	}
	state, err := openBackupStateWithLease(filepath.Join(home, ".local/state/deploy/backup-worker/state"), false, false, true)
	if err != nil {
		return err
	}
	defer state.Close()
	a, err := openBackupArchive(journal.ArchivePath)
	if err != nil {
		return err
	}
	defer a.Close()
	if a.identity.ID != journal.ArchiveID || a.identity.WriterID != journal.WorkerID {
		return fmt.Errorf("recovery archive identity changed")
	}
	release, err := lockWorkerInstallation(state, a)
	if err != nil {
		return err
	}
	err = rollbackWorkerInstallation(root, stateRoot, journal, state, a)
	release()
	if err != nil {
		return err
	}
	return finishWorkerRollback(stateRoot, journal, state)
}

func recoverWorkerSetup(home string) error {
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	coordinator, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	}
	if err != nil {
		return err
	}
	defer coordinator.Close()
	dir, err := root.Sub(".local/state/deploy/backup-worker")
	if err != nil {
		return err
	}
	defer dir.Close()
	var journal WorkerSetupJournal
	if err = dir.ReadJSON("setup.json", &journal); err != nil {
		return err
	}
	return recoverWorkerSetupLocked(home, root, dir, &journal)
}

func prepareWorkerRecovery(root, stateRoot *safefs.Directory, journal *WorkerSetupJournal, state *backupState, a *backupArchive) error {
	if err := requireLegacyNotificationOwner(state); err != nil {
		return err
	}
	journal.ArchivePath = a.dir.File().Name()
	if journal.Updating {
		var control ArchiveControl
		if err := state.get("control", &control); err != nil {
			return err
		}
		journal.BeforeControl = &control
		var minimum int64
		if err := state.get("min_free_bytes", &minimum); err != nil {
			return err
		}
		journal.BeforeMinFreeBytes = &minimum
		if err := state.get("installation", &journal.BeforeInstallation); err != nil && err != sql.ErrNoRows {
			return err
		}
		var helper deployhelper.HelperArtifactManifest
		if err := root.ReadJSON(".local/lib/deploy/backup-worker/artifact.json", &helper); err != nil {
			return err
		}
		journal.BeforeHelper = &helper
		journal.EnabledUnits = enabledWorkerUnits()
	}
	source, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	journal.CandidateHelper, err = safefs.DigestFile(source)
	source.Close()
	if err != nil {
		return err
	}
	journal.RecoveryNeeded = true
	if err = stateRoot.WriteJSON("setup.json", journal, true); err != nil {
		return err
	}
	dir, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.WriteJSON("installation.pending", installationGate{journal.ID}, false)
}

type installationGate struct {
	Operation string `json:"operation"`
}

func clearInstallationGate(directory, id string) error {
	dir, err := safefs.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	var gate installationGate
	if err = dir.ReadJSON("installation.pending", &gate); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if gate.Operation != id {
		return fmt.Errorf("installation gate belongs to another operation")
	}
	if err = unix.Unlinkat(int(dir.File().Fd()), "installation.pending", 0); err != nil {
		return err
	}
	return dir.File().Sync()
}
