package deployment

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	deploytransport "github.com/dryaf/deploy/internal/transport"
)

func TestSSHConnectionsSeparatePortsAndKeys(t *testing.T) {
	env := config.Environment{Host: "shared.example", User: "deploy", Port: 2222, SSHKey: "a.pem"}
	socket := func(e config.Environment) string {
		for _, arg := range deploytransport.GetSSHBaseArgs(e) {
			if strings.HasPrefix(arg, "ControlPath=") {
				return arg
			}
		}
		t.Fatal("no control socket")
		return ""
	}
	first := socket(env)
	env.Port = 2223
	if first == socket(env) {
		t.Fatal("different ports share a control socket")
	}
	env.Port = 2222
	env.SSHKey = "b.pem"
	if first == socket(env) {
		t.Fatal("different identities share a control socket")
	}
	if err := deploytransport.PrepareSSHControlDir(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Lstat(deploytransport.SshControlDir())
	if err != nil || !stat.IsDir() || stat.Mode().Perm() != 0700 {
		t.Fatal(stat, err)
	}
}

func TestRsyncHomePathsWithSecludedArguments(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("requires rsync")
	}
	root := t.TempDir()
	home := filepath.Join(root, "remote home")
	if err := os.MkdirAll(filepath.Join(home, ".config/containers/systemd"), 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "unit with spaces.container")
	if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	transport := `#!/bin/bash
set -eu
cd -- "$REMOTE_HOME"
while (( $# )); do
 if [[ $1 == rsync ]]; then exec "$@"; fi
 shift
done
exit 1
`
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(transport), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_HOME", home)
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	if err := deploytransport.RunRsyncSafe(config.Environment{User: "deploy", Host: "example.com"}, []string{source}, "deploy@example.com:~/.config/containers/systemd/"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".config/containers/systemd", filepath.Base(source)))
	if err != nil || string(got) != "payload" {
		t.Fatal(string(got), err)
	}
}
