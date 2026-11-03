//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func TestBackupManagedFilesRejectHardlinks(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	if err := os.WriteFile(original, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if f, err := root.Open("alias", unix.O_RDONLY, 0); err == nil {
		f.Close()
		t.Fatal("hardlink accepted")
	}
}
