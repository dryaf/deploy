package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStrictYAMLConfiguration(t *testing.T) {
	for _, server := range []bool{false, true} {
		for name, body := range map[string]string{
			"unknown": "typo: true\n", "empty": "# empty\n", "duplicate": "host: a\nhost: b\n", "extra": "host: a\n---\nhost: b\n", "trailing": "host: a\n---\n[invalid\n",
		} {
			t.Run(name+map[bool]string{false: "-deploy", true: "-server"}[server], func(t *testing.T) {
				var dst any = &Config{}
				if server {
					dst = &ServerConfig{}
				}
				if err := decodeConfig("test.yaml", []byte(body), dst); err == nil {
					t.Fatal("invalid YAML accepted")
				}
			})
		}
	}
	var cfg Config
	if err := decodeConfig("deploy.yaml", []byte("environments:\n  prod:\n    traefik:\n      email: ignored\n"), &cfg); err == nil || !strings.Contains(err.Error(), "previously ignored") {
		t.Fatal(err)
	}
	body := []byte("app_name: example\nenvironments:\n  common: &env\n    host: example.com\n    user: deploy\n  prod:\n    <<: *env\n")
	if err := decodeConfig("deploy.yaml", body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Environments["prod"].Host != "example.com" {
		t.Fatal("merge lost")
	}
}

func TestTagIdentityRequiresMatchingObjects(t *testing.T) {
	for _, kind := range []string{"lightweight", "annotated", "changed-annotation"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			git := func(args ...string) string {
				t.Helper()
				out, err := exec.Command("git", args...).CombinedOutput()
				if err != nil {
					t.Fatal(string(out), err)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "--bare", "remote.git")
			git("init", "work")
			t.Chdir(filepath.Join(root, "work"))
			git("config", "user.email", "test@example.invalid")
			git("config", "user.name", "Test")
			if err := os.WriteFile("source", []byte("A"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "source")
			git("commit", "-m", "A")
			if kind == "lightweight" {
				git("tag", "v1")
			} else {
				git("tag", "-a", "v1", "-m", "original")
			}
			git("remote", "add", "origin", filepath.Join(root, "remote.git"))
			git("push", "origin", "refs/tags/v1")
			if err := ensureTagPushed("v1"); err != nil {
				t.Fatal(err)
			}
			if kind != "changed-annotation" {
				os.WriteFile("source", []byte("B"), 0600)
				git("commit", "-am", "B")
			}
			if kind == "lightweight" {
				git("tag", "-f", "v1")
			} else {
				git("tag", "-f", "-a", "v1", "-m", "changed")
			}
			if err := ensureTagPushed("v1"); err == nil || !strings.Contains(err.Error(), "differs") {
				t.Fatal(err)
			}
			if git("rev-parse", "refs/tags/v1") == strings.Fields(git("ls-remote", "--tags", "origin", "refs/tags/v1"))[0] {
				t.Fatal("fixture failed to diverge")
			}
		})
	}
}

type capturedPayloadRunner struct {
	fakeRunner
	t               *testing.T
	version, binary string
}

func (r *capturedPayloadRunner) Sync(_ Environment, sources []string, _ string, _ ...string) error {
	for _, source := range sources {
		if filepath.Base(source) == "request.json" {
			body, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			var req imageReleaseRequest
			if err = json.Unmarshal(body, &req); err != nil {
				return err
			}
			r.version = req.Version
		}
		if strings.HasSuffix(source, "/") && strings.Contains(source, "deploy-payload-") {
			if err := os.WriteFile("build/server", []byte("B"), 0755); err != nil {
				return err
			}
			body, err := os.ReadFile(filepath.Join(source, "artifacts/server"))
			if err != nil {
				return err
			}
			r.binary = string(body)
		}
	}
	return nil
}

func TestReleaseUploadUsesCapturedBinary(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(f.root)
	f.write("Dockerfile.vps", "FROM scratch", 0600)
	p := safetyBinaryPlan(t, f, []string{"Dockerfile.vps"}, "true")
	p.Version = "A"
	r := &capturedPayloadRunner{t: t, fakeRunner: fakeRunner{local: func(*exec.Cmd) error { return os.WriteFile("build/server", []byte("A"), 0755) }}}
	if err := executeBinaryRelease(p, r); err != nil {
		t.Fatal(err)
	}
	if r.binary != "A" || r.version != "A" {
		t.Fatalf("uploaded %q with version %q", r.binary, r.version)
	}
}

func TestConcurrentBuildCannotReplaceOutput(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(f.root)
	f.write("Dockerfile.vps", "FROM scratch", 0600)
	p := safetyBinaryPlan(t, f, []string{"Dockerfile.vps"}, "true")
	entered, release := make(chan struct{}), make(chan struct{})
	first := &fakeRunner{local: func(*exec.Cmd) error {
		close(entered)
		<-release
		return os.WriteFile("build/server", []byte("A"), 0755)
	}}
	done := make(chan error, 1)
	go func() { done <- executeBinaryRelease(p, first) }()
	<-entered
	second := &fakeRunner{local: func(*exec.Cmd) error { t.Error("second build ran despite project lock"); return nil }}
	err := executeBinaryRelease(p, second)
	close(release)
	if firstErr := <-done; firstErr != nil {
		t.Fatal(firstErr)
	}
	if err == nil || !strings.Contains(err.Error(), "lock busy") {
		t.Fatal(err)
	}
}

func TestLocalLockWorker(t *testing.T) {
	if os.Getenv("DEPLOY_LOCK_WORKER") != "1" {
		return
	}
	lock, err := acquireLocalLock("database", os.Getenv("DEPLOY_LOCK_TARGET"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	os.Stdout.Write([]byte("ready\n"))
	select {}
}

func TestLocalLockReleasedAfterProcessDeath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "app.db")
	executable, _ := os.Executable()
	cmd := exec.Command(executable, "-test.run=^TestLocalLockWorker$")
	cmd.Env = append(os.Environ(), "DEPLOY_LOCK_WORKER=1", "DEPLOY_LOCK_TARGET="+target)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	ready := make([]byte, 6)
	if _, err = pipe.Read(ready); err != nil {
		t.Fatal(err)
	}
	if lock, err := acquireLocalLock("database", target); err == nil {
		lock.Close()
		t.Fatal("concurrent process acquired lock")
	}
	cmd.Process.Kill()
	cmd.Wait()
	lock, err := acquireLocalLock("database", target)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestBoundedProcessKillsChildren(t *testing.T) {
	cmd := exec.Command("bash", "-c", "trap '' TERM; sleep 30 & wait")
	started := time.Now()
	err := runBoundedContext(context.Background(), cmd, 50*time.Millisecond, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("subprocess group outlived deadline")
	}
}

func TestHealthPolicyValidation(t *testing.T) {
	c := imageConfig()
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if n.HealthStableFor != 5*time.Second || n.HealthInterval != time.Second || n.HealthSuccesses != 3 {
		t.Fatal(n)
	}
	changeEnv(&c, func(e *Environment) { e.Quadlet.HealthRequired = true })
	if _, err = normalizeAndValidateConfig(c, "prod"); err == nil {
		t.Fatal("required application health missing")
	}
}

func TestBinaryBuildFailureDoesNotStopOldService(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(stageBinaryFixture(f, "A", map[string]string{"server": "old"}, false))
	before := f.read(filepath.Join(f.root, "calls"))
	f.mark("fail-build")
	if out, err := f.run("release", stageBinaryFixture(f, "B", map[string]string{"server": "new"}, false)); err == nil {
		t.Fatal(out)
	}
	calls := strings.TrimPrefix(f.read(filepath.Join(f.root, "calls")), before)
	if strings.Contains(calls, "systemctl stop") || strings.Contains(calls, "systemctl restart") {
		t.Fatal(calls)
	}
	if f.read(filepath.Join(f.root, "running-image")) != imageA {
		t.Fatal("old service changed")
	}
	f.assertAbsent(filepath.Join(f.target, ".deploy/binary/pending.json"))
}

func TestPullConcurrencyWorker(t *testing.T) {
	if os.Getenv("DEPLOY_PULL_WORKER") != "A" {
		return
	}
	root := os.Getenv("DEPLOY_PULL_ROOT")
	t.Chdir(root)
	e := Environment{Name: "A", Host: "example.com", User: "deploy", Dir: filepath.Join(root, "remote-A"), Quadlet: Quadlet{ServiceName: "pdf"}, Database: DatabaseConfig{Driver: "sqlite", Source: "app.db"}}
	if err := pullDatabase(e); err != nil {
		t.Fatal(err)
	}
}

func waitForTestFile(t *testing.T, file string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(file); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for barrier", file)
}

func TestConcurrentPullPreservesImmediatelyPreviousDatabase(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(f.root)
	setupFakeSSH(t, f)
	realSQLite := requireSQLite(t)
	createDatabase(t, "app.db", "original")
	for _, name := range []string{"A", "B"} {
		createDatabase(t, filepath.Join(f.root, "remote-"+name, "app.db"), name)
	}
	script := `#!/bin/bash
set -eu
if [[ ${DEPLOY_PULL_WORKER:-} == A && $* == *'.restore '* ]]; then
 touch "$DEPLOY_PULL_ROOT/restore-ready"
 for attempt in {1..1000}; do [[ ! -f $DEPLOY_PULL_ROOT/restore-allow ]] || break; sleep .01; done
 [[ -f $DEPLOY_PULL_ROOT/restore-allow ]] || exit 88
fi
exec ` + shellQuote(realSQLite) + ` "$@"
`
	f.write(filepath.Join(f.bin, "sqlite3"), script, 0700)
	executable, _ := os.Executable()
	cmd := exec.Command(executable, "-test.run=^TestPullConcurrencyWorker$")
	cmd.Env = append(os.Environ(), "DEPLOY_PULL_WORKER=A", "DEPLOY_PULL_ROOT="+f.root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	waitForTestFile(t, filepath.Join(f.root, "restore-ready"))
	e := Environment{Name: "B", Host: "example.com", User: "deploy", Dir: filepath.Join(f.root, "remote-B"), Quadlet: Quadlet{ServiceName: "pdf"}, Database: DatabaseConfig{Driver: "sqlite", Source: "app.db"}}
	if err := pullDatabase(e); err == nil || !strings.Contains(err.Error(), "database lock busy") {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "restore-allow"), "go", 0600)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if databaseValue(t, "app.db") != "A" {
		t.Fatal("first pull failed")
	}
	if err := pullDatabase(e); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob("backup/*.db")
	seen := map[string]bool{}
	for _, backup := range backups {
		seen[databaseValue(t, backup)] = true
	}
	if len(backups) != 2 || !seen["original"] || !seen["A"] || databaseValue(t, "app.db") != "B" {
		t.Fatal(backups, seen)
	}
}

func TestReadinessRejectsFlappingApplication(t *testing.T) {
	f := newTransactionFixture(t)
	f.mark("active")
	f.write(filepath.Join(f.root, "running-image"), imageA, 0600)
	f.write(filepath.Join(f.bin, "curl"), `#!/bin/bash
set -eu
count=0; [[ ! -f $FAKE_ROOT/probes ]] || count=$(cat "$FAKE_ROOT/probes")
(( count += 1 )); echo "$count" > "$FAKE_ROOT/probes"
(( count % 2 == 1 ))
`, 0700)
	cmd := f.command("health", "pdf", imageA, "false", "http://localhost/", "150", "0.01", "3")
	if err := runBoundedContext(context.Background(), cmd, 350*time.Millisecond, 20*time.Millisecond); err == nil {
		t.Fatal("one healthy probe committed readiness")
	}
}

func TestReadinessWaitsForStability(t *testing.T) {
	f := newTransactionFixture(t)
	f.mark("active")
	f.write(filepath.Join(f.root, "running-image"), imageA, 0600)
	cmd := f.command("health", "pdf", imageA, "false", "", "120", "0.01", "3")
	started := time.Now()
	if err := runBoundedContext(context.Background(), cmd, 2*time.Second, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 120*time.Millisecond {
		t.Fatal("stability interval skipped")
	}
}

func TestRecoveryHasIndependentBoundedProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("remote recovery watchdog targets Linux")
	}
	for _, tc := range []struct{ name, startupDelay string }{
		{"immediate", "0"},
		{"delayed-start", "1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", `sleep "$1"
source ./transaction_common.sh
recovery_seconds=.1; shutdown_grace=.05
hang() { printf 'recovery-started\n'; run_phase 30 bash -c 'trap "" TERM; sleep 30 & wait'; }
bounded_recovery hang
`, "recovery-watchdog-test", tc.startupDelay)
			// The recovery budget starts after shell startup. Require a signal
			// exit from the inner watchdog, rather than the outer guard's deadline.
			// Descendants retain the output pipes, so a leaked child would also
			// hit that guard instead of returning here.
			out, err := boundedOutput(cmd, 5*time.Second)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || (exitErr.ExitCode() != 143 && exitErr.ExitCode() != 137) {
				t.Fatalf("recovery watchdog did not terminate its worker: output=%q, error=%v", out, err)
			}
			if !strings.Contains(string(out), "recovery-started") || !strings.Contains(err.Error(), "pending state retained") {
				t.Fatalf("missing recovery deadline result: output=%q, error=%v", out, err)
			}
		})
	}
}

