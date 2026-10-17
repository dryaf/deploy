package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const imageA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const imageB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type transactionFixture struct {
	t                               *testing.T
	root, target, home, bin, script string
}

func newTransactionFixture(t *testing.T) *transactionFixture {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "flock", "timeout", "rsync"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("shell transaction tests require %s", tool)
		}
	}
	root := t.TempDir()
	f := &transactionFixture{t: t, root: root, target: filepath.Join(root, "project with ' quotes $dollar"), home: filepath.Join(root, "home"), bin: filepath.Join(root, "bin"), script: filepath.Join(root, "transaction.sh")}
	f.write(f.script, imageTransactionScript, 0700)
	f.write(filepath.Join(f.bin, "podman"), fakePodman, 0700)
	f.write(filepath.Join(f.bin, "systemctl"), fakeSystemctl, 0700)
	f.write(filepath.Join(f.bin, "curl"), fakeCurl, 0700)
	f.write(filepath.Join(f.bin, "generator"), fakeGenerator, 0700)
	f.write(filepath.Join(f.bin, "mv"), fakeMV, 0700)
	f.write(filepath.Join(f.bin, "sync"), fakeSync, 0700)
	f.write(filepath.Join(root, "registry-image"), imageA, 0600)
	return f
}
func (f *transactionFixture) write(p, s string, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		f.t.Fatal(err)
	}
}
func (f *transactionFixture) read(p string) string {
	f.t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}
func (f *transactionFixture) unit() string {
	return filepath.Join(f.home, ".config/containers/systemd/pdf.container")
}
func (f *transactionFixture) state() string { return filepath.Join(f.target, ".deploy/image") }
func (f *transactionFixture) mark(s string) { f.write(filepath.Join(f.root, s), "1", 0600) }
func (f *transactionFixture) stage(id string, artifacts map[string]string, syncEnv bool) string {
	f.t.Helper()
	stage := filepath.Join(f.state(), "staging", id)
	req := imageReleaseRequest{Schema: 1, ReleaseID: id, Version: "v1", Commit: "abc", Service: "pdf", Target: f.target, Reference: "docker.io/example/pdf:mutable", HealthCommand: true, HealthURL: "http://127.0.0.1/health", HealthSeconds: 0.7, SyncEnv: syncEnv}
	if syncEnv {
		req.EnvFile = filepath.Join(f.target, ".env")
	}
	body, err := json.Marshal(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(stage, "request.json"), string(body), 0600)
	f.write(filepath.Join(stage, "candidate.container"), "[Unit]\nDescription="+id+"\n[Container]\nImage="+imagePlaceholder+"\nPull=never\n[Service]\nRestart=on-failure\n", 0600)
	if err := os.MkdirAll(filepath.Join(stage, "artifacts"), 0700); err != nil {
		f.t.Fatal(err)
	}
	for p, v := range artifacts {
		f.write(filepath.Join(stage, "artifacts", p), v, 0640)
	}
	if syncEnv {
		f.write(filepath.Join(stage, "env"), "SECRET="+id, 0600)
	}
	return stage
}
func (f *transactionFixture) command(mode, stage string, args ...string) *exec.Cmd {
	a := []string{"-c", `echo $$ > "$FAKE_ROOT/txpid"; exec bash "$@"`, "--", f.script, mode, stage}
	a = append(a, args...)
	c := exec.Command("bash", a...)
	c.Env = append(os.Environ(), "HOME="+f.home, "XDG_CONFIG_HOME="+filepath.Join(f.home, ".config"), "PATH="+f.bin+":"+os.Getenv("PATH"), "FAKE_ROOT="+f.root, "DEPLOY_TEST_GENERATOR="+filepath.Join(f.bin, "generator"))
	return c
}
func (f *transactionFixture) run(mode, stage string, args ...string) (string, error) {
	out, err := f.command(mode, stage, args...).CombinedOutput()
	return string(out), err
}
func (f *transactionFixture) success(stage string) {
	f.t.Helper()
	if out, err := f.run("release", stage); err != nil {
		f.t.Fatalf("transaction failed: %v\n%s", err, out)
	}
}
func (f *transactionFixture) current() string {
	var m map[string]any
	if err := json.Unmarshal([]byte(f.read(filepath.Join(f.state(), "current.json"))), &m); err != nil {
		f.t.Fatal(err)
	}
	return m["current"].(string)
}
func (f *transactionFixture) assertAbsent(p string) {
	f.t.Helper()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		f.t.Fatalf("expected absent %s: %v", p, err)
	}
}
func (f *transactionFixture) assertOld(unit string) {
	f.t.Helper()
	if got := f.read(f.unit()); got != unit {
		f.t.Fatalf("Quadlet not restored:\n%s", got)
	}
	if f.current() != "A" {
		f.t.Fatal("wrong commit")
	}
	if f.read(filepath.Join(f.root, "running-image")) != imageA {
		f.t.Fatal("wrong running image")
	}
	f.assertAbsent(filepath.Join(f.state(), "pending.json"))
}

