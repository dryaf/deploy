package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupRemovalFixture struct {
	calls   int
	desired string
	result  backup.RemovalResult
}

func (f *backupRemovalFixture) Remove(ctx context.Context, target config.ServerConfig, sources map[string]config.ServerConfig, desired string) (backup.RemovalResult, error) {
	f.calls++
	f.desired = desired
	return f.result, nil
}

func TestShortBackupUninstallAndDryRun(t *testing.T) {
	file := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(file, []byte(backupOnlyInventory), 0600); err != nil {
		t.Fatal(err)
	}
	w, _ := safefs.NewID("w")
	a, _ := safefs.NewID("a")
	f := &backupRemovalFixture{result: backup.RemovalResult{WorkerID: w, ArchiveID: a, ArchivePath: "/home/deploy/archive"}}
	var out bytes.Buffer
	c := CLI{Inspector: backupTestInspector{}, BackupRemover: f, Out: &out}
	opts := Options{Inventory: file, JSON: true, NonInteractive: true, DryRun: true}
	if err := c.Run(context.Background(), "backup-worker", []string{"uninstall", "manager"}, opts); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatal("dry run removed backup")
	}
	var preview Plan
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(preview.Actions, " "), "revoke source access") || strings.Contains(strings.Join(preview.Actions, " "), "enroll declared sources") {
		t.Fatal("uninstall preview advertised installation", preview.Actions)
	}
	opts.DryRun = false
	out.Reset()
	if err := c.Run(context.Background(), "backup-worker", []string{"uninstall", "manager"}, opts); err != nil {
		t.Fatal(err)
	}
	var result BackupResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.State != "uninstalled" || result.ArchivePath != f.result.ArchivePath || result.WorkerID != w || f.calls != 1 || !validDigest(f.desired) {
		t.Fatal(out.String(), err)
	}
}
