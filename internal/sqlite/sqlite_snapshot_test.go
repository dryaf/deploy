package sqlite

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSQLiteSnapshotRejectsUninitializedSource(t *testing.T) {
	testutil.RequireSQLite(t)
	for name, content := range map[string][]byte{"zero": {}, "truncated": []byte("SQLite format 3\x00"), "garbage": []byte(strings.Repeat("x", 100))} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.db")
			dest := filepath.Join(root, "snapshot.db")
			if err := os.WriteFile(source, content, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Snapshot(source, dest); err == nil {
				t.Fatal("uninitialized source accepted")
			}
			if err := ValidateSQLiteSnapshot(source); err == nil {
				t.Fatal("uninitialized received snapshot accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("destination created for invalid source")
			}
		})
	}
	root := t.TempDir()
	if err := Snapshot(filepath.Join(root, "missing.db"), filepath.Join(root, "snapshot.db")); err == nil {
		t.Fatal("missing source accepted")
	}
	source := filepath.Join(root, "empty.db")
	if out, err := exec.Command(testutil.RequireSQLite(t), source, "VACUUM;").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := Snapshot(source, filepath.Join(root, "initialized-empty.db")); err != nil {
		t.Fatal("initialized empty DB rejected", err)
	}
}

func TestSQLiteWALSnapshotIsStandaloneAndLeavesSourceWAL(t *testing.T) {
	testutil.RequireSQLite(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python required for WAL fixture")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	archive := filepath.Join(root, "archive")
	if err = os.Mkdir(archive, 0700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(archive, "database.db")
	// Leave committed pages in WAL, as a process crash would; do not checkpoint them.
	script := `import os,sqlite3,sys
c=sqlite3.connect(sys.argv[1]); c.execute('PRAGMA journal_mode=WAL'); c.execute('PRAGMA wal_autocheckpoint=0'); c.execute('CREATE TABLE sample(value TEXT)'); c.execute("INSERT INTO sample VALUES ('wal-data')"); c.commit(); os._exit(0)`
	if out, err := exec.Command(python, "-I", "-c", script, source).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err = Snapshot(source, dest); err != nil {
		t.Fatal(err)
	}
	if testutil.DatabaseValue(t, dest) != "wal-data" {
		t.Fatal("committed WAL data lost")
	}
	if out, err := exec.Command(testutil.RequireSQLite(t), "-readonly", source, "PRAGMA journal_mode;").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "wal" {
		t.Fatal("source journal mode changed", err, string(out))
	}
	before, err := safefs.FileChecksum(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dest, 0400); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(archive, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(archive, 0700)
	if err = ValidateStandaloneSQLiteSnapshot(dest, time.Second); err != nil {
		t.Fatal(err)
	}
	after, err := safefs.FileChecksum(dest)
	if err != nil || before != after {
		t.Fatal("validation changed bytes", err)
	}
	entries, err := os.ReadDir(archive)
	if err != nil || len(entries) != 1 {
		t.Fatal("validation created sidecars", err)
	}
}

func TestSQLiteSnapshotRejectsDestinationAliases(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	testutil.CreateDatabase(t, source, "original")
	for _, name := range []string{"same", "symlink", "hardlink"} {
		t.Run(name, func(t *testing.T) {
			dest := source
			if name != "same" {
				dest = filepath.Join(root, name)
				var err error
				if name == "symlink" {
					err = os.Symlink(source, dest)
				} else {
					err = os.Link(source, dest)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := Snapshot(source, dest); err == nil {
				t.Fatal("aliased destination accepted")
			}
			if testutil.DatabaseValue(t, source) != "original" {
				t.Fatal("source changed")
			}
		})
	}
}

func TestSQLiteValidatorUnsupportedSchemaAndBoundedOutput(t *testing.T) {
	testutil.RequireSQLite(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python required for collation fixture")
	}
	file := filepath.Join(t.TempDir(), "custom.db")
	script := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); c.create_collation('application_order',lambda a,b:(a>b)-(a<b)); c.execute('CREATE TABLE sample(value TEXT COLLATE application_order)'); c.execute('CREATE INDEX sample_index ON sample(value)'); c.execute("INSERT INTO sample VALUES ('x')"); c.commit(); c.close()`
	if out, err := exec.Command(python, "-I", "-c", script, file).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	err = ValidateSQLiteSnapshot(file)
	var finding *ValidationError
	if !errors.As(err, &finding) || finding.Kind != "unsupported-validator" {
		t.Fatalf("unsupported schema misclassified: %v", err)
	}
	var output process.Buffer
	output.Limit = 4
	if _, err = output.Write([]byte("12345")); err == nil || output.Len() > 4 {
		t.Fatal("unbounded output accepted")
	}
}
