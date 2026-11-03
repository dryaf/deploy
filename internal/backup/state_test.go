//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupBootstrapResumesOnlyJournalOwnedEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "worker.db"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if state, err := openBackupState(path, false); err == nil {
		state.Close()
		t.Fatal("normal open initialized incomplete state")
	}
	state, err := openBackupStateBootstrap(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = state.put("sentinel", "keep"); err != nil {
		t.Fatal(err)
	}
	state.Close()
	state, err = openBackupStateBootstrap(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	var value string
	if err = state.get("sentinel", &value); err != nil || value != "keep" {
		t.Fatal("resume replaced existing state", value, err)
	}
}
