package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupFakeSSH(t *testing.T, f *transactionFixture) {
	t.Helper()
	t.Setenv("HOME", f.home)
	t.Setenv("FAKE_ROOT", f.root)
	t.Setenv("PATH", f.bin+":"+os.Getenv("PATH"))
	f.write(filepath.Join(f.bin, "ssh"), `#!/bin/bash
set -eu
while (($#)); do
 case $1 in -o|-p|-i|-l) shift 2;; -*) shift;; *) shift; break;; esac
done
if (($# == 1)); then exec bash -c "$1"; else exec "$@"; fi
`, 0700)
}

func TestCapacityFailsBeforeActivation(t *testing.T) {
	f := newTransactionFixture(t)
	f.success(f.stage("A", nil, false))
	before := f.read(filepath.Join(f.root, "calls"))
	stage := f.stage("B", nil, false)
	var req imageReleaseRequest
	json.Unmarshal([]byte(f.read(filepath.Join(stage, "request.json"))), &req)
	req.MinFreeBytes = 1 << 40
	body, _ := json.Marshal(req)
	f.write(filepath.Join(stage, "request.json"), string(body), 0600)
	out, err := f.run("release", stage)
	if err == nil || !strings.Contains(out, "Insufficient disk space") {
		t.Fatal(out, err)
	}
	calls := strings.TrimPrefix(f.read(filepath.Join(f.root, "calls")), before)
	if strings.Contains(calls, "systemctl stop") || strings.Contains(calls, "systemctl restart") {
		t.Fatal(calls)
	}
	if f.current() != "A" {
		t.Fatal("committed despite capacity failure")
	}
}

func TestHistoryRetentionProtectsReleasesAndBackups(t *testing.T) {
	f := newTransactionFixture(t)
	setupFakeSSH(t, f)
	for _, id := range []string{"A", "B", "C"} {
		f.success(f.stage(id, nil, false))
	}
	database := filepath.Join(f.target, "app.db")
	names := []string{"old", "middle", "new"}
	backups := make([]string, 0, 3)
	for i, name := range names {
		backup := filepath.Join(f.target, "backup", "app.prod.before-release."+name+".db")
		createDatabase(t, backup, name)
		if err := recordLocalBackup(database, backup, "prod", "before-release"); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(100+i), 0)
		os.Chtimes(backup, stamp, stamp)
		backups = append(backups, backup)
	}
	reference, _ := json.Marshal(map[string]string{"backup": backups[0]})
	f.write(filepath.Join(f.state(), "releases/C/before/database.json"), string(reference), 0600)
	unmanaged := filepath.Join(f.target, "backup/manual.db")
	f.write(unmanaged, "manual", 0600)
	c := imageConfig()
	changeEnv(&c, func(e *Environment) {
		e.Dir = f.target
		e.Database = DatabaseConfig{Driver: "sqlite", Source: "app.db", Retention: RetentionConfig{KeepLast: 1}}
		e.Storage.Releases = RetentionConfig{KeepLast: 1}
	})
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err = pruneHistory(n, true); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(f.state(), "releases/A")); err != nil {
		t.Fatal("preview deleted release", err)
	}
	if err = pruneHistory(n, false); err != nil {
		t.Fatal(err)
	}
	f.assertAbsent(filepath.Join(f.state(), "releases/A"))
	f.assertAbsent(backups[1])
	f.assertAbsent(backups[1] + ".meta.json")
	for _, file := range []string{backups[0], backups[2], unmanaged, filepath.Join(f.state(), "releases/B/manifest.json"), filepath.Join(f.state(), "releases/C/manifest.json")} {
		if _, err = os.Stat(file); err != nil {
			t.Fatal(file, err)
		}
	}
}

