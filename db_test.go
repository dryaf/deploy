package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the production transport steps locally, with real rsync/files and fake
// service/container commands. No SSH connection is made by this runner.
type filesystemRunner struct {
	env             []string
	syncs, failSync int
}

func (r *filesystemRunner) SSH(_ Environment, script string) (string, error) {
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, out)
	}
	return string(out), nil
}
func (r *filesystemRunner) Sync(_ Environment, sources []string, dest string, options ...string) error {
	r.syncs++
	if r.syncs == r.failSync {
		return fmt.Errorf("injected transfer failure")
	}
	_, target, ok := strings.Cut(dest, ":")
	if !ok {
		return fmt.Errorf("bad destination")
	}
	args := append([]string{"-a"}, options...)
	args = append(args, "--")
	args = append(args, sources...)
	args = append(args, target)
	cmd := exec.Command("rsync", args...)
	cmd.Env = r.env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}
func (r *filesystemRunner) Local(cmd *exec.Cmd) error { return cmd.Run() }

func requireSQLite(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("requires SQLite CLI")
	}
	return p
}
func createDatabase(t *testing.T, path, value string) {
	t.Helper()
	tool := requireSQLite(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(tool, path, "CREATE TABLE sample(value TEXT); INSERT INTO sample VALUES ('"+value+"');").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
}
func databaseValue(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command(requireSQLite(t), "-readonly", path, "SELECT value FROM sample;").CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	return strings.TrimSpace(string(out))
}

func TestDatabaseDryRunsDoNotExecuteOrModifyFiles(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for name, value := range map[string]string{"app.db": "original", "app.db.bak": "backup", "app.db-wal": "wal", "app.db-shm": "shm", "app.db.temp_safecopy": "unrelated"} {
		if err := os.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := Environment{Dir: "/srv/app", Database: DatabaseConfig{Driver: "sqlite", Source: "app.db"}}
	dryRun = true
	defer func() { dryRun = false }()
	if err := pullDatabase(env); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{failAt: 1}
	if err := pushDatabase(env, ModeBinary, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatal(runner.calls)
	}
	for name, want := range map[string]string{"app.db": "original", "app.db.bak": "backup", "app.db-wal": "wal", "app.db-shm": "shm", "app.db.temp_safecopy": "unrelated"} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != want {
			t.Fatal(name, string(got), err)
		}
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 5 {
		t.Fatal(entries)
	}
}

func TestDatabasePullPublishesOnlyCompleteSnapshots(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			requireSQLite(t)
			remote := filepath.Join(root, "remote 'quoted \"path$")
			createDatabase(t, filepath.Join(remote, "app.db"), "remote")
			createDatabase(t, "app.db", "local")
			original, _ := os.ReadFile("app.db")
			bin := filepath.Join(root, "bin")
			os.Mkdir(bin, 0700)
			script := "#!/bin/bash\nexec bash -c \"${!#}\"\n"
			if failure {
				script = "#!/bin/bash\nprintf partial\nexit 255\n"
				os.WriteFile("app.db-wal", []byte("wal"), 0600)
				os.WriteFile("app.db-shm", []byte("shm"), 0600)
			}
			os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700)
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			env := Environment{User: "deploy", Host: "example.com", Dir: remote, Database: DatabaseConfig{Driver: "sqlite", Source: "app.db"}}
			err := pullDatabase(env)
			if failure {
				if err == nil {
					t.Fatal("download error ignored")
				}
				got, _ := os.ReadFile("app.db")
				if !bytes.Equal(original, got) {
					t.Fatal("local database changed")
				}
				for _, suffix := range []string{"-wal", "-shm"} {
					if _, err := os.Stat("app.db" + suffix); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if databaseValue(t, "app.db") != "remote" || databaseValue(t, "app.db.bak") != "local" {
				t.Fatal("database or backup wrong")
			}
		})
	}
}

