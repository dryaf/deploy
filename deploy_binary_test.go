package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type localBinaryReleaseRunner struct {
	fakeRunner
	fixture *transactionFixture
}

func (r *localBinaryReleaseRunner) SSH(_ Environment, script string) (string, error) {
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = r.fixture.command("release", "unused").Env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *localBinaryReleaseRunner) Sync(e Environment, sources []string, destination string, extra ...string) error {
	args := append([]string{"-a"}, extra...)
	args = append(args, sources...)
	args = append(args, strings.TrimPrefix(destination, remoteDestination(e, "")))
	if out, err := exec.Command("rsync", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("local release sync: %w\n%s", err, out)
	}
	return nil
}

func TestBinaryReleaseUploadsBuiltBinaryDespiteArtifactExclusions(t *testing.T) {
	for _, exclusion := range []string{"server", "server*", ""} {
		t.Run(displayDefault(exclusion, "same-name-artifact"), func(t *testing.T) {
			f := newTransactionFixture(t)
			t.Chdir(f.root)
			f.write("compiled-server", "new binary", 0755)
			f.write("server", "old binary", 0755)
			info, err := os.Stat("compiled-server")
			if err != nil {
				t.Fatal(err)
			}
			// Match size and mtime to catch rsync quick-check skipping the build output.
			if err := os.Chtimes("server", info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			f.write("Dockerfile.vps", "FROM scratch\nCOPY server /server\n", 0600)
			f.write("assets/keep.txt", "included", 0600)
			f.write("assets/secret.db", "excluded", 0600)
			f.write(filepath.Join(f.target, "server"), "remote old binary", 0755)
			c := imageConfig()
			c.Deployment.Mode = ModeBinary
			c.BinaryName = "server"
			c.Build.Cmd = "cp -p compiled-server build/server"
			c.Artifacts.Include = []string{"Dockerfile.vps", "assets", "server"}
			c.Artifacts.Exclude = []string{"*.db"}
			if exclusion != "" {
				c.Artifacts.Exclude = append(c.Artifacts.Exclude, exclusion)
			}
			changeEnv(&c, func(e *Environment) {
				e.Dir = f.target
				e.Quadlet.EnvFile = ptr("")
				e.Quadlet.Image = "localhost/pdf:latest"
			})
			n, err := normalizeAndValidateConfig(c, "prod")
			if err != nil {
				t.Fatal(err)
			}
			p, err := planBinaryRelease(n, "v1", "abc")
			if err != nil {
				t.Fatal(err)
			}
			runner := &filesystemRunner{env: f.command("release", "unused").Env}
			if err := executeBinaryRelease(p, runner); err != nil {
				t.Fatal(err)
			}
			if got := f.read(filepath.Join(f.target, "server")); got != "new binary" {
				t.Fatalf("deployed %q instead of the newly built binary", got)
			}
			if got := f.read(filepath.Join(f.target, "assets/keep.txt")); got != "included" {
				t.Fatalf("included artifact changed: %q", got)
			}
			f.assertAbsent(filepath.Join(f.target, "assets/secret.db"))
		})
	}
}

func TestBinaryReleasePreparesDataDirectory(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "existing"}[existing], func(t *testing.T) {
			f := newTransactionFixture(t)
			t.Chdir(f.root)
			data := filepath.Join(f.target, "data")
			if existing {
				f.write(filepath.Join(data, "keep.db"), "persistent data", 0600)
				if err := os.Chmod(data, 0750); err != nil {
					t.Fatal(err)
				}
			} else {
				f.assertAbsent(f.target)
			}
			c := imageConfig()
			c.Deployment.Mode = ModeBinary
			c.BinaryName = "server"
			c.Artifacts.Include = []string{"Dockerfile.vps"}
			changeEnv(&c, func(e *Environment) {
				e.Dir = f.target
				e.Quadlet.EnvFile = ptr("")
				e.Quadlet.Image = "localhost/pdf:latest"
				e.Quadlet.ContainerUID = 65532
				e.Quadlet.ContainerGID = 65532
				e.Quadlet.ChownVolumes = []string{"./data"}
				e.Quadlet.Volumes = []string{"./data:/data:Z"}
			})
			n, err := normalizeAndValidateConfig(c, "prod")
			if err != nil {
				t.Fatal(err)
			}
			p, err := planBinaryRelease(n, "v1", "abc")
			if err != nil {
				t.Fatal(err)
			}
			f.write(filepath.Join(f.root, "build/server"), "binary", 0755)
			f.write(filepath.Join(f.root, "Dockerfile.vps"), "FROM scratch", 0600)
			container := filepath.Join(f.root, "candidate.container")
			f.write(container, p.Quadlet, 0600)
			if err := stageBinaryRelease(p, container, &localBinaryReleaseRunner{fixture: f}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(f.read(filepath.Join(f.root, "calls")), "podman unshare chown -R 65532:65532 "+data) {
				t.Fatal("default data directory ownership was not updated")
			}
			if existing {
				if f.read(filepath.Join(data, "keep.db")) != "persistent data" {
					t.Fatal("existing data changed")
				}
				if info, err := os.Stat(data); err != nil || info.Mode().Perm() != 0750 {
					t.Fatal("existing data directory permissions changed", info, err)
				}
			}
		})
	}
}

