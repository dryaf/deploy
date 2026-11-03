package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/safefs"
)

type lostClearLED struct {
	*fakeLED
	id   string
	lost bool
}

func (f *lostClearLED) Clear(ctx context.Context, id string) error {
	err := f.fakeLED.Clear(ctx, id)
	if id == f.id && !f.lost {
		f.lost = true
		return errors.New("lost clear acknowledgement")
	}
	return err
}

func TestLEDBindingLostClearAcknowledgementSurvivesRestart(t *testing.T) {
	s, now, path := bindingFixture(t, binding("selected", "backup"))
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	api := &lostClearLED{fakeLED: &fakeLED{states: map[string]led.State{}}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	id := bindingFault(t, s, "selected", "critical")
	api.id = id
	v.Sequence, v.BasisRevision = 2, 2
	v.Subjects[0].Condition = "ok"
	must(t, s.Replace(ctx, v))
	if r.Step(ctx) == nil {
		t.Fatal("uncertain clear was acknowledged")
	}
	var obsolete int
	must(t, s.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", id).Scan(&obsolete))
	if obsolete != 1 {
		t.Fatal("clear tombstone lost")
	}
	c := s.config
	must(t, s.Close())
	reopened, err := Open(path, c, false)
	must(t, err)
	defer reopened.Close()
	reopened.now = func() time.Time { return *now }
	r = LEDReconciler{Store: reopened, API: api, Budget: 2 * time.Second}
	// A fresh observation is needed before resuming destruction of old faults.
	clears := len(api.cleared)
	must(t, r.Step(ctx))
	must(t, reopened.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", id).Scan(&obsolete))
	for _, cleared := range api.cleared[clears:] {
		if cleared == id {
			t.Fatal("restart resumed fault cleanup without fresh evidence")
		}
	}
	v.Sequence, v.BasisRevision = 3, 3
	must(t, reopened.Replace(ctx, v))
	must(t, r.Step(ctx))
	if err = reopened.db.QueryRow("SELECT obsolete FROM led_states WHERE id=?", id).Scan(&obsolete); err != sql.ErrNoRows {
		t.Fatal("acknowledged tombstone not retired", err)
	}
	v.Sequence, v.BasisRevision = 4, 4
	v.Subjects[0].Condition = "critical"
	must(t, reopened.Replace(ctx, v))
	must(t, r.Step(ctx))
	if bindingFault(t, reopened, "selected", "critical") == id {
		t.Fatal("clear replay can target new fault")
	}
}

func TestLEDLegacyMigrationRetainsFaultUntilFreshResolution(t *testing.T) {
	s, now, path := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	const oldID = "deploy.old-fault"
	_, err := s.db.Exec("INSERT INTO led_states(id,condition) VALUES(?,'critical')", oldID)
	must(t, err)
	downgradeLEDLayout(t, s)
	_, err = s.db.Exec("PRAGMA user_version=6")
	must(t, err)
	must(t, s.Close())
	reopened, err := Open(path, configuration(), false)
	must(t, err)
	defer reopened.Close()
	reopened.now = func() time.Time { return *now }
	api := &fakeLED{states: map[string]led.State{oldID: {ID: oldID, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}}}
	r := LEDReconciler{Store: reopened, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	if _, exists := api.states[oldID]; !exists {
		t.Fatal("migration cleared legacy fault before fresh evidence")
	}
	must(t, reopened.Replace(ctx, snapshot(*now, 2, 2)))
	must(t, r.Step(ctx))
	if _, exists := api.states[oldID]; exists {
		t.Fatal("known legacy fault could not be retired")
	}
}

func TestLEDLegacySnapshotRemainsReadableAndQuarantined(t *testing.T) {
	for _, version := range []int{6, 7, 8} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s, _, path := fixture(t)
			ctx := context.Background()
			artifact := filepath.Join(t.TempDir(), "snapshot")
			m, err := s.Snapshot(ctx, artifact)
			must(t, err)
			dir, err := safefs.Open(artifact)
			must(t, err)
			defer dir.Close()
			db, err := snapshotDB(dir, "state.db", false)
			must(t, err)
			ddl := "DROP TABLE led_migration_operations; DROP TABLE led_migration_heads; DROP TABLE led_migrations; PRAGMA user_version=8"
			if version < 8 {
				ddl += "; DROP TABLE led_operations; DROP TABLE led_operation_heads; PRAGMA user_version=7"
			}
			if version == 6 {
				ddl += "; DROP INDEX led_binding_states; ALTER TABLE led_states DROP COLUMN binding; ALTER TABLE led_states DROP COLUMN identity; DELETE FROM metadata WHERE key='led-layout'; PRAGMA user_version=6"
			}
			_, err = db.Exec(ddl)
			must(t, err)
			must(t, db.Close())
			f, err := snapshotFile(dir, "state.db", s.config.MaxDatabaseBytes)
			must(t, err)
			info, err := f.Stat()
			must(t, err)
			m.DatabaseHash, err = safefs.DigestFile(f)
			must(t, err)
			must(t, f.Close())
			m.Schema, m.DatabaseBytes = version, info.Size()
			must(t, dir.WriteJSON("snapshot.json", m, true))
			verified, err := VerifySnapshot(ctx, artifact, s.config)
			must(t, err)
			if verified != m {
				t.Fatal("legacy snapshot changed")
			}
			c := s.config
			must(t, s.Close())
			j, err := prepareSnapshotRecovery(ctx, path, artifact, c, nil)
			must(t, err)
			if j.Snapshot.Schema != version || j.Phase != "quarantined" {
				t.Fatal(j)
			}
			if opened, err := Open(path, c, false); err == nil {
				opened.Close()
				t.Fatal("legacy quarantine activated")
			}

		})
	}
}

func TestLEDLayoutMigrationRollsBackAsOneTransaction(t *testing.T) {
	s, _, path := fixture(t)
	downgradeLEDLayout(t, s)
	_, err := s.db.Exec("PRAGMA user_version=6; CREATE INDEX led_binding_states ON led_states(condition)")
	must(t, err)
	must(t, s.Close())
	if opened, err := Open(path, configuration(), false); err == nil {
		opened.Close()
		t.Fatal("conflicting migration succeeded")
	}
	dir, err := safefs.Open(path)
	must(t, err)
	defer dir.Close()
	db, err := snapshotDB(dir, "state.db", false)
	must(t, err)
	defer db.Close()
	var version, columns int
	must(t, db.QueryRow("PRAGMA user_version").Scan(&version))
	must(t, db.QueryRow("SELECT count(*) FROM pragma_table_info('led_states') WHERE name IN ('binding','identity')").Scan(&columns))
	if version != 6 || columns != 0 {
		t.Fatal("partial LED migration survived", version, columns)
	}
	_, err = db.Exec("DROP INDEX led_binding_states")
	must(t, err)
	must(t, db.Close())
	reopened, err := Open(path, configuration(), false)
	must(t, err)
	reopened.Close()
}
