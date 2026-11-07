package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestPinnedBackupLayoutReadOnlyStatusAndMissingAuthority(t *testing.T) {
	for _, loss := range []string{"none", "layout", "runtime", "identity"} {
		t.Run(loss, func(t *testing.T) {
			home, state, j := setupObservationFixture(t)
			testutil.Must(t, state.Close())
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			var c WorkerInstallation
			testutil.Must(t, readInstalledWorkerJSON(root, &c))
			external := t.TempDir()
			desired := config.WorkerPaths{Runtime: filepath.Join(external, "runtime.json"), State: filepath.Join(external, "database"), Installation: "~/retained-helper"}
			p, err := ResolveInstallationPaths(home, "", "", "", desired)
			testutil.Must(t, err)
			// This is a manually arranged read-only fixture. New setup can pin
			// these paths; relocating an existing installation remains a separate
			// operation and cannot be inferred from changed YAML.
			testutil.Must(t, os.Rename(filepath.Join(home, ".local/state/deploy/backup-worker/state"), p.State))
			testutil.Must(t, os.Rename(filepath.Join(home, ".local/lib/deploy/backup-worker"), p.Installation))
			c.Paths = &p
			raw, err := json.Marshal(c)
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(p.Runtime, raw, 0600))
			testutil.Must(t, os.Remove(filepath.Join(home, ".config/deploy/backup-worker/runtime.json")))
			j.OwnedFiles = map[string]string{p.OwnedPath(p.Runtime): fmtDigest(raw)}
			j.Declaration.Version, j.Declaration.Paths = 2, &p
			j.Declaration.Desired = &config.DesiredWorkers{Backup: &config.BackupDeclaration{Paths: desired}}
			j.Declaration.DesiredHash, err = j.Declaration.Desired.Fingerprint()
			testutil.Must(t, err)
			switch loss {
			case "layout":
				j.Declaration.Paths = nil
			case "runtime":
				testutil.Must(t, os.Remove(p.Runtime))
			case "identity":
				j.Declaration.Paths.Home = filepath.Join(home, "changed")
			}
			testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
			got, err := InspectSetup(context.Background(), home)
			if loss == "none" {
				if err != nil || got.WorkerID != j.WorkerID || got.Stage != "complete" {
					t.Fatal("status lost pinned custom paths", got, err)
				}
				if _, err = loadInstalledWorker(home); err != nil {
					t.Fatal("runtime locator", err)
				}
				if statePath, err := installedBackupStatePath(home); err != nil || statePath != p.State {
					t.Fatal("state locator", statePath, err)
				}
			} else if err == nil {
				t.Fatal("lost path authority accepted", loss)
			}
			if _, err := os.Lstat(filepath.Join(home, ".config/deploy/backup-worker/runtime.json")); !os.IsNotExist(err) {
				t.Fatal("read-only status created default runtime")
			}
		})
	}
}

func TestPinnedBackupFilesPublishAndRollbackOutsideHome(t *testing.T) {
	home, external := t.TempDir(), t.TempDir()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	testutil.Must(t, root.Mkdir("journal"))
	journalRoot, err := root.Sub("journal")
	testutil.Must(t, err)
	defer journalRoot.Close()
	name := filepath.Join(external, "runtime.json")
	before, after := []byte("old owned runtime"), []byte("new runtime")
	testutil.Must(t, os.WriteFile(name, before, 0600))
	j := WorkerSetupJournal{OwnedFiles: map[string]string{name: fmtDigest(before)}, Files: map[string]setupFileChange{}}
	testutil.Must(t, installSetupFile(root, journalRoot, &j, name, after))
	// A foreign edit prevents both rollback and another publication.
	testutil.Must(t, os.WriteFile(name, []byte("foreign change"), 0600))
	if rollbackWorkerFiles(root, &j) == nil {
		t.Fatal("rollback clobbered a foreign edit")
	}
	if installSetupFile(root, journalRoot, &j, name, after) == nil {
		t.Fatal("publication clobbered a foreign edit")
	}
	testutil.Must(t, os.WriteFile(name, after, 0600))
	testutil.Must(t, rollbackWorkerFiles(root, &j))
	actual, err := os.ReadFile(name)
	testutil.Must(t, err)
	if string(actual) != string(before) {
		t.Fatal("rollback did not restore external runtime")
	}
	newFile := filepath.Join(external, "new-unit.service")
	testutil.Must(t, installSetupFile(root, journalRoot, &j, newFile, after))
	testutil.Must(t, rollbackWorkerFiles(root, &j))
	if _, err := os.Lstat(newFile); !os.IsNotExist(err) {
		t.Fatal("rollback retained an unpublished file", err)
	}
}

func TestPinnedBackupReportSlotChecksSelectedDirectory(t *testing.T) {
	home := t.TempDir()
	p, err := ResolveInstallationPaths(home, "", "", "", config.WorkerPaths{UnitDirectory: "~/selected-units"})
	testutil.Must(t, err)
	fragment := filepath.Join(p.UnitDirectory, "deploy-backup-report.service")
	u := backupCutoverManager{home: home, paths: p.unitPaths(filepath.Join(home, "archive")), run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("LoadState=loaded\nActiveState=inactive\nFragmentPath=" + fragment + "\nDropInPaths=\nNames=deploy-backup-report.service\n"), nil
	}}
	testutil.Must(t, u.reportSlot(context.Background(), "deploy-backup-report.service"))
	fragment = filepath.Join(home, ".config/systemd/user/deploy-backup-report.service")
	if err := u.reportSlot(context.Background(), "deploy-backup-report.service"); err == nil || !strings.Contains(err.Error(), "another fragment") {
		t.Fatal("accepted old manager fragment", err)
	}
}

func TestBackupOwnedAbsolutePathRefusesSymlink(t *testing.T) {
	root, err := safefs.Open(t.TempDir())
	testutil.Must(t, err)
	defer root.Close()
	external := t.TempDir()
	testutil.Must(t, os.WriteFile(filepath.Join(external, "owned"), []byte("fixture"), 0600))
	testutil.Must(t, os.Symlink(external, filepath.Join(root.File().Name(), "link")))
	if _, err = readBackupOwnedFile(root, filepath.Join(root.File().Name(), "link/owned"), 100); err == nil {
		t.Fatal("absolute owned path traversed symlink")
	}
}
