package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryLimitsNormalizeForSystemd(t *testing.T) {
	for input, want := range map[string]string{"512MiB": "512M", "512MB": "512M", "512M": "512M", "1GiB": "1G", "512b": "512B", "1024": "1024"} {
		c := imageConfig()
		changeEnv(&c, func(e *Environment) { e.Quadlet.Memory = input })
		n, err := normalizeAndValidateConfig(c, "prod")
		if err != nil || n.Env.Quadlet.Memory != want {
			t.Fatalf("%s: %s %v", input, n.Env.Quadlet.Memory, err)
		}
		rendered, err := renderQuadlet(n, "")
		if err != nil || !strings.Contains(rendered, "MemoryMax="+want+"\n") {
			t.Fatal(rendered, err)
		}
	}
	for _, invalid := range []string{"0", "-1G", "1Ki", "10GiBB", "999999999999999999999G", "8E"} {
		if _, err := normalizeMemory(invalid); err == nil {
			t.Fatalf("accepted invalid memory: %s", invalid)
		}
	}
}

func TestSSHConnectionsSeparatePortsAndKeys(t *testing.T) {
	env := Environment{Host: "shared.example", User: "deploy", Port: 2222, SSHKey: "a.pem"}
	socket := func(e Environment) string {
		for _, arg := range getSSHBaseArgs(e) {
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
	if err := prepareSSHControlDir(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Lstat(sshControlDir())
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
	if err := runRsyncSafe(Environment{User: "deploy", Host: "example.com"}, []string{source}, "deploy@example.com:~/.config/containers/systemd/"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".config/containers/systemd", filepath.Base(source)))
	if err != nil || string(got) != "payload" {
		t.Fatal(string(got), err)
	}
}

func TestReadonlyArtifactDirectoriesInstallAndRollback(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "install", true: "rollback"}[failure], func(t *testing.T) {
			f := newTransactionFixture(t)
			stage := f.stage("A", map[string]string{"assets/config": "old"}, false)
			os.Chmod(filepath.Join(stage, "artifacts/assets"), 0555)
			t.Cleanup(func() {
				os.Chmod(filepath.Join(stage, "artifacts/assets"), 0700)
				os.Chmod(filepath.Join(f.target, "assets"), 0700)
			})
			f.success(stage)
			if s, err := os.Stat(filepath.Join(f.target, "assets")); err != nil || s.Mode().Perm() != 0555 {
				t.Fatal(s, err)
			}
			old := f.read(f.unit())
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			stageB := f.stage("B", map[string]string{"assets/config": "new", "assets/new": "new"}, false)
			os.Chmod(filepath.Join(stageB, "artifacts/assets"), 0500)
			t.Cleanup(func() { os.Chmod(filepath.Join(stageB, "artifacts/assets"), 0700) })
			if failure {
				f.mark("fail-health")
			}
			out, err := f.run("release", stageB)
			if !failure {
				if err != nil || f.read(filepath.Join(f.target, "assets/config")) != "new" {
					t.Fatal(out, err)
				}
				return
			}
			if err == nil || strings.Contains(out, "ROLLBACK FAILED") {
				t.Fatal(out, err)
			}
			f.assertOld(old)
			if f.read(filepath.Join(f.target, "assets/config")) != "old" {
				t.Fatal("old file not restored")
			}
			if s, err := os.Stat(filepath.Join(f.target, "assets")); err != nil || s.Mode().Perm() != 0555 {
				t.Fatal(s, err)
			}
		})
	}
}

func TestDifferentServicesCannotShareDeploymentTarget(t *testing.T) {
	f := newTransactionFixture(t)
	f.mark("hold-pull")
	a := f.command("release", f.stage("A", nil, false))
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Process.Kill()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(filepath.Join(f.root, "pull-entered")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b := f.stage("B", nil, false)
	req := filepath.Join(b, "request.json")
	f.write(req, strings.Replace(f.read(req), `"service":"pdf"`, `"service":"other"`, 1), 0600)
	if out, err := f.run("release", b); err == nil || !strings.Contains(out, "lock is busy for target_dir") {
		t.Fatal(out, err)
	}
	f.mark("allow-pull")
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run("release", b); err == nil || !strings.Contains(out, "already owned") {
		t.Fatal(out, err)
	}
	if f.current() != "A" {
		t.Fatal("first service state overwritten")
	}
}

func TestDurabilityFailuresPreserveConfirmedDeployment(t *testing.T) {
	for _, point := range []string{"fail-pending-sync", "fail-target-sync"} {
		t.Run(point, func(t *testing.T) {
			f := newTransactionFixture(t)
			f.success(f.stage("A", map[string]string{"config": "old"}, false))
			old := f.read(f.unit())
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark(point)
			before := f.read(filepath.Join(f.root, "calls"))
			if out, err := f.run("release", f.stage("B", map[string]string{"config": "new"}, false)); err == nil {
				t.Fatal(out)
			}
			f.assertOld(old)
			if f.read(filepath.Join(f.target, "config")) != "old" {
				t.Fatal("file not restored")
			}
			if strings.Contains(strings.TrimPrefix(f.read(filepath.Join(f.root, "calls")), before), "systemctl stop") {
				t.Fatal("stopped the healthy service before any active mutation")
			}
		})
	}
}
