package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func RequireSQLite(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("requires SQLite CLI")
	}
	return p
}

func CreateDatabase(t *testing.T, path, value string) {
	t.Helper()
	tool := RequireSQLite(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(tool, path, "CREATE TABLE sample(value TEXT); INSERT INTO sample VALUES ('"+value+"');").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
}

func DatabaseValue(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command(RequireSQLite(t), "-readonly", path, "SELECT value FROM sample;").CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	return strings.TrimSpace(string(out))
}
