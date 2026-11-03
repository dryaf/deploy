package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func schemaMigrationFixture(t *testing.T) (string, Config, protocol.Identity, string) {
	t.Helper()
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	c := configuration()
	s, err := Open(path, c, true)
	must(t, err)
	must(t, s.Stage(producer, "staged"))
	must(t, s.Replace(context.Background(), snapshot(time.Now(), 1, 1)))
	must(t, s.transaction(context.Background(), func(tx *sql.Tx) error { _, e := s.currentLEDState(tx, workerLEDBinding, "critical"); return e }))
	_, err = s.db.Exec("PRAGMA user_version=9")
	must(t, err)
	id := s.Identity()
	must(t, s.Close())
	op, err := safefs.NewID("n")
	must(t, err)
	return path, c, id, op
}
func TestManagedSchemaMigrationPreservesStateAndBackup(t *testing.T) {
	path, c, id, op := schemaMigrationFixture(t)
	ctx := context.Background()
	must(t, MigrateManagedSchema(ctx, path, c, id, op, 9))
	d, err := privateDirectory(path)
	must(t, err)
	defer d.Close()
	stage, err := d.Sub("schema-migrations/" + op)
	must(t, err)
	defer stage.Close()
	var intent ManagedSchemaMigration
	must(t, stage.ReadJSON("intent.json", &intent))
	backup, err := snapshotDB(stage, "before.db", true)
	must(t, err)
	defer backup.Close()
	var version, count int
	must(t, backup.QueryRow("PRAGMA user_version").Scan(&version))
	if version != 9 {
		t.Fatal(version)
	}
	must(t, backup.QueryRow("SELECT count(*) FROM led_states").Scan(&count))
	if count != 1 {
		t.Fatal("lost backup LED evidence", count)
	}
	s, err := OpenManagedVerification(path, c, id)
	must(t, err)
	must(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	if version != currentSchema {
		t.Fatal(version)
	}
	must(t, s.db.QueryRow("SELECT count(*) FROM led_states").Scan(&count))
	if count != 1 {
		t.Fatal("lost LED evidence")
	}
	must(t, s.db.QueryRow("SELECT count(*) FROM status").Scan(&count))
	if count != 1 {
		t.Fatal("lost producer status")
	}
	must(t, s.RequireInactiveProducers())
	must(t, s.Close())
	must(t, MigrateManagedSchema(ctx, path, c, id, op, 9))
	var again ManagedSchemaMigration
	must(t, stage.ReadJSON("complete.json", &again))
	if again != intent {
		t.Fatal("retry changed original proof")
	}
	other, _ := safefs.NewID("n")
	if MigrateManagedSchema(ctx, path, c, id, other, 9) == nil {
		t.Fatal("upgraded DB adopted without original operation")
	}
}
func TestManagedSchemaMigrationInterrupted(t *testing.T) {
	for _, point := range []string{"schema_intent", "schema_copy", "schema_backup", "schema_transaction", "schema_committed", "schema_complete", "schema_released"} {
		t.Run(point, func(t *testing.T) {
			path, c, id, op := schemaMigrationFixture(t)
			fail := func(p string) error {
				if p == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if migrateManagedSchema(context.Background(), path, c, id, op, 9, fail) == nil {
				t.Fatal("missing interruption")
			}
			if point != "schema_released" {
				if s, e := Open(path, c, false); e == nil {
					s.Close()
					t.Fatal("pending migration allowed ordinary startup")
				}
			}
			must(t, MigrateManagedSchema(context.Background(), path, c, id, op, 9))
			s, e := OpenManagedVerification(path, c, id)
			must(t, e)
			must(t, s.Close())
		})
	}
}
func TestManagedSchemaMigrationRejectsChangedAuthority(t *testing.T) {
	for _, damage := range []string{"backup", "identity", "active", "quarantine", "intent", "marker", "config", "lease", "sidecar"} {
		t.Run(damage, func(t *testing.T) {
			path, c, id, op := schemaMigrationFixture(t)
			ctx := context.Background()
			if damage == "active" || damage == "lease" {
				s, e := Open(path, c, false)
				must(t, e)
				if damage == "active" {
					must(t, s.Activate(producer, "staged"))
					_, e = s.db.Exec("PRAGMA user_version=9")
					must(t, e)
					must(t, s.Close())
				} else {
					defer s.Close()
				}
			} else {
				err := migrateManagedSchema(ctx, path, c, id, op, 9, func(p string) error {
					if p == "schema_backup" {
						return errors.New("stop")
					}
					return nil
				})
				if err == nil {
					t.Fatal("did not stop")
				}
			}
			switch damage {
			case "sidecar":
				target := filepath.Join(t.TempDir(), "unrelated")
				must(t, os.WriteFile(target, []byte("private"), 0600))
				must(t, os.Symlink(target, filepath.Join(path, "state.db-wal")))
			case "backup":
				must(t, os.WriteFile(filepath.Join(path, "schema-migrations", op, "before.db"), []byte("changed"), 0600))
			case "identity":
				id.Recovery, _ = safefs.NewID("n")
			case "quarantine":
				must(t, os.WriteFile(filepath.Join(path, "recovery.json"), []byte("{}"), 0600))
			case "intent":
				must(t, os.WriteFile(filepath.Join(path, "schema-migrations", op, "intent.json"), []byte("{}"), 0600))
			case "marker":
				d, e := privateDirectory(path)
				must(t, e)
				defer d.Close()
				db, e := snapshotDB(d, "state.db", false)
				must(t, e)
				_, e = db.Exec("PRAGMA user_version=10")
				must(t, e)
				must(t, db.Close())
			case "config":
				c.MaxMessages++
			}
			if MigrateManagedSchema(ctx, path, c, id, op, 9) == nil {
				t.Fatal("changed authority accepted", damage)
			}
		})
	}
}

type schemaCrashInput struct {
	Path             string
	Config           Config
	Identity         protocol.Identity
	Operation, Point string
}

func TestManagedSchemaCrashHelper(t *testing.T) {
	input := os.Getenv("DEPLOY_SCHEMA_CRASH_INPUT")
	if input == "" {
		t.Skip("subprocess helper")
	}
	var v schemaCrashInput
	raw, e := os.ReadFile(input)
	must(t, e)
	must(t, json.Unmarshal(raw, &v))
	err := migrateManagedSchema(context.Background(), v.Path, v.Config, v.Identity, v.Operation, 9, func(p string) error {
		if p == v.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("missing crash", err)
}
func TestManagedSchemaMigrationSIGKILL(t *testing.T) {
	for _, point := range []string{"schema_intent", "schema_copy", "schema_backup", "schema_transaction", "schema_committed", "schema_complete", "schema_released"} {
		t.Run(point, func(t *testing.T) {
			path, c, id, op := schemaMigrationFixture(t)
			input := filepath.Join(t.TempDir(), "input.json")
			raw, e := json.Marshal(schemaCrashInput{path, c, id, op, point})
			must(t, e)
			must(t, os.WriteFile(input, raw, 0600))
			executable, e := os.Executable()
			must(t, e)
			cmd := exec.Command(executable, "-test.run=^TestManagedSchemaCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_SCHEMA_CRASH_INPUT="+input)
			out, e := process.LimitedOutput(cmd, 20*time.Second, 8192)
			if e == nil || cmd.ProcessState == nil {
				t.Fatal("missing SIGKILL", e, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong crash", string(out))
			}
			must(t, MigrateManagedSchema(context.Background(), path, c, id, op, 9))
			s, e := OpenManagedVerification(path, c, id)
			must(t, e)
			must(t, s.Close())
		})
	}
}

func TestManagedSchemaMigrationIncludesCommittedWAL(t *testing.T) {
	path, c, id, op := schemaMigrationFixture(t)
	d, e := privateDirectory(path)
	must(t, e)
	defer d.Close()
	db, e := snapshotDB(d, "state.db", false)
	must(t, e)
	defer db.Close()
	_, e = db.Exec("PRAGMA wal_autocheckpoint=0; INSERT INTO metadata(key,value) VALUES('wal-migration-test','committed-before-migration')")
	must(t, e)
	info, e := os.Stat(filepath.Join(path, "state.db-wal"))
	must(t, e)
	if info.Size() == 0 {
		t.Fatal("test requires committed WAL")
	}
	must(t, MigrateManagedSchema(context.Background(), path, c, id, op, 9))
	stage, e := d.Sub("schema-migrations/" + op)
	must(t, e)
	defer stage.Close()
	backup, e := snapshotDB(stage, "before.db", true)
	must(t, e)
	defer backup.Close()
	var value string
	must(t, backup.QueryRow("SELECT value FROM metadata WHERE key='wal-migration-test'").Scan(&value))
	if value != "committed-before-migration" {
		t.Fatal("backup lost committed WAL", value)
	}
}

func TestManagedSchemaCompletionVerification(t *testing.T) {
	for _, damage := range []string{"none", "backup", "completion", "marker", "gate"} {
		t.Run(damage, func(t *testing.T) {
			path, c, id, op := schemaMigrationFixture(t)
			ctx := context.Background()
			must(t, MigrateManagedSchema(ctx, path, c, id, op, 9))
			s, e := OpenManagedVerification(path, c, id)
			must(t, e)
			defer s.Close()
			switch damage {
			case "backup":
				must(t, os.Remove(filepath.Join(path, "schema-migrations", op, "before.db")))
			case "completion":
				must(t, os.Remove(filepath.Join(path, "schema-migrations", op, "complete.json")))
			case "marker":
				_, e = s.db.Exec("DELETE FROM metadata WHERE key=?", "schema-migration:"+op)
				must(t, e)
			case "gate":
				must(t, os.WriteFile(filepath.Join(path, "schema-migration.json"), []byte("{}"), 0600))
			}
			e = VerifyManagedSchemaMigration(ctx, path, c, id, op, 9)
			if (damage == "none") != (e == nil) {
				t.Fatal(damage, e)
			}
			if damage == "completion" {
				if _, e = os.Stat(filepath.Join(path, "schema-migrations", op, "complete.json")); !os.IsNotExist(e) {
					t.Fatal("read-only verification repaired completion")
				}
			}
		})
	}
}
