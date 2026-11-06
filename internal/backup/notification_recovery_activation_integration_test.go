//go:build linux && notificationintegration

package backup

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
)

// Executes the pinned helper only. No user manager, container, credentials,
// provider endpoint or installed backup configuration is accessed.
func TestBackupRecoveryActivationNativeHelper(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(home, ".local/lib/deploy/backup-worker/deploy")
	testutil.Must(t, os.MkdirAll(filepath.Dir(binary), 0700))
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	cmd.Dir = "../.."
	if out, err := process.LimitedOutput(cmd, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	raw, err := os.ReadFile(binary)
	testutil.Must(t, err)
	testutil.Must(t, os.Remove(binary))
	testutil.Must(t, os.WriteFile(binary, raw, 0700))
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	probe := exec.Command(binary, "backup-worker", "__signal-adoption-probe")
	if out, err := process.LimitedOutput(probe, 15*time.Second, 1024); err != nil || string(out) != "backup-signal-adoption-v1\n" {
		t.Fatal("native Signal adoption capability", err, string(out))
	}
	probe = exec.Command(binary, "backup-worker", "__notification-signal-migration-probe")
	if out, err := process.LimitedOutput(probe, 15*time.Second, 1024); err != nil || string(out) != "backup-notification-signal-migration-v1\n" {
		t.Fatal("native Signal notification migration capability", err, string(out))
	}
	u := &backupCutoverManager{}
	testutil.Must(t, u.PrepareRecovery(context.Background(), WorkerUnitPaths{Home: home}, hash))
	testutil.Must(t, u.PrepareNextRecovery(context.Background(), WorkerUnitPaths{Home: home}, hash))
	testutil.Must(t, u.PrepareMigrationCycle(context.Background(), WorkerUnitPaths{Home: home}, hash))
	if u.PrepareRecovery(context.Background(), WorkerUnitPaths{Home: home}, fmt.Sprintf("%x", sha256.Sum256([]byte("old")))) == nil {
		t.Fatal("wrong helper hash accepted")
	}
	cmd = exec.Command(binary, "--dry-run", "backup-worker", "notification-recovery-activate", "--local", "--spec", filepath.Join(home, "absent.json"))
	if out, err := process.LimitedOutput(cmd, 15*time.Second, 4096); err != nil {
		t.Fatal(err, string(out))
	}
	testutil.Must(t, u.PrepareRemigration(context.Background(), WorkerUnitPaths{Home: home}, hash))
	if u.PrepareRemigration(context.Background(), WorkerUnitPaths{Home: home}, fmt.Sprintf("%x", sha256.Sum256([]byte("old")))) == nil {
		t.Fatal("wrong remigration helper hash accepted")
	}
	cmd = exec.Command(binary, "--dry-run", "backup-worker", "notification-remigrate", "--local", "--spec", filepath.Join(home, "absent.json"))
	if out, err := process.LimitedOutput(cmd, 15*time.Second, 4096); err != nil {
		t.Fatal(err, string(out))
	}
	if _, err = os.Stat(filepath.Join(home, "absent.json")); !os.IsNotExist(err) {
		t.Fatal("native dry-run wrote spec", err)
	}
}