func stageBinaryFixture(f *transactionFixture, id string, artifacts map[string]string, syncEnv bool) string {
	f.t.Helper()
	copyArtifacts := map[string]string{"Dockerfile.vps": "FROM scratch"}
	for name, content := range artifacts {
		copyArtifacts[name] = content
	}
	original := f.stage(id, copyArtifacts, syncEnv)
	destination := filepath.Join(f.target, ".deploy/binary/staging", id)
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(original, destination); err != nil {
		f.t.Fatal(err)
	}
	var request imageReleaseRequest
	if err := json.Unmarshal([]byte(f.read(filepath.Join(destination, "request.json"))), &request); err != nil {
		f.t.Fatal(err)
	}
	request.Reference = "localhost/pdf:latest"
	request.Stop = true
	body, err := json.Marshal(binaryReleaseRequest{imageReleaseRequest: request, Dockerfile: "Dockerfile.vps"})
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(destination, "request.json"), string(body), 0600)
	f.write(filepath.Join(destination, "candidate.container"), "[Container]\nImage=localhost/pdf:latest\nHealthCmd=true\n[Service]\nRestart=on-failure\n[Install]\nWantedBy=default.target\n", 0600)
	f.write(f.script, binaryTransactionScript, 0700)
	return destination
}

func TestBinaryTransactionFirstInstallAndRollback(t *testing.T) {
	for _, point := range []string{"fail-build", "fail-health", "fail-install", "fail-reload", "fail-restart", "fail-target-sync", "kill-after-unit", "kill-before-commit"} {
		t.Run(point, func(t *testing.T) {
			f := newTransactionFixture(t)
			f.success(stageBinaryFixture(f, "A", map[string]string{"server": "old binary", "Dockerfile.vps": "old dockerfile", "config/value": "old config"}, true))
			if strings.Contains(f.read(filepath.Join(f.root, "calls")), "systemctl stop") {
				t.Fatal("first install stopped a nonexistent service")
			}
			oldUnit := f.read(f.unit())
			state := filepath.Join(f.target, ".deploy/binary")
			oldCurrent := f.read(filepath.Join(state, "current.json"))
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark(point)
			stage := stageBinaryFixture(f, "B", map[string]string{"server": "new binary", "Dockerfile.vps": "broken dockerfile", "config/value": "new config", "config/new": "new"}, true)
			out, err := f.run("release", stage)
			if err == nil {
				t.Fatal("failure was ignored", out)
			}
			if strings.HasPrefix(point, "kill-") {
				if out, err := f.run("action", stageBinaryFixture(f, "recover", nil, false), "start"); err != nil {
					t.Fatal(out, err)
				}
			}
			if f.read(f.unit()) != oldUnit || f.read(filepath.Join(state, "current.json")) != oldCurrent {
				t.Fatal("unit or current record not restored", out)
			}
			for name, want := range map[string]string{"server": "old binary", "Dockerfile.vps": "old dockerfile", "config/value": "old config", ".env": "SECRET=A"} {
				if got := f.read(filepath.Join(f.target, name)); got != want {
					t.Fatalf("%s: %s", name, got)
				}
			}
			if f.read(filepath.Join(f.root, "running-image")) != imageA {
				t.Fatal("old image not restored", out)
			}
			f.assertAbsent(filepath.Join(f.target, "config/new"))
			f.assertAbsent(filepath.Join(state, "pending.json"))
		})
	}
}

