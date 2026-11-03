package install

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// Model an already installed schema-9 release; ordinary setup uses only the
// current schema. The ELF/probe boundary has its own native executable test.
func downgradeManagedSchemaFixture(t *testing.T, spec ManagedSpec) ManagedManifest {
	t.Helper()
	d, e := privateManagedDir(spec.Root, true)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	j, e := readManagedJournal(d)
	if e != nil {
		t.Fatal(e)
	}
	j.Intent.After.Schema = 9
	if j.Intent.Before != nil {
		t.Fatal("fixture requires initial setup")
	}
	for name, v := range map[string]any{"lifecycle.json": j, "installation.json": j.Intent.After, "operations/" + j.Intent.ID + "/intent.json": j.Intent} {
		if e = d.WriteJSON(name, v, true); e != nil {
			t.Fatal(e)
		}
	}
	db, e := sql.Open("sqlite", "file:"+filepath.Join(spec.State, "state.db")+"?mode=rw")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec("PRAGMA user_version=9"); e != nil {
		t.Fatal(e)
	}
	return j.Intent.After
}
func TestManagedSchemaLifecycle(t *testing.T) {
	spec, i, f, id := managedFixture(t)
	ctx := context.Background()
	if _, e := i.prepare(ctx, "setup", spec); e != nil {
		t.Fatal(e)
	}
	old := downgradeManagedSchemaFixture(t, spec)
	if _, e := i.prepare(ctx, "update", spec); e == nil {
		t.Fatal("implicit schema change accepted")
	}
	m, e := i.prepare(ctx, "migrate-schema", spec)
	if e != nil {
		t.Fatal(e)
	}
	if m.Schema != worker.StorageSchemaVersion() || m.HelperRevision != m.Revision || m.Identity != id || m.ConfigHash != old.ConfigHash || m.Installation != old.Installation || m.Mode != "verification" {
		t.Fatal(m)
	}
	checkManagedInstalled(t, spec, m, id)
	original, e := os.ReadFile(filepath.Join(spec.Root, managedHelperName(old)))
	if e != nil {
		t.Fatal(e)
	}
	replacement, e := os.ReadFile(filepath.Join(spec.Root, managedHelperName(m)))
	if e != nil {
		t.Fatal(e)
	}
	if managedDigest(original) != old.HelperHash || managedDigest(replacement) != m.HelperHash {
		t.Fatal("lost helper pin")
	}
	starts := f.starts
	if _, e = i.resume(ctx, spec.Root); e != nil {
		t.Fatal(e)
	}
	if starts != f.starts {
		t.Fatal("completed retry restarted service")
	}
	if _, e = i.rollback(ctx, spec.Root, old.Revision); e == nil {
		t.Fatal("rollback crossed schema migration")
	}
	// Later ordinary updates keep using the retained compatible helper.
	updated, e := i.prepare(ctx, "update", spec)
	if e != nil {
		t.Fatal(e)
	}
	if updated.HelperRevision != m.HelperRevision {
		t.Fatal("helper unexpectedly replaced")
	}
	if _, e = i.uninstall(ctx, spec.Root); e != nil {
		t.Fatal(e)
	}
}
func TestManagedSchemaLifecycleInterruptions(t *testing.T) {
	for _, point := range []string{"prepared", "stop-returned", "schema-migrated", "stopped", "files", "unit-published", "manifest-published", "reloaded", "started", "complete"} {
		t.Run(point, func(t *testing.T) {
			spec, i, _, id := managedFixture(t)
			ctx := context.Background()
			if _, e := i.prepare(ctx, "setup", spec); e != nil {
				t.Fatal(e)
			}
			downgradeManagedSchemaFixture(t, spec)
			i.fault = func(p string) error {
				if p == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, e := i.prepare(ctx, "migrate-schema", spec); e == nil {
				t.Fatal("missing fault")
			}
			i.fault = nil
			if point != "complete" {
				if _, e := i.uninstall(ctx, spec.Root); e == nil {
					t.Fatal("uninstall bypassed unfinished migration")
				}
			}
			m, e := i.resume(ctx, spec.Root)
			if e != nil {
				t.Fatal(e)
			}
			checkManagedInstalled(t, spec, m, id)
		})
	}
}
func TestManagedSchemaLifecycleRejectsAuthorityDrift(t *testing.T) {
	for _, change := range []string{"identity", "config", "database", "probe", "helper", "unit", "source-schema"} {
		t.Run(change, func(t *testing.T) {
			spec, i, f, _ := managedFixture(t)
			ctx := context.Background()
			if _, e := i.prepare(ctx, "setup", spec); e != nil {
				t.Fatal(e)
			}
			old := downgradeManagedSchemaFixture(t, spec)
			switch change {
			case "identity":
				f.onStop = func() {
					d, e := privateManagedDir(spec.State, true)
					if e != nil {
						t.Fatal(e)
					}
					defer d.Close()
					id := old.Identity
					id.Recovery = "n-00000000000000000000000000000000"
					if e = d.WriteJSON("identity.json", id, true); e != nil {
						t.Fatal(e)
					}
				}
			case "config":
				c, e := ReadRuntime(spec.RuntimeFile)
				if e != nil {
					t.Fatal(e)
				}
				c.Worker.MaxMessages++
				writeRuntime(t, spec.RuntimeFile, c)
			case "database":
				f.onStop = func() {
					if e := os.Remove(filepath.Join(spec.State, "state.db")); e != nil {
						t.Fatal(e)
					}
				}
			case "probe":
				i.probe = func(context.Context, *safefs.Directory, string, string, bool) error {
					return errors.New("incompatible executable")
				}
			case "helper":
				if e := os.WriteFile(filepath.Join(spec.Root, "recovery-helper"), []byte("changed"), 0700); e != nil {
					t.Fatal(e)
				}
			case "unit":
				if e := os.WriteFile(filepath.Join(spec.UnitDirectory, managedUnit), []byte("changed"), 0600); e != nil {
					t.Fatal(e)
				}
			case "source-schema":
				db, e := sql.Open("sqlite", "file:"+filepath.Join(spec.State, "state.db")+"?mode=rw")
				if e != nil {
					t.Fatal(e)
				}
				_, e = db.Exec("PRAGMA user_version=8")
				if e != nil {
					t.Fatal(e)
				}
				db.Close()
			}
			starts := f.starts
			if _, e := i.prepare(ctx, "migrate-schema", spec); e == nil {
				t.Fatal("changed authority accepted")
			}
			if starts != f.starts {
				t.Fatal("invalid migration started service")
			}
		})
	}
}

func TestManagedSchemaCompletedResumeChecksBackup(t *testing.T) {
	spec, i, f, _ := managedFixture(t)
	ctx := context.Background()
	if _, e := i.prepare(ctx, "setup", spec); e != nil {
		t.Fatal(e)
	}
	downgradeManagedSchemaFixture(t, spec)
	if _, e := i.prepare(ctx, "migrate-schema", spec); e != nil {
		t.Fatal(e)
	}
	d, e := privateManagedDir(spec.Root, true)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	j, e := readManagedJournal(d)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(spec.State, "schema-migrations", j.Intent.ID, "before.db")); e != nil {
		t.Fatal(e)
	}
	starts, stops := f.starts, f.stops
	if _, e = i.resume(ctx, spec.Root); e == nil {
		t.Fatal("completed migration ignored missing backup")
	}
	if starts != f.starts || stops != f.stops {
		t.Fatal("completion inspection changed service activity")
	}
}
