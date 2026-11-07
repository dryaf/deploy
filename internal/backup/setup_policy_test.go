package backup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupPolicyInspectionRetainsSnapshotsWithoutAdoption(t *testing.T) {
	home, _, j := setupObservationFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	observed, policy, err := InspectSetupPolicy(context.Background(), home)
	testutil.Must(t, err)
	if policy != nil || observed.Stage != "complete" {
		t.Fatal("old policy silently reconstructed", observed, policy)
	}
	inv, err := config.ParseInventory("server.yaml", []byte("version: 3\nservers:\n  manager:\n    host: pi.invalid\n    user: backup\n    backup_worker:\n      sources: [source]\n  source: {host: source.invalid, user: app}\n"))
	testutil.Must(t, err)
	j.Declaration.Desired = inv.Servers["manager"].Desired
	j.Declaration.DesiredHash, err = j.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
	const name = ".local/state/deploy/backup-worker/setup.json"
	testutil.Must(t, root.WriteJSON(name, j, true))
	before, err := os.ReadFile(filepath.Join(home, name))
	testutil.Must(t, err)
	for range 2 {
		observed, policy, err = InspectSetupPolicy(context.Background(), home)
		testutil.Must(t, err)
		if observed.HelperSHA256 != j.Declaration.HelperSHA256 || !reflect.DeepEqual(policy, j.Declaration.Desired) {
			t.Fatal("policy evidence changed", observed, policy)
		}
	}
	after, err := os.ReadFile(filepath.Join(home, name))
	testutil.Must(t, err)
	if string(before) != string(after) {
		t.Fatal("policy inspection rewrote setup authority")
	}
	j.Declaration.Desired.Backup.Schedule = "weekly"
	testutil.Must(t, root.WriteJSON(name, j, true))
	if _, _, err = InspectSetupPolicy(context.Background(), home); err == nil {
		t.Fatal("tampered policy accepted")
	}
	if _, err = InspectSetup(context.Background(), home); err == nil {
		t.Fatal("ordinary status ignored snapshot drift")
	}
}

func TestBackupDesiredSnapshotChecksDependencyScope(t *testing.T) {
	d := config.DesiredWorkers{Backup: &config.BackupDeclaration{}, Notification: &config.NotificationDeclaration{}}
	hash, err := d.Fingerprint()
	testutil.Must(t, err)
	scope, err := d.NotificationFingerprint()
	testutil.Must(t, err)
	j := &SetupDeclaration{Desired: &d, DesiredHash: hash, NotificationDesiredHash: scope}
	testutil.Must(t, validateSetupDesired(j))
	j.NotificationDesiredHash = hash
	if validateSetupDesired(j) == nil {
		t.Fatal("different dependency pin accepted")
	}
}
