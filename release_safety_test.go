package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func safetyBinaryPlan(t *testing.T, f *transactionFixture, includes []string, build string) ReleasePlan {
	t.Helper()
	c := imageConfig()
	c.Deployment.Mode = ModeBinary
	c.BinaryName = "server"
	c.Build.Cmd = build
	c.Artifacts.Include = includes
	changeEnv(&c, func(e *Environment) {
		e.Dir = f.target
		e.Quadlet.EnvFile = ptr("")
		e.Quadlet.Image = "localhost/pdf:latest"
	})
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	p, err := planBinaryRelease(n, "safety", "abc")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBinaryBuildUsesOnlyStagedArtifactsAndPreservesPaths(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(f.root)
	f.write("Dockerfile.vps", "FROM scratch\nCOPY . /app", 0600)
	f.write("scripts/backup_tar.sh", "backup script", 0700)
	f.write(filepath.Join(f.target, ".env"), "secret", 0600)
	f.write(filepath.Join(f.target, "data/app.db"), "database", 0600)
	f.write(filepath.Join(f.target, "backup/old.db"), "backup", 0600)
	f.write(filepath.Join(f.bin, "podman-real"), fakePodman, 0700)
	f.write(filepath.Join(f.bin, "podman"), "#!/bin/bash\nif [[ $1 == build ]]; then pwd > \"$FAKE_ROOT/build-context\"; find . -type f > \"$FAKE_ROOT/build-inputs\"; fi\nexec "+shellQuote(filepath.Join(f.bin, "podman-real"))+" \"$@\"\n", 0700)
	p := safetyBinaryPlan(t, f, []string{"Dockerfile.vps", "scripts/backup_tar.sh"}, "printf candidate > build/server")
	if err := executeBinaryRelease(p, &filesystemRunner{env: f.command("release", "unused").Env}); err != nil {
		t.Fatal(err)
	}
	if f.read(filepath.Join(f.target, "scripts/backup_tar.sh")) != "backup script" {
		t.Fatal("nested artifact missing")
	}
	f.assertAbsent(filepath.Join(f.target, "backup_tar.sh"))
	context := strings.TrimSpace(f.read(filepath.Join(f.root, "build-context")))
	if !strings.Contains(context, "/.deploy/binary/staging/") || !strings.HasSuffix(context, "/artifacts") {
		t.Fatal("live target used as build context", context)
	}
	inputs := f.read(filepath.Join(f.root, "build-inputs"))
	for _, forbidden := range []string{".env", "app.db", "old.db", "transaction.sh", "request.json"} {
		if strings.Contains(inputs, forbidden) {
			t.Fatal("secret or control file in build context", inputs)
		}
	}
}

func TestCustomBuildMustProduceFreshRegularOutput(t *testing.T) {
	for _, command := range []string{":", "touch build/server", "ln -s ../Dockerfile.vps build/server", "mkdir build/server"} {
		t.Run(command, func(t *testing.T) {
			f := newTransactionFixture(t)
			t.Chdir(f.root)
			f.write("Dockerfile.vps", "FROM scratch", 0600)
			f.write("build/server", "stale binary", 0700)
			p := safetyBinaryPlan(t, f, []string{"Dockerfile.vps"}, command)
			runner := &filesystemRunner{env: f.command("release", "unused").Env}
			if err := executeBinaryRelease(p, runner); err == nil {
				t.Fatal("invalid build output accepted")
			}
			if runner.syncs != 0 {
				t.Fatal("invalid build was uploaded")
			}
		})
	}
}

func TestDeletedManagedFilesAreRemovedAndRollbackRestoresThem(t *testing.T) {
	for _, mode := range []DeploymentMode{ModeBinary, ModeImage} {
		for _, rollback := range []bool{false, true} {
			t.Run(string(mode)+"-"+map[bool]string{false: "commit", true: "rollback"}[rollback], func(t *testing.T) {
				f := newTransactionFixture(t)
				stage := f.stage
				if mode == ModeBinary {
					stage = func(id string, a map[string]string, s bool) string { return stageBinaryFixture(f, id, a, s) }
				}
				f.write(filepath.Join(f.target, "assets/unmanaged.txt"), "user file", 0600)
				f.success(stage("A", map[string]string{"assets/obsolete.txt": "old"}, false))
				f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
				if rollback {
					f.mark("fail-health")
				}
				out, err := f.run("release", stage("B", map[string]string{"assets/current.txt": "new"}, false))
				if rollback {
					if err == nil || strings.Contains(out, "ROLLBACK FAILED") {
						t.Fatal(out, err)
					}
					if f.read(filepath.Join(f.target, "assets/obsolete.txt")) != "old" {
						t.Fatal("deleted file not restored")
					}
					f.assertAbsent(filepath.Join(f.target, "assets/current.txt"))
				} else {
					if err != nil {
						t.Fatal(out, err)
					}
					f.assertAbsent(filepath.Join(f.target, "assets/obsolete.txt"))
					if f.read(filepath.Join(f.target, "assets/current.txt")) != "new" {
						t.Fatal("new artifact missing")
					}
				}
				if f.read(filepath.Join(f.target, "assets/unmanaged.txt")) != "user file" {
					t.Fatal("unmanaged file changed")
				}
			})
		}
	}
}

func databaseStage(f *transactionFixture, mode DeploymentMode, id, database, rollback string) string {
	f.t.Helper()
	stage := ""
	if mode == ModeBinary {
		stage = stageBinaryFixture(f, id, map[string]string{"server": id}, false)
	} else {
		stage = f.stage(id, map[string]string{"server": id}, false)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(f.read(filepath.Join(stage, "request.json"))), &req); err != nil {
		f.t.Fatal(err)
	}
	req["database"], req["environment"], req["database_rollback"] = database, "prod", rollback
	req["container_uid"], req["container_gid"] = 0, 0
	body, err := json.Marshal(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(stage, "request.json"), string(body), 0600)
	return stage
}

func TestReleaseDatabaseRecoveryPolicy(t *testing.T) {
	for _, mode := range []DeploymentMode{ModeBinary, ModeImage} {
		for _, policy := range []string{"unchanged", "manual", "restore", "kill-manual", "kill-restore"} {
			t.Run(string(mode)+"-"+policy, func(t *testing.T) {
				f := newTransactionFixture(t)
				requireSQLite(t)
				db := filepath.Join(f.target, "data/app.db")
				createDatabase(t, db, "old")
				rollback := strings.TrimPrefix(policy, "kill-")
				f.success(databaseStage(f, mode, "A", db, "manual"))
				if policy != "unchanged" {
					f.write(filepath.Join(f.bin, "systemctl-real"), fakeSystemctl, 0700)
					kill := ""
					if strings.HasPrefix(policy, "kill-") {
						kill = "kill -KILL \"$(cat \"$FAKE_ROOT/txpid\")\"\n"
					}
					f.write(filepath.Join(f.bin, "systemctl"), "#!/bin/bash\nif [[ ${2:-} == restart ]]; then sqlite3 "+shellQuote(db)+" \"UPDATE sample SET value='migrated';\"\n"+kill+"fi\nexec "+shellQuote(filepath.Join(f.bin, "systemctl-real"))+" \"$@\"\n", 0700)
				}
				f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
				f.mark("fail-health")
				out, err := f.run("release", databaseStage(f, mode, "B", db, rollback))
				if err == nil {
					t.Fatal("unhealthy release succeeded", out)
				}
				if strings.HasPrefix(policy, "kill-") {
					if mode == ModeBinary {
						_, err = f.run("action", stageBinaryFixture(f, "recover", nil, false), "start")
					} else {
						_, err = f.run("action", f.stage("recover", nil, false), "start")
					}
					if (err != nil) != (rollback == "manual") {
						t.Fatal("wrong recovery result", err)
					}
				}
				if rollback == "manual" {
					f.assertAbsent(filepath.Join(f.root, "active"))
					if databaseValue(t, db) != "migrated" {
						t.Fatal("candidate writes discarded")
					}
					if _, err := os.Stat(filepath.Join(f.target, ".deploy", string(mode), "pending.json")); err != nil {
						t.Fatal("pending recovery missing", err)
					}
				} else {
					if databaseValue(t, db) != "old" || f.read(filepath.Join(f.root, "running-image")) != imageA {
						t.Fatal("previous deployment not restored", out)
					}
					f.assertAbsent(filepath.Join(f.target, ".deploy", string(mode), "pending.json"))
				}
				backups, err := filepath.Glob(filepath.Join(filepath.Dir(db), "backup", "app.prod.before-release.*.db"))
				if err != nil || len(backups) != 2 {
					t.Fatal("pre-release backups missing", backups, err)
				}
				for _, backup := range backups {
					if databaseValue(t, backup) != "old" {
						t.Fatal("wrong pre-release backup")
					}
				}
				if rollback == "restore" {
					candidateBackups, _ := filepath.Glob(filepath.Join(filepath.Dir(db), "backup", "app.prod.before-rollback.*.db"))
					if len(candidateBackups) != 1 || databaseValue(t, candidateBackups[0]) != "migrated" {
						t.Fatal("candidate database was not retained", candidateBackups)
					}
				}
				if rollback == "manual" {
					f.write(db, f.read(filepath.Join(f.target, ".deploy", string(mode), "releases/B/before/database.db")), 0600)
					var recovery string
					if mode == ModeBinary {
						recovery = stageBinaryFixture(f, "recover-manually", nil, false)
					} else {
						recovery = f.stage("recover-manually", nil, false)
					}
					if out, err := f.run("action", recovery, "start"); err != nil {
						t.Fatal("manual restoration could not finish recovery", out, err)
					}
					if databaseValue(t, db) != "old" || f.read(filepath.Join(f.root, "running-image")) != imageA {
						t.Fatal("manual recovery did not restore old deployment")
					}
				}
			})
		}
	}
}

func TestPullRetainsHistoryAndRestoresWALDatabaseTransactionally(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(f.root)
	requireSQLite(t)
	remote := filepath.Join(f.root, "remote")
	createDatabase(t, filepath.Join(remote, "app.db"), "production")
	createDatabase(t, "app.db", "local-original")
	if out, err := exec.Command("sqlite3", "app.db", "PRAGMA journal_mode=WAL;").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	f.write(filepath.Join(f.bin, "ssh"), "#!/bin/bash\nexec bash -c \"${!#}\"\n", 0700)
	t.Setenv("PATH", f.bin+":"+os.Getenv("PATH"))
	t.Setenv("HOME", f.home)
	e := Environment{Name: "prod", User: "deploy", Host: "example.com", Dir: remote, Quadlet: Quadlet{ServiceName: "pdf"}, Database: DatabaseConfig{Driver: "sqlite", Source: "app.db"}}
	for i := 0; i < 2; i++ {
		if err := pullDatabase(e); err != nil {
			t.Fatal(err)
		}
	}
	backups, err := filepath.Glob("backup/app.local.before-pull-from-prod.*.db")
	if err != nil || len(backups) != 2 {
		t.Fatal(backups, err)
	}
	values := map[string]bool{}
	for _, backup := range backups {
		values[databaseValue(t, backup)] = true
		info, err := os.Stat(backup)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("backup not private", info, err)
		}
	}
	if !values["local-original"] || !values["production"] || databaseValue(t, "app.db") != "production" {
		t.Fatal("backup history lost", values)
	}
}

