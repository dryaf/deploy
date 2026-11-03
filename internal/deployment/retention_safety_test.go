//go:build linux

package deployment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupRegressionLocalRetentionCorruptSurvivor(t *testing.T) {
	t.Chdir(t.TempDir())
	testutil.CreateDatabase(t, "app.db", "current")
	testutil.Must(t, os.Mkdir("backup", 0700))
	for i, name := range []string{"old", "new"} {
		file := filepath.Join("backup", name+".db")
		testutil.CreateDatabase(t, file, name)
		testutil.Must(t, recordLocalBackup("app.db", file, "local", "before-pull-from-prod"))
		raw, err := os.ReadFile(file + ".meta.json")
		testutil.Must(t, err)
		var rec backupRecord
		testutil.Must(t, json.Unmarshal(raw, &rec))
		rec.CreatedAt = time.Unix(int64(100+i), 0).UTC().Format(time.RFC3339Nano)
		testutil.Must(t, safefs.WritePrivateJSON(file+".meta.json", rec))
	}
	testutil.Must(t, os.WriteFile("backup/new.db", []byte("corrupt"), 0600))
	err := PruneLocalDatabase(config.Environment{Name: "prod", Database: config.DatabaseConfig{Driver: "sqlite", Source: "app.db", Retention: config.RetentionConfig{KeepLast: 1}}}, false)
	if err == nil {
		t.Fatal("cleanup accepted corrupt retained backup")
	}
	if _, exists := os.Stat("backup/old.db"); os.IsNotExist(exists) {
		t.Errorf("last valid local snapshot deleted despite corrupt retained newest; prune returned %v", err)
	}
}

func TestBackupRegressionRemoteRetentionCorruptSurvivor(t *testing.T) {
	f := newTransactionFixture(t)
	setupFakeSSH(t, f)
	f.success(f.stage("A", nil, false))
	database := filepath.Join(f.target, "app.db")
	for i, name := range []string{"old", "new"} {
		file := filepath.Join(f.target, "backup", name+".db")
		testutil.CreateDatabase(t, file, name)
		testutil.Must(t, recordLocalBackup(database, file, "prod", "before-release"))
		when := time.Unix(int64(100+i), 0)
		testutil.Must(t, os.Chtimes(file, when, when))
	}
	testutil.Must(t, os.WriteFile(filepath.Join(f.target, "backup/new.db"), []byte("corrupt"), 0600))
	c := imageConfig()
	changeEnv(&c, func(e *config.Environment) {
		e.Dir = f.target
		e.Database = config.DatabaseConfig{Driver: "sqlite", Source: "app.db", Retention: config.RetentionConfig{KeepLast: 1}}
	})
	n, err := config.Normalize(c, "prod")
	testutil.Must(t, err)
	err = PruneHistory(n, false)
	if err == nil {
		t.Fatal("cleanup accepted corrupt retained backup")
	}
	if _, exists := os.Stat(filepath.Join(f.target, "backup/old.db")); os.IsNotExist(exists) {
		t.Errorf("last valid remote snapshot deleted despite corrupt retained newest; prune returned %v", err)
	}
}
