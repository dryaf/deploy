//go:build linux

package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupUninstallAfterCompletedRotation(t *testing.T) {
	home := t.TempDir()
	setupRecoveryFixture(t, home)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	_, _, c, _ := healthyWorkerFixture(t)
	body, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(home, recoveryFixtureConfig), body, 0600))
	var setup WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &setup))
	setup.Stage = "complete"
	setup.OwnedFiles[recoveryFixtureConfig] = fmtDigest(body)
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", setup, true))
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/lifecycle.json", workerLifecycleJournal{Schema: 1, Operation: "rotate-key", Stage: "complete", Installation: c}, false))
	bin := t.TempDir()
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncase \"$*\" in *is-enabled*) echo enabled;; *show*) echo loaded;; esac\n"), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	prepare := workerLifecycleRequest{Operation: "uninstall-prepare"}
	// An interrupted rotation must still be finished before another operation.
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/lifecycle.json", workerLifecycleJournal{Schema: 1, Operation: "rotate-key", Stage: "prepared", Installation: c}, true))
	if err = workerLifecycle(home, prepare); err == nil || !strings.Contains(err.Error(), "requires completion") {
		t.Fatalf("unfinished rotation was overwritten: %v", err)
	}
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/lifecycle.json", workerLifecycleJournal{Schema: 1, Operation: "rotate-key", Stage: "complete", Installation: c}, true))
	testutil.Must(t, workerLifecycle(home, prepare))
	testutil.Must(t, workerLifecycle(home, prepare))
	var journal workerLifecycleJournal
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/lifecycle.json", &journal))
	if journal.Stage != "prepared" || journal.Operation != "uninstall" {
		t.Fatalf("uninstall did not prepare: %+v", journal)
	}
	finish := workerLifecycleRequest{Operation: "uninstall-finish"}
	if err = workerLifecycle(home, finish); err == nil || !strings.Contains(err.Error(), "revocation acknowledgement missing") {
		t.Fatalf("missing revocation accepted: %v", err)
	}
	for _, source := range c.Sources {
		finish.Results = append(finish.Results, deploysource.SourceEnrollmentResult{SourceID: source.SourceID, WorkerID: c.WorkerID})
	}
	testutil.Must(t, workerLifecycle(home, finish))
	// Completed uninstall retries work even after the runtime file was removed.
	testutil.Must(t, workerLifecycle(home, prepare))
	testutil.Must(t, workerLifecycle(home, finish))
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/lifecycle.json", &journal))
	if journal.Stage != "complete" {
		t.Fatal("uninstall not completed")
	}
	for name := range setup.OwnedFiles {
		if _, err = os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("owned file remains: %s: %v", name, err)
		}
	}
	archive, err := openBackupArchive(filepath.Join(home, "archive"))
	testutil.Must(t, err)
	archive.Close()
}