func TestPullRestoreFailurePreservesCommittedWALData(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(f.root)
	requireSQLite(t)
	remote := filepath.Join(f.root, "remote")
	createDatabase(t, filepath.Join(remote, "app.db"), "remote")
	createDatabase(t, "app.db", "original")
	writer := exec.Command("sqlite3", "app.db")
	input, err := writer.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := writer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); writer.Wait() }()
	if _, err := fmt.Fprintln(input, "PRAGMA journal_mode=WAL; UPDATE sample SET value='committed-wal'; BEGIN IMMEDIATE; SELECT 'locked';"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(output)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "locked" {
			break
		}
	}
	f.write(filepath.Join(f.bin, "ssh"), "#!/bin/bash\nexec bash -c \"${!#}\"\n", 0700)
	t.Setenv("PATH", f.bin+":"+os.Getenv("PATH"))
	t.Setenv("HOME", f.home)
	e := Environment{Name: "prod", User: "deploy", Host: "example.com", Dir: remote, Quadlet: Quadlet{ServiceName: "pdf"}, Database: DatabaseConfig{Driver: "sqlite", Source: "app.db"}}
	if err := pullDatabase(e); err == nil {
		t.Fatal("restore succeeded while another writer held the database")
	}
	if _, err := os.Stat("app.db-wal"); err != nil {
		t.Fatal("WAL detached on restore failure", err)
	}
	if databaseValue(t, "app.db") != "committed-wal" {
		t.Fatal("committed WAL data lost")
	}
	backups, _ := filepath.Glob("backup/app.local.before-pull-from-prod.*.db")
	if len(backups) != 1 || databaseValue(t, backups[0]) != "committed-wal" {
		t.Fatal("consistent backup missing", backups)
	}
}

