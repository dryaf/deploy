package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestBackupHomeStoragePolicyAndUnits(t *testing.T) {
	policy := configfixture.WorkerPolicy(t)
	policy.Storage.Mode = "home"
	policy.Storage.RequireMount = ""
	policy.Storage.Path = "/home/backup/.local/share/deploy/backup-worker/archive"
	if _, err := config.NormalizeWorker(policy); err != nil {
		t.Fatal(err)
	}
	base, err := workerStorageBase("/home/backup", policy.Storage)
	if err != nil || base != "/home/backup" {
		t.Fatal(base, err)
	}
	paths := WorkerUnitPaths{Home: "/home/backup", Config: "/home/backup/.config/deploy/backup-worker/runtime.json", State: "/home/backup/.local/state/deploy/backup-worker/state", Archive: policy.Storage.Path, Keys: "/home/backup/.config/deploy/backup-worker/ssh", RelayState: "/home/backup/.local/share/deploy/backup-worker/signal", RelaySocket: "/home/backup/.local/state/deploy/backup-worker/relay"}
	image := "example.test/deploy@sha256:" + strings.Repeat("a", 64)
	units, err := renderWorkerUnits(WorkerInstallation{Policy: policy}, paths, image, image)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(units["deploy-backup.container"], "ConditionPathIsMountPoint=") || strings.Contains(units["deploy-backup.container"], "RequiresMountsFor=") {
		t.Fatal("home treated as mountpoint")
	}
	policy.Storage.RequireMount = "/mnt/backup"
	if _, err = config.NormalizeWorker(policy); err == nil {
		t.Fatal("contradictory storage policy accepted")
	}
	policy.Storage.Mode = ""
	policy.Storage.RequireMount = ""
	if _, err = config.NormalizeWorker(policy); err == nil {
		t.Fatal("legacy required mount default weakened")
	}
}
func TestBackupRequiredMountHasNoHomeFallback(t *testing.T) {
	data := []byte("31 1 8:1 / / rw - ext4 /dev/sda rw\n32 1 8:2 / /mnt/backup\\040drive rw - ext4 /dev/sdb rw\n")
	if workerMountPresent(data, "/mnt/missing") || workerMountPresent(data, "/home/backup") {
		t.Fatal("missing external mount accepted")
	}
	if !workerMountPresent(data, "/mnt/backup drive") {
		t.Fatal("escaped mount point not recognized")
	}
	for _, s := range []config.WorkerStorage{{Mode: "home", Path: "/home/backup"}, {Mode: "home", Path: "/home/backup/archive", RequireMount: "/mnt/backup"}, {Path: "/home/backup/archive", RequireMount: "/mnt/backup"}, {Mode: "unknown", Path: "/archive"}} {
		if _, err := workerStorageBase("/home/backup", s); err == nil {
			t.Fatal("invalid storage accepted", s)
		}
	}
}

func TestBackupDefaultArchiveCreatesOnlyInsideInspectedXDGRoot(t *testing.T) {
	p, err := ResolveInstallationPaths("/home/pi", "", "", "/srv/pi-data", config.WorkerPaths{})
	if err != nil {
		t.Fatal(err)
	}
	s := config.WorkerStorage{Mode: "home", Path: filepath.Join(p.Data, "archive")}
	base, err := workerStorageBase(p.Home, s, p)
	if err != nil || base != p.DataHome {
		t.Fatal("default XDG archive lost its creation root", base, err)
	}
	s.Path = "/other/backup/archive"
	base, err = workerStorageBase(p.Home, s, p)
	if err != nil || base != "/other/backup" {
		t.Fatal("unrelated override acquired XDG creation authority", base, err)
	}
}

func TestBackupHomeStoragePinsExistingFilesystemWithoutCreatingPaths(t *testing.T) {
	home := t.TempDir()
	archive := filepath.Join(home, "new/archive")
	before, err := homeArchiveFilesystem(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(archive)); !os.IsNotExist(err) {
		t.Fatal("inspection created archive parent")
	}
	if err = os.MkdirAll(archive, 0700); err != nil {
		t.Fatal(err)
	}
	after, err := homeArchiveFilesystem(archive)
	if err != nil || before != after {
		t.Fatal("same filesystem pin changed", err)
	}
	link := filepath.Join(home, "link")
	if err = os.Symlink(archive, link); err != nil {
		t.Fatal(err)
	}
	if _, err = homeArchiveFilesystem(filepath.Join(link, "child")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
}
