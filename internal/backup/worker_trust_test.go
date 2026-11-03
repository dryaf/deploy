//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func trustFixture(t *testing.T) (string, *safefs.Directory, *safefs.Directory, *WorkerSetupJournal) {
	t.Helper()
	home := t.TempDir()
	setupRecoveryFixture(t, home)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	t.Cleanup(func() { root.Close() })
	stateRoot, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	t.Cleanup(func() { stateRoot.Close() })
	var journal WorkerSetupJournal
	testutil.Must(t, stateRoot.ReadJSON("setup.json", &journal))
	return home, root, stateRoot, &journal
}

func TestBackupTrustPreparationIsolatedAndPublicationRollback(t *testing.T) {
	home, root, stateRoot, journal := trustFixture(t)
	candidate := []byte("new host key")
	sources := []SetupSource{{Alias: "source", KnownHosts: string(candidate)}}
	assertLive := func(want string) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(home, recoveryFixtureTrust))
		testutil.Must(t, err)
		if string(body) != want {
			t.Fatalf("live SSH trust changed: %q", body)
		}
	}
	testutil.Must(t, stageWorkerTrust(home, root, stateRoot, journal, "source", candidate))
	assertLive("old host key")
	testutil.Must(t, recoverWorkerSetup(home)) // Preparation failed/disconnected before recovery was armed.
	assertLive("old host key")
	if err := installWorkerTrust(root, stateRoot, journal, sources); err == nil {
		t.Fatal("published trust without recovery")
	}
	journal.RecoveryNeeded = true
	staged := filepath.Join(home, stagedWorkerTrustPath(journal.ID, "source"))
	testutil.Must(t, os.WriteFile(staged, []byte("different candidate"), 0600))
	if err := installWorkerTrust(root, stateRoot, journal, sources); err == nil {
		t.Fatal("published a changed qualification candidate")
	}
	assertLive("old host key")
	testutil.Must(t, stageWorkerTrust(home, root, stateRoot, journal, "source", candidate))
	testutil.Must(t, installWorkerTrust(root, stateRoot, journal, sources))
	assertLive("new host key")
	var recovered WorkerSetupJournal
	testutil.Must(t, stateRoot.ReadJSON("setup.json", &recovered))
	testutil.Must(t, rollbackWorkerFiles(root, &recovered))
	assertLive("old host key")
}

func TestBackupTrustRejectsLiveDrift(t *testing.T) {
	home, root, stateRoot, journal := trustFixture(t)
	testutil.Must(t, stageWorkerTrust(home, root, stateRoot, journal, "source", []byte("new host key")))
	testutil.Must(t, os.WriteFile(filepath.Join(home, recoveryFixtureTrust), []byte("administrator edit"), 0600))
	journal.RecoveryNeeded = true
	if err := installWorkerTrust(root, stateRoot, journal, []SetupSource{{Alias: "source", KnownHosts: "new host key"}}); err == nil {
		t.Fatal("overwrote concurrent trust edit")
	}
	body, err := os.ReadFile(filepath.Join(home, recoveryFixtureTrust))
	testutil.Must(t, err)
	if string(body) != "administrator edit" {
		t.Fatal("lost operator trust edit")
	}
}

func TestBackupTrustLegacyOwnershipRequiresExactReference(t *testing.T) {
	for _, reference := range []string{recoveryFixtureTrust, ".config/deploy/backup-worker/ssh/other.known_hosts"} {
		t.Run(filepath.Base(reference), func(t *testing.T) {
			home, root, stateRoot, journal := trustFixture(t)
			delete(journal.OwnedFiles, recoveryFixtureTrust)
			c := WorkerInstallation{Schema: 1, WorkerID: journal.WorkerID, ArchiveID: journal.ArchiveID, Sources: []source.WorkerSource{{Alias: "source", KnownHosts: filepath.Join(home, reference)}}}
			testutil.Must(t, root.WriteJSON(workerRuntimeConfig, c, true))
			body, err := safefs.ReadFile(root, workerRuntimeConfig, 8<<20)
			testutil.Must(t, err)
			journal.OwnedFiles[workerRuntimeConfig] = fmtDigest(body)
			err = stageWorkerTrust(home, root, stateRoot, journal, "source", []byte("new host key"))
			if reference != recoveryFixtureTrust {
				if err == nil {
					t.Fatal("adopted unreferenced trust file")
				}
				return
			}
			testutil.Must(t, err)
			var persisted WorkerSetupJournal
			testutil.Must(t, stateRoot.ReadJSON("setup.json", &persisted))
			if persisted.OwnedFiles[recoveryFixtureTrust] != fmtDigest([]byte("old host key")) {
				t.Fatal("legacy ownership was not durable")
			}
		})
	}
}

func TestBackupTrustInitialPublicationRollback(t *testing.T) {
	home, root, stateRoot, journal := trustFixture(t)
	testutil.Must(t, os.Remove(filepath.Join(home, recoveryFixtureTrust)))
	delete(journal.OwnedFiles, recoveryFixtureTrust)
	journal.Updating = false
	testutil.Must(t, stageWorkerTrust(home, root, stateRoot, journal, "source", []byte("first host key")))
	if _, err := os.Stat(filepath.Join(home, recoveryFixtureTrust)); !os.IsNotExist(err) {
		t.Fatal("initial staging published live trust")
	}
	journal.RecoveryNeeded = true
	testutil.Must(t, installWorkerTrust(root, stateRoot, journal, []SetupSource{{Alias: "source", KnownHosts: "first host key"}}))
	var recovered WorkerSetupJournal
	testutil.Must(t, stateRoot.ReadJSON("setup.json", &recovered))
	testutil.Must(t, rollbackWorkerFiles(root, &recovered))
	if _, err := os.Stat(filepath.Join(home, recoveryFixtureTrust)); !os.IsNotExist(err) {
		t.Fatal("rollback retained new trust file")
	}
}