func TestBootGateRejectsPendingStateWithoutLiveTransaction(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(f.stage("A", nil, false))
	gate := filepath.Join(f.state(), "activation-check.sh")
	cmd := exec.Command("bash", gate)
	cmd.Env = f.command("release", "unused").Env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	f.write(filepath.Join(f.state(), "pending.json"), `{"release_id":"A"}`, 0600)
	cmd = exec.Command("bash", gate)
	cmd.Env = f.command("release", "unused").Env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "recovery before startup") {
		t.Fatal(string(out), err)
	}
}

func TestCapturedPayloadCleanupHandlesReadonlyDirectories(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir("readonly", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("readonly/config", []byte("content"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod("readonly", 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod("readonly", 0700) })
	c := imageConfig()
	c.Artifacts.Include = []string{"readonly"}
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	p, err := planImageRelease(n, "v1", "abc")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := captureRelease(p, "")
	if err != nil {
		t.Fatal(err)
	}
	payload.Close()
	if _, err = os.Lstat(payload.Dir); !os.IsNotExist(err) {
		t.Fatal("read-only payload was not cleaned up", err)
	}
}

func TestActivationGateEscapesSystemdDollarExpansion(t *testing.T) {
	c := imageConfig()
	changeEnv(&c, func(e *Environment) { e.Dir = "/srv/path with $literal" })
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := renderQuadlet(n, imagePlaceholder)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, `ExecStartPre=/bin/bash "/srv/path with $$literal/.deploy/image/activation-check.sh"`) {
		t.Fatal(unit)
	}
}

func TestReleaseVersionRejectsShellSubstitution(t *testing.T) {
	for _, version := range []string{"v1'; touch exploit", "v1$(id)", "v1`id`", "v1;id", "v1|id", "-v1"} {
		if err := validateReleaseVersion(version); err == nil {
			t.Fatal("unsafe metadata tag accepted", version)
		}
	}
	for _, version := range []string{"v1.2.3", "release/2026-10", "v1.0.0+build.1"} {
		if err := validateReleaseVersion(version); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMaintenanceGenerationPropagatesWriteFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "maintenance.html"), []byte("stale"), 0400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := generateMaintenance(Environment{Quadlet: Quadlet{ServiceName: "app"}}, filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing output directory accepted")
	}
	if _, _, err := generateMaintenance(Environment{Quadlet: Quadlet{ServiceName: "app"}}, root); err == nil {
		t.Fatal("stale read-only maintenance output accepted")
	}
}

func TestSQLiteSnapshotHonorsOperationBudget(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	createDatabase(t, "app.db", "source")
	tool := requireSQLite(t)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/bash\nif [[ $* == *'.backup '* ]]; then sleep 30; fi\nexec " + shellQuote(tool) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "sqlite3"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	started := time.Now()
	if err := sqliteSnapshot("app.db", "backup.db", 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("snapshot ignored the configured budget")
	}
}