func TestHistoryRetentionRefusesPendingOrCorruptState(t *testing.T) {
	for _, kind := range []string{"pending", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			f := newTransactionFixture(t)
			setupFakeSSH(t, f)
			f.success(f.stage("A", nil, false))
			if kind == "pending" {
				f.write(filepath.Join(f.state(), "pending.json"), "{}", 0600)
			} else {
				f.write(filepath.Join(f.state(), "releases/A/manifest.json"), "invalid", 0600)
			}
			c := imageConfig()
			changeEnv(&c, func(e *Environment) { e.Dir = f.target; e.Storage.Releases.KeepLast = 1 })
			n, err := normalizeAndValidateConfig(c, "prod")
			if err != nil {
				t.Fatal(err)
			}
			if err = pruneHistory(n, false); err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			if _, err = os.Stat(filepath.Join(f.state(), "releases/A")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOffHostBackupAndApplicationRestore(t *testing.T) {
	f := newTransactionFixture(t)
	setupFakeSSH(t, f)
	createDatabase(t, filepath.Join(f.target, "app.db"), "expected")
	e := Environment{Name: "prod", Host: "source.example.com", User: "deploy", Dir: f.target, Quadlet: Quadlet{ServiceName: "pdf"}, Database: DatabaseConfig{Driver: "sqlite", Source: "app.db", OffHost: OffHostConfig{Host: "backup.example.com", User: "backup", Dir: filepath.Join(f.root, "off-host"), RestoreCheck: `[ "$(sqlite3 -readonly "$DEPLOY_RESTORE_DATABASE" 'select value from sample;')" = expected ]`}}}
	id, err := backupOffHost(e)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyOffHostRestore(e, id); err != nil {
		t.Fatal(err)
	}
	if databaseValue(t, filepath.Join(f.target, "app.db")) != "expected" {
		t.Fatal("backup changed source")
	}
	f.write(filepath.Join(e.Database.OffHost.Dir, id, "database.db"), "corrupted", 0600)
	if err = verifyOffHostRestore(e, id); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal("corruption accepted", err)
	}
}

func TestLocalRetentionKeepsRecentAndUnmanagedSnapshots(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	createDatabase(t, "app.db", "current")
	for range 3 {
		if _, err := backupLocalDatabase("app.db", "local", "before-pull-from-prod"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backupLocalDatabase("app.db", "local", "before-pull-from-test"); err != nil {
		t.Fatal(err)
	}
	e := Environment{Name: "prod", Database: DatabaseConfig{Driver: "sqlite", Source: "app.db", Retention: RetentionConfig{KeepLast: 1}}}
	if err := pruneLocalDatabase(e, true); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob("backup/*.db"); len(files) != 4 {
		t.Fatal(files)
	}
	if err := pruneLocalDatabase(e, false); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob("backup/*.db"); len(files) != 2 {
		t.Fatal(files)
	}
	if databaseValue(t, "app.db") != "current" {
		t.Fatal("cleanup modified database")
	}
}

func TestHistoryRetentionCleansClosedOperationsAndProtectsTheirBackups(t *testing.T) {
	f := newTransactionFixture(t)
	setupFakeSSH(t, f)
	f.success(f.stage("A", nil, false))
	database := filepath.Join(f.target, "app.db")
	var backups []string
	for i, name := range []string{"old", "protected", "recent"} {
		file := filepath.Join(f.target, "backup", "app.prod.before-push."+name+".db")
		createDatabase(t, file, name)
		if err := recordLocalBackup(database, file, "prod", "before-push"); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(100+i), 0)
		os.Chtimes(file, stamp, stamp)
		backups = append(backups, file)
	}
	var operations []string
	for i, id := range []string{"op-A", "op-B", "op-C"} {
		directory := filepath.Join(f.target, ".deploy/db/operations", id)
		record := map[string]any{"schema": 1, "release_id": id, "service": "pdf", "target": f.target, "before_backup": backups[1]}
		body, _ := json.Marshal(record)
		f.write(filepath.Join(directory, "manifest.json"), string(body), 0600)
		stamp := time.Unix(int64(100+i), 0)
		os.Chtimes(directory, stamp, stamp)
		operations = append(operations, directory)
	}
	c := imageConfig()
	changeEnv(&c, func(e *Environment) {
		e.Dir = f.target
		e.Database = DatabaseConfig{Driver: "sqlite", Source: "app.db", Retention: RetentionConfig{KeepLast: 1}}
		e.Storage.Releases.KeepLast = 1
	})
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err = pruneHistory(n, false); err != nil {
		t.Fatal(err)
	}
	f.assertAbsent(operations[0])
	f.assertAbsent(operations[1])
	f.assertAbsent(backups[0])
	for _, file := range []string{operations[2], backups[1], backups[2]} {
		if _, err = os.Stat(file); err != nil {
			t.Fatal(file, err)
		}
	}
}

func TestFailedPushRetainsPublishedCandidateBeforeRewind(t *testing.T) {
	f := newTransactionFixture(t)
	t.Chdir(t.TempDir())
	createDatabase(t, "app.db", "incoming")
	remote := filepath.Join(f.target, "app.db")
	createDatabase(t, remote, "original")
	f.write(filepath.Join(f.bin, "mv"), `#!/bin/bash
set -eu
if [[ ${!#} == `+shellQuote(remote)+` && -f $FAKE_ROOT/fail-after-database-install ]]; then
 /usr/bin/mv "$@"
 /usr/bin/rm "$FAKE_ROOT/fail-after-database-install"
 exit 1
fi
exec /usr/bin/mv "$@"
`, 0700)
	f.mark("fail-after-database-install")
	e := Environment{Name: "prod", Host: "example.com", User: "deploy", Dir: f.target, Quadlet: Quadlet{ServiceName: "pdf"}, Database: DatabaseConfig{Driver: "sqlite", Source: "app.db"}}
	if err := pushDatabase(e, ModeImage, &filesystemRunner{env: f.command("release", "unused").Env}); err == nil {
		t.Fatal("publication failure ignored")
	}
	if databaseValue(t, remote) != "original" {
		t.Fatal("original database not restored")
	}
	backups, _ := filepath.Glob(filepath.Join(f.target, "backup/app.prod.before-push-rollback.*.db"))
	if len(backups) != 1 || databaseValue(t, backups[0]) != "incoming" {
		t.Fatal("incoming state was not retained before rewind", backups)
	}
	f.assertAbsent(filepath.Join(f.target, ".deploy/db/pending.json"))
}