func TestReleaseAbortsWhenDatabaseBackupCannotBeRetained(t *testing.T) {
	for _, mode := range []DeploymentMode{ModeBinary, ModeImage} {
		t.Run(string(mode), func(t *testing.T) {
			f := newTransactionFixture(t)
			requireSQLite(t)
			db := filepath.Join(f.target, "data/app.db")
			createDatabase(t, db, "old")
			f.success(databaseStage(f, mode, "A", db, "manual"))
			if err := os.RemoveAll(filepath.Join(filepath.Dir(db), "backup")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.root, filepath.Join(filepath.Dir(db), "backup")); err != nil {
				t.Fatal(err)
			}
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			out, err := f.run("release", databaseStage(f, mode, "B", db, "manual"))
			if err == nil || strings.Contains(out, "ROLLBACK FAILED") {
				t.Fatal(out, err)
			}
			if databaseValue(t, db) != "old" || f.read(filepath.Join(f.root, "running-image")) != imageA {
				t.Fatal("backup failure changed live deployment", out)
			}
			if _, err := os.Stat(filepath.Join(f.root, "active")); err != nil {
				t.Fatal("old service was not restored", err)
			}
		})
	}
}

func TestDeletedFilesInReadonlyRemovedDirectory(t *testing.T) {
	for _, mode := range []DeploymentMode{ModeBinary, ModeImage} {
		for _, rollback := range []bool{false, true} {
			t.Run(string(mode)+fmt.Sprint(rollback), func(t *testing.T) {
				f := newTransactionFixture(t)
				stage := f.stage
				if mode == ModeBinary {
					stage = func(id string, a map[string]string, s bool) string { return stageBinaryFixture(f, id, a, s) }
				}
				first := stage("A", map[string]string{"readonly/old": "old"}, false)
				if err := os.Chmod(filepath.Join(first, "artifacts/readonly"), 0555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					os.Chmod(filepath.Join(first, "artifacts/readonly"), 0700)
					os.Chmod(filepath.Join(f.target, "readonly"), 0700)
				})
				f.success(first)
				f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
				if rollback {
					f.mark("fail-health")
				}
				out, err := f.run("release", stage("B", map[string]string{"new": "new"}, false))
				if rollback {
					if err == nil || strings.Contains(out, "ROLLBACK FAILED") {
						t.Fatal(out, err)
					}
					if f.read(filepath.Join(f.target, "readonly/old")) != "old" {
						t.Fatal("deleted read-only artifact not restored")
					}
				} else {
					if err != nil {
						t.Fatal(out, err)
					}
					f.assertAbsent(filepath.Join(f.target, "readonly/old"))
				}
				info, err := os.Stat(filepath.Join(f.target, "readonly"))
				if err != nil || info.Mode().Perm() != 0555 {
					t.Fatal("original directory mode not preserved", info, err)
				}
			})
		}
	}
}