func TestImageTransactionInstallAndMutableUpdate(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(f.stage("A", nil, false))
	if f.current() != "A" || !strings.Contains(f.read(f.unit()), imageA) {
		t.Fatal("first release not pinned")
	}
	f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
	f.success(f.stage("B", nil, false))
	if f.current() != "B" || !strings.Contains(f.read(f.unit()), imageB) {
		t.Fatal("mutable tag not resolved on each release")
	}
	var commit map[string]any
	json.Unmarshal([]byte(f.read(filepath.Join(f.state(), "current.json"))), &commit)
	if commit["previous"] != "A" {
		t.Fatal(commit)
	}
	f.write(filepath.Join(f.root, "registry-image"), imageA, 0600)
	if f.read(filepath.Join(f.root, "running-image")) != imageB {
		t.Fatal("tag mutation changed running image")
	}
	prune := exec.Command(filepath.Join(f.bin, "podman"), "image", "prune", "-f")
	prune.Env = f.command("release", "unused").Env
	if out, err := prune.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	for _, image := range []string{imageA, imageB} {
		if _, err := os.Stat(filepath.Join(f.root, "images", image)); err != nil {
			t.Fatal("retained image pruned", err)
		}
	}
	for _, id := range []string{"A", "B"} {
		m := map[string]any{}
		json.Unmarshal([]byte(f.read(filepath.Join(f.state(), "releases", id, "manifest.json"))), &m)
		if _, err := os.Stat(filepath.Join(f.root, "tags", strings.ReplaceAll(m["retention_tag"].(string), "/", "_"))); err != nil {
			t.Fatal("image not retained", err)
		}
	}
}
func TestImagePreactivationFailuresKeepService(t *testing.T) {
	for _, point := range []string{"fail-pull", "fail-auth", "fail-inspect", "fail-platform", "fail-generator", "missing-image", "unmanaged", "quadlet-drift", "drift", "missing-env", "fail-tag"} {
		t.Run(point, func(t *testing.T) {
			f := newTransactionFixture(t)
			if point == "unmanaged" {
				f.write(f.unit(), "unmanaged", 0600)
			} else {
				f.success(f.stage("A", nil, false))
			}
			var old string
			if point != "unmanaged" {
				old = f.read(f.unit())
			}
			stage := f.stage("B", nil, false)
			switch point {
			case "missing-image":
				os.Remove(filepath.Join(f.root, "images", imageA))
			case "quadlet-drift":
				f.write(f.unit(), old+"# unmanaged change\n", 0600)
				old = f.read(f.unit())
			case "drift":
				f.write(filepath.Join(f.root, "running-image"), imageB, 0600)
			case "missing-env":
				var r map[string]any
				json.Unmarshal([]byte(f.read(filepath.Join(stage, "request.json"))), &r)
				r["env_file"] = filepath.Join(f.target, "missing-env")
				b, _ := json.Marshal(r)
				f.write(filepath.Join(stage, "request.json"), string(b), 0600)
			default:
				f.mark(point)
			}
			before, _ := os.ReadFile(filepath.Join(f.root, "calls"))
			out, err := f.run("release", stage)
			if err == nil {
				t.Fatal("expected failure", out)
			}
			after := f.read(filepath.Join(f.root, "calls"))
			calls := strings.TrimPrefix(after, string(before))
			if strings.Contains(calls, "systemctl stop") || strings.Contains(calls, "systemctl restart") || strings.Contains(calls, "daemon-reload") {
				t.Fatal("changed service before activation", calls)
			}
			if point == "unmanaged" {
				if f.read(f.unit()) != "unmanaged" || strings.Contains(calls, "podman pull") {
					t.Fatal(calls)
				}
			} else if f.read(f.unit()) != old {
				t.Fatal("changed unit")
			}
		})
	}
}
func TestImageRollbackRestoresManagedFiles(t *testing.T) {
	for _, point := range []string{"fail-health", "fail-host-health", "fail-restart", "fail-reload", "fail-install", "wrong-image"} {
		t.Run(point, func(t *testing.T) {
			f := newTransactionFixture(t)
			f.write(filepath.Join(f.target, "assets", "untouched"), "user data", 0644)
			f.write(filepath.Join(f.target, "assets", "config.deploy-install"), "unmanaged sibling", 0600)
			f.success(f.stage("A", map[string]string{"assets/config": "old", "old-only": "keep"}, true))
			old := f.read(f.unit())
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark(point)
			stage := f.stage("B", map[string]string{"assets/config": "new", "assets/new-file": "new", "new-dir/file": "new"}, true)
			out, err := f.run("release", stage)
			if err == nil {
				t.Fatal("expected rollback", out)
			}
			f.assertOld(old)
			if f.read(filepath.Join(f.target, "assets/config")) != "old" || f.read(filepath.Join(f.target, ".env")) != "SECRET=A" || f.read(filepath.Join(f.target, "assets/untouched")) != "user data" {
				t.Fatal("files not restored")
			}
			if f.read(filepath.Join(f.target, "assets/config.deploy-install")) != "unmanaged sibling" {
				t.Fatal("unmanaged sibling overwritten")
			}
			f.assertAbsent(filepath.Join(f.target, "assets/new-file"))
			f.assertAbsent(filepath.Join(f.target, "new-dir"))
			if strings.Contains(out, "SECRET=") {
				t.Fatal("environment leaked")
			}
		})
	}
}
func TestFailedFirstInstallAndInactiveRollback(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		f := newTransactionFixture(t)
		f.mark("fail-health")
		f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
		stage := f.stage("B", map[string]string{"new/file": "candidate"}, true)
		if out, err := f.run("release", stage); err == nil {
			t.Fatal(out)
		}
		f.assertAbsent(f.unit())
		f.assertAbsent(filepath.Join(f.target, ".env"))
		f.assertAbsent(filepath.Join(f.target, "new"))
		f.assertAbsent(filepath.Join(f.state(), "current.json"))
		f.assertAbsent(filepath.Join(f.state(), "pending.json"))
	})
	t.Run("inactive", func(t *testing.T) {
		f := newTransactionFixture(t)
		f.success(f.stage("A", nil, false))
		os.Remove(filepath.Join(f.root, "active"))
		f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
		f.mark("fail-health")
		if out, err := f.run("release", f.stage("B", nil, false)); err == nil {
			t.Fatal(out)
		}
		f.assertAbsent(filepath.Join(f.root, "active"))
		if f.current() != "A" {
			t.Fatal("old commit not restored")
		}
	})
}
func TestPendingRecoveryBeforeAndAfterCommit(t *testing.T) {
	for _, point := range []string{"kill-before-commit", "kill-after-commit", "kill-after-unit"} {
		t.Run(point, func(t *testing.T) {
			f := newTransactionFixture(t)
			f.success(f.stage("A", map[string]string{"config": "old"}, true))
			old := f.read(f.unit())
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark(point)
			if out, err := f.run("release", f.stage("B", map[string]string{"config": "new"}, true)); err == nil {
				t.Fatal("expected kill", out)
			}
			if _, err := os.Stat(filepath.Join(f.state(), "pending.json")); err != nil {
				t.Fatal("pending missing", err)
			}
			stage := f.stage("action", nil, false)
			if out, err := f.run("action", stage, "stop"); err != nil {
				t.Fatal(err, out)
			}
			want := "A"
			if point == "kill-after-commit" {
				want = "B"
			}
			if f.current() != want {
				t.Fatalf("wanted %s got %s", want, f.current())
			}
			if want == "A" && f.read(f.unit()) != old {
				t.Fatal("old quadlet not recovered")
			}
			f.assertAbsent(filepath.Join(f.state(), "pending.json"))
		})
	}
}
func TestRollbackFailurePreservesPending(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(f.stage("A", nil, false))
	f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
	f.mark("fail-health")
	f.mark("fail-stop")
	out, err := f.run("release", f.stage("B", nil, false))
	if err == nil || !strings.Contains(out, "ROLLBACK FAILED") || !strings.Contains(out, "Readiness deadline") {
		t.Fatal(err, out)
	}
	if _, err := os.Stat(filepath.Join(f.state(), "pending.json")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(f.root, "fail-stop"))
	if out, err := f.run("action", f.stage("action", nil, false), "stop"); err != nil {
		t.Fatal(err, out)
	}
	if f.current() != "A" {
		t.Fatal("recovery failed")
	}
}
func TestServiceLockAcrossTargetDirectories(t *testing.T) {
	f := newTransactionFixture(t)
	stage := f.stage("A", nil, false)
	locks := filepath.Join(f.home, ".local/state/deploy/locks")
	if err := os.MkdirAll(locks, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(locks, serviceLockName("pdf")+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"release", "action"} {
		out, err := f.run(mode, stage, "restart")
		if err == nil || !strings.Contains(out, "lock is busy") {
			t.Fatal(err, out)
		}
	}
	f.target = filepath.Join(f.root, "other-project")
	stage = f.stage("B", nil, false)
	if out, err := f.run("release", stage); err == nil || !strings.Contains(out, "lock is busy") {
		t.Fatal(err, out)
	}
}
func TestManagedSymlinkRejected(t *testing.T) {
	f := newTransactionFixture(t)
	outside := filepath.Join(f.root, "outside")
	f.write(filepath.Join(outside, "config"), "safe", 0600)
	os.MkdirAll(f.target, 0700)
	if err := os.Symlink(outside, filepath.Join(f.target, "assets")); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run("release", f.stage("A", map[string]string{"assets/config": "bad"}, false)); err == nil {
		t.Fatal(out)
	}
	if f.read(filepath.Join(outside, "config")) != "safe" {
		t.Fatal("followed symlink")
	}
}
func TestHealthDeadlineBoundsHangingProbe(t *testing.T) {
	f := newTransactionFixture(t)
	f.mark("hang-probe")
	f.mark("active")
	f.write(filepath.Join(f.root, "running-image"), imageB, 0600)
	c := exec.Command("timeout", "-s", "KILL", "0.3", "bash", f.script, "health", "pdf", imageB, "true", "")
	c.Env = f.command("release", "unused").Env
	started := time.Now()
	if err := c.Run(); err == nil {
		t.Fatal("hanging probe accepted")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("deadline exceeded")
	}
}
func TestInstalledQuadletGenerator(t *testing.T) {
	generator := "/usr/lib/systemd/system-generators/podman-system-generator"
	if _, err := os.Stat(generator); err != nil {
		t.Skip("Quadlet generator unavailable")
	}
	dir := t.TempDir()
	c := imageConfig()
	changeEnv(&c, func(e *Environment) {
		e.Quadlet.CPU = "0.5"
		e.Quadlet.Memory = "1G"
		e.Quadlet.HealthCmd = "curl --fail http://127.0.0.1:3000/health"
		e.Quadlet.NetworkAliases = []string{"pdf-prod"}
		e.Quadlet.AutoRestart = true
	})
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := renderQuadlet(n, imageA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdf.container"), []byte(unit), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(generator, "--user", "--dryrun")
	cmd.Env = append(os.Environ(), "QUADLET_UNIT_DIRS="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(out))
	}
	for _, s := range []string{"---pdf.service---", "--pull never", "--health-cmd", "CPUQuota=50%", "MemoryMax=1G", "--network-alias pdf-prod"} {
		if !bytes.Contains(out, []byte(s)) {
			t.Fatalf("missing %s: %s", s, out)
		}
	}
}

const fakePodman = `#!/bin/bash
set -eu
echo "podman $*" >> "$FAKE_ROOT/calls"
case "$1 $2" in
 'build --iidfile')
  if [ -f "$FAKE_ROOT/fail-build" ]; then rm "$FAKE_ROOT/fail-build"; exit 1; fi
  image=$(cat "$FAKE_ROOT/registry-image")
  mkdir -p "$FAKE_ROOT/images" "$FAKE_ROOT/tags"; touch "$FAKE_ROOT/images/$image"
  printf '%s' "$image" > "$3"
  printf '%s' "$image" > "$FAKE_ROOT/tags/${7//\//_}"
  if [[ -f $FAKE_ROOT/mutate-build-tag ]]; then
   printf 'sha256:%064d' 0 > "$FAKE_ROOT/tags/${7//\//_}"
  fi;;
 'pull --quiet')
  [ ! -f "$FAKE_ROOT/fail-pull" ] || exit 125
  [ ! -f "$FAKE_ROOT/fail-auth" ] || { echo 'Registry access denied' >&2; exit 125; }
  if [ -f "$FAKE_ROOT/hold-pull" ]; then
   touch "$FAKE_ROOT/pull-entered"
   for i in {1..100}; do [ ! -f "$FAKE_ROOT/allow-pull" ] || break; sleep .05; done
   [ -f "$FAKE_ROOT/allow-pull" ] || exit 1
  fi
  image=$(cat "$FAKE_ROOT/registry-image")
  mkdir -p "$FAKE_ROOT/images"; touch "$FAKE_ROOT/images/$image"
  printf '%s\n' "$image";;
 'image exists') [ -f "$FAKE_ROOT/images/$3" ];;
 'image prune') for file in "$FAKE_ROOT/images/"*; do image=${file##*/}; if ! grep -r -q -F "$image" "$FAKE_ROOT/tags"; then rm -f "$file"; fi; done;;
 'image inspect')
  [ ! -f "$FAKE_ROOT/fail-inspect" ] || exit 1
  arch=amd64; [ ! -f "$FAKE_ROOT/fail-platform" ] || arch=arm64
  image=$3
  if [[ $image != sha256:* ]]; then image=$(cat "$FAKE_ROOT/tags/${image//\//_}"); fi
  jq -n --arg id "$image" --arg arch "$arch" '[{Id:$id,Os:"linux",Architecture:$arch,RepoDigests:["docker.io/example/pdf@sha256:abc"]}]';;
 'container inspect') cat "$FAKE_ROOT/running-image";;
 'healthcheck run')
  if [ -f "$FAKE_ROOT/hang-probe" ]; then sleep 10; fi
  if [ -f "$FAKE_ROOT/fail-health" ] && grep -q bbbbbbbb "$FAKE_ROOT/running-image"; then exit 1; fi;;
 'info --format') case "$3" in *Store.GraphRoot*) echo "$FAKE_ROOT/images";; *Host.OS*) echo linux;; *Host.Arch*) echo amd64;; *Host.CgroupControllers*) if [ -f "$FAKE_ROOT/missing-controllers" ]; then echo '["pids"]'; else echo '["cpu","memory","pids"]'; fi;; esac;;
 *) if [ "$1" = tag ]; then [ ! -f "$FAKE_ROOT/fail-tag" ] || exit 1; mkdir -p "$FAKE_ROOT/tags"; printf '%s' "$2" > "$FAKE_ROOT/tags/${3//\//_}"; else echo 'unexpected podman invocation' >&2; exit 1; fi;;
esac
`
const fakeSystemctl = `#!/bin/bash
set -eu
shift
echo "systemctl $*" >> "$FAKE_ROOT/calls"
unit="$HOME/.config/containers/systemd/pdf.container"
case "$1" in
 show-environment) :;;
 is-active) [ -f "$FAKE_ROOT/active" ] || exit 3;;
 cat) [ -f "$unit" ];;
 stop) [ ! -f "$FAKE_ROOT/fail-stop" ] || exit 1; rm -f "$FAKE_ROOT/active";;
 daemon-reload) if [ -f "$FAKE_ROOT/fail-reload" ]; then rm "$FAKE_ROOT/fail-reload"; exit 1; fi;;
 start|restart)
  if [ -f "$FAKE_ROOT/fail-restart" ]; then rm "$FAKE_ROOT/fail-restart"; exit 1; fi
  sed -n 's/^Image=//p' "$unit" | tr -d '\n' > "$FAKE_ROOT/running-image"
  if [ -f "$FAKE_ROOT/wrong-image" ]; then rm "$FAKE_ROOT/wrong-image"; sed -i 's/bbbbbbbb/cccccccc/' "$FAKE_ROOT/running-image"; fi
  touch "$FAKE_ROOT/active";;
 enable|disable) :;;
 *) echo 'unexpected systemctl invocation' >&2; exit 1;;
esac
`
const fakeSync = `#!/bin/bash
set -eu
echo "sync $*" >> "$FAKE_ROOT/calls"
if [[ -f $FAKE_ROOT/fail-pending-sync && ${!#} == */pending.json.tmp ]]; then rm "$FAKE_ROOT/fail-pending-sync"; exit 1; fi
if [[ -f $FAKE_ROOT/fail-target-sync && ${!#} == *'project with '* ]]; then
 case "${!#}" in */.deploy/*) ;; *) rm "$FAKE_ROOT/fail-target-sync"; exit 1;; esac
fi
`
const fakeGenerator = `#!/bin/bash
set -eu
[ ! -f "$FAKE_ROOT/fail-generator" ] || { echo 'invalid Quadlet' >&2; exit 1; }
printf '[Service]\n' > "$2/pdf.service"
`
const fakeMV = `#!/bin/bash
set -eu
dest=${!#}
if [[ $dest == */app.db && $* == *deploy-install* ]]; then
 if [[ -f $FAKE_ROOT/fail-db-install ]]; then rm "$FAKE_ROOT/fail-db-install"; exit 1; fi
 if [[ -f $FAKE_ROOT/kill-db-after-install ]]; then
  /usr/bin/mv "$@"; rm "$FAKE_ROOT/kill-db-after-install"; kill -KILL "$(cat "$FAKE_ROOT/txpid")"; exit 0
 fi
fi
if [[ $dest == */pdf.container && $* == *deploy-install* ]]; then
 if [ -f "$FAKE_ROOT/fail-install" ]; then rm "$FAKE_ROOT/fail-install"; exit 1; fi
 /usr/bin/mv "$@"
 if [ -f "$FAKE_ROOT/term-after-unit" ]; then rm "$FAKE_ROOT/term-after-unit"; kill -TERM "$(cat "$FAKE_ROOT/txpid")"; fi
 if [ -f "$FAKE_ROOT/kill-after-unit" ]; then rm "$FAKE_ROOT/kill-after-unit"; kill -KILL "$(cat "$FAKE_ROOT/txpid")"; fi
 exit 0
fi
if [[ $dest == */boot.json && -f $FAKE_ROOT/kill-boot ]]; then
 /usr/bin/mv "$@"; rm "$FAKE_ROOT/kill-boot"; kill -KILL "$(cat "$FAKE_ROOT/txpid")"; exit 0
fi
if [[ $dest == */current.json ]]; then
 if [ -f "$FAKE_ROOT/kill-before-commit" ]; then rm "$FAKE_ROOT/kill-before-commit"; kill -KILL "$(cat "$FAKE_ROOT/txpid")"; sleep .1; exit 1; fi
 /usr/bin/mv "$@"
 if [ -f "$FAKE_ROOT/kill-after-commit" ]; then rm "$FAKE_ROOT/kill-after-commit"; kill -KILL "$(cat "$FAKE_ROOT/txpid")"; fi
 exit 0
fi
exec /usr/bin/mv "$@"
`

func Example_serviceLockName() {
	fmt.Println(serviceLockName("pdf") == serviceLockName("pdf")) // Output: true
}

func TestImageBootActionsPersistAndRecover(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(f.stage("A", nil, false))
	record := filepath.Join(f.state(), "boot.json")
	override := filepath.Join(filepath.Dir(f.unit()), "pdf.container.d", "90-deploy-boot.conf")
	if out, err := f.run("action", f.stage("disable", nil, false), "disable"); err != nil {
		t.Fatal(err, out)
	}
	if f.read(override) != "[Install]\nWantedBy=\n" || !strings.Contains(f.read(record), `"enabled": false`) {
		t.Fatal("boot disable was not persisted")
	}
	// A new deployment must preserve the administrator's boot setting.
	f.success(f.stage("B", nil, false))
	if f.read(override) != "[Install]\nWantedBy=\n" {
		t.Fatal("release lost disabled boot setting")
	}
	f.mark("fail-reload")
	if out, err := f.run("action", f.stage("enable-fails", nil, false), "enable"); err == nil {
		t.Fatal(out)
	}
	if f.read(override) != "[Install]\nWantedBy=\n" {
		t.Fatal("failed enable did not restore previous boot setting")
	}
	f.assertAbsent(filepath.Join(f.state(), "boot-pending.json"))
	f.mark("kill-boot")
	if out, err := f.run("action", f.stage("enable-killed", nil, false), "enable"); err == nil {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(f.state(), "boot-pending.json")); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run("action", f.stage("recover-boot", nil, false), "stop"); err != nil {
		t.Fatal(err, out)
	}
	if f.read(override) != "[Install]\nWantedBy=\n" {
		t.Fatal("interrupted enable was not recovered")
	}
	if out, err := f.run("action", f.stage("enable", nil, false), "enable"); err != nil {
		t.Fatal(err, out)
	}
	if f.read(override) != "[Install]\nWantedBy=\nWantedBy=default.target\n" {
		t.Fatal("enable did not restore WantedBy")
	}
	calls := f.read(filepath.Join(f.root, "calls"))
	if strings.Contains(calls, "systemctl enable") || strings.Contains(calls, "systemctl disable") {
		t.Fatal("used unsupported generated-unit action", calls)
	}
}

func TestFailedFirstMutationDoesNotRestartService(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(f.stage("A", nil, false))
	old := f.read(f.unit())
	before := f.read(filepath.Join(f.root, "calls"))
	f.mark("fail-install")
	if out, err := f.run("release", f.stage("B", nil, false)); err == nil {
		t.Fatal(out)
	}
	calls := strings.TrimPrefix(f.read(filepath.Join(f.root, "calls")), before)
	for _, bad := range []string{"systemctl stop", "systemctl restart", "daemon-reload"} {
		if strings.Contains(calls, bad) {
			t.Fatal("restarted service despite no active change", calls)
		}
	}
	f.assertOld(old)
}

func TestPendingCannotBeBypassedThroughAnotherTarget(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(f.stage("A", nil, false))
	f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
	f.mark("kill-after-unit")
	if out, err := f.run("release", f.stage("B", nil, false)); err == nil {
		t.Fatal(out)
	}
	before := f.read(filepath.Join(f.root, "calls"))
	originalTarget := f.target
	f.target = filepath.Join(f.root, "different-project")
	if out, err := f.run("action", f.stage("wrong-project", nil, false), "restart"); err == nil || !strings.Contains(out, "another target_dir") {
		t.Fatal(err, out)
	}
	if f.read(filepath.Join(f.root, "calls")) != before {
		t.Fatal("bypassed pending recovery")
	}
	f.target = originalTarget
	if out, err := f.run("action", f.stage("right-project", nil, false), "stop"); err != nil {
		t.Fatal(err, out)
	}
	if f.current() != "A" {
		t.Fatal("pending state did not recover")
	}
}

const fakeCurl = `#!/bin/bash
set -eu
echo 'curl host-check' >> "$FAKE_ROOT/calls"
if [ -f "$FAKE_ROOT/fail-host-health" ] && grep -q bbbbbbbb "$FAKE_ROOT/running-image"; then exit 1; fi
`

func TestConcurrentImageReleasesAndServiceAction(t *testing.T) {
	f := newTransactionFixture(t)
	f.mark("hold-pull")
	first := f.command("release", f.stage("A", nil, false))
	var output bytes.Buffer
	first.Stdout = &output
	first.Stderr = &output
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- first.Wait() }()
	t.Cleanup(func() { f.mark("allow-pull") })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.root, "pull-entered")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			first.Process.Kill()
			<-done
			t.Fatal("first transaction did not enter pull")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, mode := range []string{"release", "action"} {
		out, err := f.run(mode, f.stage("second-"+mode, nil, false), "restart")
		if err == nil || !strings.Contains(out, "lock is busy") {
			t.Fatal(err, out)
		}
	}
	f.mark("allow-pull")
	if err := <-done; err != nil {
		t.Fatal(err, output.String())
	}
	if f.current() != "A" {
		t.Fatal("wrong release committed")
	}
}

func TestTerminatedTransactionRollsBack(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(f.stage("A", nil, false))
	old := f.read(f.unit())
	f.mark("term-after-unit")
	out, err := f.run("release", f.stage("B", nil, false))
	if err == nil || !strings.Contains(out, "previous state restored") {
		t.Fatal(err, out)
	}
	f.assertOld(old)
}

func TestResourcePrerequisites(t *testing.T) {
	f := newTransactionFixture(t)
	run := func(q Quadlet) error {
		c := exec.Command("bash", "-c", imageResourceChecks(q))
		c.Env = f.command("release", "unused").Env
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
	if err := run(Quadlet{CPU: "1", Memory: "1G"}); err != nil {
		t.Fatal(err)
	}
	f.mark("missing-controllers")
	for _, q := range []Quadlet{{CPU: "1"}, {Memory: "1G"}} {
		if err := run(q); err == nil || !strings.Contains(err.Error(), "delegated cgroup") {
			t.Fatal("missing resource controller accepted", err)
		}
	}
	if err := run(Quadlet{}); err != nil {
		t.Fatal("unlimited service requires resource controllers", err)
	}
}
