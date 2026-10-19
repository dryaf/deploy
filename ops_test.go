package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRightsOwnership(t *testing.T) {
	for _, target := range []string{"user", "container"} {
		t.Run(target, func(t *testing.T) {
			f := newTransactionFixture(t)
			t.Setenv("HOME", f.home)
			t.Setenv("FAKE_ROOT", f.root)
			t.Setenv("PATH", f.bin+":"+os.Getenv("PATH"))
			f.write(filepath.Join(f.bin, "ssh"), "#!/bin/bash\nexec bash -c \"${!#}\"\n", 0700)
			f.write(filepath.Join(f.bin, "podman"), "#!/bin/bash\n[[ $1 == unshare ]] || exit 1\nshift\nexec \"$@\"\n", 0700)
			f.write(filepath.Join(f.bin, "chown"), "#!/bin/bash\nprintf '%s\\n' \"$@\" >> \"$FAKE_ROOT/ownership\"\n", 0700)
			f.write(filepath.Join(f.target, "data/backup/retained.db"), "backup", 0600)
			f.write(filepath.Join(f.target, "data/live.db"), "live", 0600)
			e := Environment{User: "deploy", Host: "example.com", Dir: f.target, Quadlet: Quadlet{ServiceName: "pdf", ContainerUID: 65532, ContainerGID: 65533, ChownVolumes: []string{"data"}}, Database: DatabaseConfig{Driver: "sqlite", Source: "data/live.db"}}
			if err := rights(e, target); err != nil {
				t.Fatal(err)
			}
			calls := f.read(filepath.Join(f.root, "ownership"))
			owner := "0:0"
			if target == "container" {
				owner = "65532:65533"
			}
			if !strings.Contains(calls, owner) || !strings.Contains(calls, "data/live.db") || strings.Contains(calls, "backup") {
				t.Fatal(calls)
			}
		})
	}
}

func TestRightsAndPrunePropagateSSHFailure(t *testing.T) {
	f := newTransactionFixture(t)
	t.Setenv("PATH", f.bin+":"+os.Getenv("PATH"))
	f.write(filepath.Join(f.bin, "ssh"), "#!/bin/bash\nexit 42\n", 0700)
	e := Environment{User: "deploy", Host: "example.com", Dir: f.target, Quadlet: Quadlet{ServiceName: "pdf", ChownVolumes: []string{"data"}}}
	for _, err := range []error{rights(e, "user"), pruneImages(e)} {
		if err == nil || !strings.Contains(err.Error(), "42") {
			t.Fatal(err)
		}
	}
}