func TestDatabaseConfigurationRejectsUnsafeSettings(t *testing.T) {
	for _, mode := range []DeploymentMode{ModeBinary, ModeImage} {
		for _, bad := range []string{"driver", "source", "rollback", "backup-owner"} {
			t.Run(string(mode)+"-"+bad, func(t *testing.T) {
				c := imageConfig()
				c.Deployment.Mode = mode
				if mode == ModeBinary {
					c.BinaryName = "server"
				}
				changeEnv(&c, func(e *Environment) {
					e.Database = DatabaseConfig{Driver: "sqlite", Source: "data/app.db"}
					switch bad {
					case "driver":
						e.Database.Driver = "postgres"
					case "source":
						e.Database.Source = "../other.db"
					case "rollback":
						e.Database.Rollback = "ignore"
					case "backup-owner":
						e.Quadlet.ChownVolumes = []string{"./data/backup"}
					}
				})
				if _, err := normalizeAndValidateConfig(c, "prod"); err == nil {
					t.Fatal("unsafe database settings accepted")
				}
			})
		}
	}
}

func TestInterruptedDatabaseRewindResumesOriginalRestore(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires Python for a committed WAL fixture")
	}
	for _, mode := range []DeploymentMode{ModeBinary, ModeImage} {
		t.Run(string(mode), func(t *testing.T) {
			f := newTransactionFixture(t)
			requireSQLite(t)
			db := filepath.Join(f.target, "data/app.db")
			createDatabase(t, db, "old")
			f.success(databaseStage(f, mode, "A", db, "restore"))
			code := `import sqlite3,os,sys
c=sqlite3.connect(sys.argv[1]);c.execute("UPDATE sample SET value='main-phase'");c.commit()
c.execute('PRAGMA journal_mode=WAL');c.execute('PRAGMA wal_autocheckpoint=0')
c.execute("UPDATE sample SET value='wal-phase'");c.commit();os._exit(0)
`
			f.write(filepath.Join(f.bin, "systemctl-real"), fakeSystemctl, 0700)
			f.write(filepath.Join(f.bin, "systemctl"), "#!/bin/bash\nif [[ ${2:-} == restart ]]; then python3 -c "+shellQuote(code)+" "+shellQuote(db)+"; fi\nexec "+shellQuote(filepath.Join(f.bin, "systemctl-real"))+" \"$@\"\n", 0700)
			f.write(filepath.Join(f.bin, "rm"), "#!/bin/bash\nif [[ -f $FAKE_ROOT/kill-rewind && $* == *app.db-wal* ]]; then /usr/bin/rm \"$@\"; /usr/bin/rm \"$FAKE_ROOT/kill-rewind\"; kill -KILL \"$(cat \"$FAKE_ROOT/txpid\")\"; exit 1; fi\nexec /usr/bin/rm \"$@\"\n", 0700)
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark("fail-health")
			f.mark("kill-rewind")
			if out, err := f.run("release", databaseStage(f, mode, "B", db, "restore")); err == nil {
				t.Fatal("rewind was not interrupted", out)
			}
			marker := filepath.Join(f.target, ".deploy", string(mode), "releases/B/database-restoring.json")
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("rewind was not journaled", err)
			}
			var recovery string
			if mode == ModeBinary {
				recovery = stageBinaryFixture(f, "recover-rewind", nil, false)
			} else {
				recovery = f.stage("recover-rewind", nil, false)
			}
			if out, err := f.run("action", recovery, "start"); err != nil {
				t.Fatal(out, err)
			}
			if databaseValue(t, db) != "old" || f.read(filepath.Join(f.root, "running-image")) != imageA {
				t.Fatal("original snapshot was not restored")
			}
			backups, _ := filepath.Glob(filepath.Join(filepath.Dir(db), "backup/app.prod.before-rollback.*.db"))
			if len(backups) != 1 || databaseValue(t, backups[0]) != "wal-phase" {
				t.Fatal("original candidate backup was replaced", backups)
			}
		})
	}
}