func TestDatabasePushRequiresBackupAndRestoresFailures(t *testing.T) {
	for _, point := range []string{"success", "backup", "upload", "install", "status", "active"} {
		t.Run(point, func(t *testing.T) {
			f := newTransactionFixture(t)
			t.Chdir(t.TempDir())
			sqlite := requireSQLite(t)
			createDatabase(t, "app.db", "new")
			createDatabase(t, filepath.Join(f.target, "app.db"), "old")
			f.write(filepath.Join(f.bin, "sqlite3"), "#!/bin/bash\nif [[ -f $FAKE_ROOT/fail-db-backup && $* == *before.db* ]]; then echo 'backup failed' >&2; exit 1; fi\nexec "+shellQuote(sqlite)+" \"$@\"\n", 0700)
			runner := &filesystemRunner{env: f.command("release", "unused").Env}
			switch point {
			case "backup":
				f.mark("fail-db-backup")
			case "upload":
				runner.failSync = 1
			case "install":
				f.mark("fail-db-install")
			case "status":
				f.write(filepath.Join(f.bin, "systemctl"), "#!/bin/bash\nif [[ $2 == show-environment ]];then exit 0;fi\nexit 1\n", 0700)
			case "active":
				f.mark("active")
			}
			env := Environment{Host: "example.com", User: "deploy", Dir: f.target, Database: DatabaseConfig{Driver: "sqlite", Source: "app.db"}, Quadlet: Quadlet{ServiceName: "pdf"}}
			err := pushDatabase(env, ModeBinary, runner)
			if point == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if databaseValue(t, filepath.Join(f.target, "app.db")) != "new" || databaseValue(t, filepath.Join(f.target, "app.db.bak")) != "old" {
					t.Fatal("wrong database/backup")
				}
			} else {
				if err == nil {
					t.Fatal("unsafe success")
				}
				if databaseValue(t, filepath.Join(f.target, "app.db")) != "old" {
					t.Fatal("previous database changed", err)
				}
			}
			f.assertAbsent(filepath.Join(f.target, ".deploy/db/pending.json"))
		})
	}
}

func TestDatabaseCrashRecoveryIncludesWALData(t *testing.T) {
	f := newTransactionFixture(t)
	requireSQLite(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires Python for SQLite WAL crash fixture")
	}
	if err := os.MkdirAll(f.target, 0700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(f.target, "app.db")
	code := `import sqlite3,os,sys
c=sqlite3.connect(sys.argv[1]);c.execute('PRAGMA journal_mode=WAL');c.execute('PRAGMA wal_autocheckpoint=0');c.execute('CREATE TABLE sample(value TEXT)');c.execute("INSERT INTO sample VALUES ('old-wal')");c.commit();os._exit(0)
`
	if out, err := exec.Command("python3", "-c", code, database).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	stage := filepath.Join(f.target, ".deploy/db/staging/dbtest")
	os.MkdirAll(stage, 0700)
	createDatabase(t, filepath.Join(stage, "upload.db"), "new")
	req := databaseRequest{Schema: 1, ReleaseID: "dbtest", Service: "pdf", Target: f.target, Mode: ModeBinary, Database: database}
	body, _ := json.Marshal(req)
	f.write(filepath.Join(stage, "request.json"), string(body), 0600)
	f.write(f.script, databaseTransactionScript, 0700)
	f.mark("kill-db-after-install")
	if out, err := f.run("release", stage); err == nil {
		t.Fatal(out)
	}
	if databaseValue(t, database) != "new" {
		t.Fatal("crash did not happen after publication")
	}
	f.write(f.unit(), "[Container]\nImage=localhost/pdf:legacy\n", 0600)
	if out, err := f.run("action", stageBinaryFixture(f, "recover-db", nil, false), "stop"); err != nil {
		t.Fatal(out, err)
	}
	if databaseValue(t, database) != "old-wal" {
		t.Fatal("committed WAL data lost during recovery")
	}
	f.assertAbsent(filepath.Join(f.target, ".deploy/db/pending.json"))
}

func TestBinaryUploadFailureKeepsExistingServiceRunning(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(t.TempDir())
	f.mark("active")
	f.write(f.unit(), "legacy unit", 0600)
	f.write(filepath.Join(f.target, "server"), "old binary", 0755)
	os.WriteFile("Dockerfile.vps", []byte("FROM unused"), 0600)
	cfg := imageConfig()
	cfg.Deployment.Mode = ModeBinary
	cfg.BinaryName = "server"
	cfg.Build.Cmd = "printf new > build/server"
	cfg.Artifacts.Include = []string{"Dockerfile.vps"}
	changeEnv(&cfg, func(e *Environment) { e.Dir = f.target; e.Quadlet.StopOnDeploy = true; e.Quadlet.EnvFile = ptr("") })
	n, err := normalizeAndValidateConfig(cfg, "prod")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planBinaryRelease(n, "v1", "abc")
	if err != nil {
		t.Fatal(err)
	}
	runner := &filesystemRunner{env: f.command("release", "unused").Env, failSync: 3}
	if err := executeBinaryRelease(plan, runner); err == nil {
		t.Fatal("transfer failure ignored")
	}
	if _, err := os.Stat(filepath.Join(f.root, "active")); err != nil {
		t.Fatal("previous service stopped")
	}
	if f.read(f.unit()) != "legacy unit" || f.read(filepath.Join(f.target, "server")) != "old binary" {
		t.Fatal("active files changed during staging")
	}
}
