package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureOwnedDirectoryPrivateCreationAndExistingPermissions(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "new", "leaf")
	d, err := EnsureOwnedDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	for _, path := range []string{filepath.Join(parent, "new"), path} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("new directory is not private", info, err)
		}
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	d, err = EnsureOwnedDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("changed existing permissions", info, err)
	}
	if err := os.Chmod(path, 0770); err != nil {
		t.Fatal(err)
	}
	if d, err := EnsureOwnedDirectory(path); err == nil {
		d.Close()
		t.Fatal("accepted writable directory")
	}
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0770 {
		t.Fatal("repaired unowned permissions", info, err)
	}
}

func TestEnsureOwnedDirectoryRefusesSymlinkWalk(t *testing.T) {
	parent, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(parent, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "new", "leaf")} {
		if d, err := EnsureOwnedDirectory(path); err == nil {
			d.Close()
			t.Fatal("traversed symlink", path)
		}
	}
	if _, err := os.Lstat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatal("created files through symlink", err)
	}
}