func TestFailedStartupCreatingDatabasePreservesOrRemovesItByPolicy(t *testing.T) {
	for _, policy := range []string{"manual", "restore"} {
		t.Run(policy, func(t *testing.T) {
			f := newTransactionFixture(t)
			requireSQLite(t)
			db := filepath.Join(f.target, "data/app.db")
			if err := os.MkdirAll(filepath.Dir(db), 0700); err != nil {
				t.Fatal(err)
			}
			f.success(databaseStage(f, ModeBinary, "A", db, policy))
			f.write(filepath.Join(f.bin, "systemctl-real"), fakeSystemctl, 0700)
			f.write(filepath.Join(f.bin, "systemctl"), "#!/bin/bash\nif [[ ${2:-} == restart ]]; then sqlite3 "+shellQuote(db)+" \"CREATE TABLE sample(value TEXT); INSERT INTO sample VALUES ('created');\"; fi\nexec "+shellQuote(filepath.Join(f.bin, "systemctl-real"))+" \"$@\"\n", 0700)
			f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
			f.mark("fail-health")
			out, err := f.run("release", databaseStage(f, ModeBinary, "B", db, policy))
			if err == nil {
				t.Fatal("unhealthy startup succeeded", out)
			}
			if policy == "manual" {
				if databaseValue(t, db) != "created" {
					t.Fatal("new database was discarded")
				}
				f.assertAbsent(filepath.Join(f.root, "active"))
			} else {
				f.assertAbsent(db)
				if f.read(filepath.Join(f.root, "running-image")) != imageA {
					t.Fatal("old service not restored")
				}
				backups, _ := filepath.Glob(filepath.Join(filepath.Dir(db), "backup/app.prod.before-rollback.*.db"))
				if len(backups) != 1 || databaseValue(t, backups[0]) != "created" {
					t.Fatal("new database was not backed up", backups)
				}
			}
		})
	}
}