func TestBinaryLegacyQuadletAndInactiveStateRollback(t *testing.T) {
	for _, reference := range []string{"localhost/pdf:legacy", "localhost/pdf@" + imageA} {
		t.Run(reference, func(t *testing.T) {
			f := newTransactionFixture(t)
			legacy := "[Container]\nImage=" + reference + "\nHealthCmd=true\n[Install]\nWantedBy=default.target\n"
			f.write(f.unit(), legacy, 0600)
			f.write(filepath.Join(f.root, "images", imageA), "", 0600)
			f.write(filepath.Join(f.root, "tags", strings.ReplaceAll(reference, "/", "_")), imageA, 0600)
			f.write(filepath.Join(f.target, "server"), "legacy", 0755)
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark("fail-health")
			out, err := f.run("release", stageBinaryFixture(f, "A", map[string]string{"server": "new"}, false))
			if err == nil || strings.Contains(out, "ROLLBACK FAILED") {
				t.Fatal(out, err)
			}
			if f.read(f.unit()) != legacy || f.read(filepath.Join(f.target, "server")) != "legacy" {
				t.Fatal("legacy deployment not restored", out)
			}
			f.assertAbsent(filepath.Join(f.root, "active"))
		})
	}
}

func TestBinaryLegacyTagRecoveryAfterBuildInterruption(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "active"}[active], func(t *testing.T) {
			f := newTransactionFixture(t)
			reference := "localhost/pdf:legacy"
			legacy := "[Container]\nImage=" + reference + "\nHealthCmd=true\n[Install]\nWantedBy=default.target\n"
			f.write(f.unit(), legacy, 0600)
			f.write(filepath.Join(f.root, "images", imageA), "", 0600)
			tag := filepath.Join(f.root, "tags", "localhost_pdf:legacy")
			f.write(tag, imageA, 0600)
			if active {
				f.write(filepath.Join(f.root, "running-image"), imageA, 0600)
				f.mark("active")
			}
			artifacts := map[string]string{"server": "same binary", "Dockerfile.vps": "same dockerfile"}
			for name, content := range artifacts {
				f.write(filepath.Join(f.target, name), content, 0640)
			}
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			stage := stageBinaryFixture(f, "B", artifacts, false)
			var request binaryReleaseRequest
			if err := json.Unmarshal([]byte(f.read(filepath.Join(stage, "request.json"))), &request); err != nil {
				t.Fatal(err)
			}
			request.Reference = reference
			request.Stop = false
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			f.write(filepath.Join(stage, "request.json"), string(body), 0600)
			f.mark("kill-after-build")
			if out, err := f.run("release", stage); err == nil {
				t.Fatal("build interruption was ignored", out)
			}
			if f.read(tag) != imageB {
				t.Fatal("build did not change the legacy tag")
			}
			f.mark("fail-tag")
			if out, err := f.run("action", stageBinaryFixture(f, "recover-fails", nil, false), "enable"); err == nil {
				t.Fatal("failed image reference restoration was ignored", out)
			}
			pending := filepath.Join(f.target, ".deploy/binary/pending.json")
			if _, err := os.Stat(pending); err != nil {
				t.Fatal("failed recovery lost its pending state", err)
			}
			if err := os.Remove(filepath.Join(f.root, "fail-tag")); err != nil {
				t.Fatal(err)
			}
			if out, err := f.run("action", stageBinaryFixture(f, "recover", nil, false), "enable"); err != nil {
				t.Fatal(out, err)
			}
			if f.read(tag) != imageA || f.read(f.unit()) != legacy {
				t.Fatal("recovery did not restore the previous legacy image reference")
			}
			if active {
				if _, err := os.Stat(filepath.Join(f.root, "active")); err != nil || f.read(filepath.Join(f.root, "running-image")) != imageA {
					t.Fatal("recovery did not preserve the running service", err)
				}
			} else {
				f.assertAbsent(filepath.Join(f.root, "active"))
			}
			f.assertAbsent(pending)
		})
	}
}

func TestBinaryUIDWithoutOwnershipPaths(t *testing.T) {
	f := newTransactionFixture(t)
	stage := stageBinaryFixture(f, "A", map[string]string{"server": "binary"}, false)
	var request binaryReleaseRequest
	if err := json.Unmarshal([]byte(f.read(filepath.Join(stage, "request.json"))), &request); err != nil {
		t.Fatal(err)
	}
	request.UID = 65532
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(stage, "request.json"), string(body), 0600)
	f.success(stage)
	if strings.Contains(f.read(filepath.Join(f.root, "calls")), "podman unshare") {
		t.Fatal("changed ownership without configured paths")
	}
}

func TestBinaryBuildPinsItsOwnImageDespiteTagMutation(t *testing.T) {
	f := newTransactionFixture(t)
	f.mark("mutate-build-tag")
	f.success(stageBinaryFixture(f, "A", map[string]string{"server": "binary"}, false))
	if f.read(filepath.Join(f.root, "running-image")) != imageA {
		t.Fatal("deployed another build's image")
	}
}