func TestBinaryVolumeOwnershipSkipsDatabaseBackups(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(f.root)
	requireSQLite(t)
	db := filepath.Join(f.target, "data/app.db")
	createDatabase(t, db, "old")
	createDatabase(t, filepath.Join(filepath.Dir(db), "backup/historical.db"), "historical")
	f.write("Dockerfile.vps", "FROM scratch", 0600)
	f.write(filepath.Join(f.bin, "podman-real"), fakePodman, 0700)
	f.write(filepath.Join(f.bin, "podman"), "#!/bin/bash\nif [[ $1 == unshare ]]; then shift; exec \"$@\"; fi\nexec "+shellQuote(filepath.Join(f.bin, "podman-real"))+" \"$@\"\n", 0700)
	f.write(filepath.Join(f.bin, "chown"), "#!/bin/bash\nprintf '%s\\n' \"$@\" >> \"$FAKE_ROOT/chown-paths\"\n", 0700)
	p := safetyBinaryPlan(t, f, []string{"Dockerfile.vps"}, "printf candidate > build/server")
	p.Config.Env.Database = DatabaseConfig{Driver: "sqlite", Source: "data/app.db"}
	p.Config.Env.Quadlet.ContainerUID, p.Config.Env.Quadlet.ContainerGID = 65532, 65532
	p.Config.Env.Quadlet.ChownVolumes = []string{"data"}
	if err := executeBinaryRelease(p, &filesystemRunner{env: f.command("release", "unused").Env}); err != nil {
		t.Fatal(err)
	}
	calls := f.read(filepath.Join(f.root, "chown-paths"))
	if strings.Contains(calls, "/backup") || !strings.Contains(calls, db) {
		t.Fatal("backup ownership was changed or data ownership skipped", calls)
	}
}

func TestUnreadableDatabaseCannotBeMistakenForMissingDatabase(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission checks require an unprivileged test user")
	}
	f := newTransactionFixture(t)
	requireSQLite(t)
	f.success(stageBinaryFixture(f, "A", map[string]string{"server": "old"}, false))
	db := filepath.Join(f.target, "private/app.db")
	createDatabase(t, db, "old")
	if err := os.Chmod(filepath.Dir(db), 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(db), 0700) })
	f.write(filepath.Join(f.root, "registry-image"), imageB, 0600)
	out, err := f.run("release", databaseStage(f, ModeBinary, "B", db, "manual"))
	if err == nil || !strings.Contains(out, "inaccessible") || strings.Contains(out, "ROLLBACK FAILED") {
		t.Fatal(out, err)
	}
	if f.read(filepath.Join(f.root, "running-image")) != imageA {
		t.Fatal("inaccessible database allowed candidate startup")
	}
}

func TestDatabaseFingerprintIncludesMigrationMetadataAndRowids(t *testing.T) {
	requireSQLite(t)
	db := filepath.Join(t.TempDir(), "app.db")
	createDatabase(t, db, "same data")
	fingerprint := func() string {
		t.Helper()
		out, err := exec.Command("bash", "-c", transactionCommonScript+"\nsqlite_fingerprint "+shellQuote(db)).CombinedOutput()
		if err != nil {
			t.Fatal(string(out), err)
		}
		return string(out)
	}
	original := fingerprint()
	for _, sql := range []string{"PRAGMA user_version=7;", "PRAGMA user_version=0; PRAGMA application_id=42;", "PRAGMA application_id=0; UPDATE sample SET rowid=rowid+1;"} {
		if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
		if fingerprint() == original {
			t.Fatalf("database change ignored: %s", sql)
		}
	}
}
