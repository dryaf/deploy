package worker

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func legacyFixture(t *testing.T) (string, protocol.Identity) {
	s, now, dir := fixture(t)
	downgradeLEDLayout(t, s)
	must(t, s.Replace(context.Background(), snapshot(*now, 1, 1)))
	_, err := s.Enqueue(context.Background(), report(*now, "old-report", 1, 1, 1))
	must(t, err)
	for _, table := range []string{"fault_transitions", "policy_faults", "policy_support", "interaction_contexts", "command_inbox", "commands", "command_results", "command_replies", "command_reply_attempts", "telegram_cursors", "provider_inbox", "command_rate", "signal_imports", "policy_dependencies", "notification_references", "notification_deliveries", "command_aliases", "gateway_queries", "gateway_pages", "command_admissions"} {
		_, err = s.db.Exec("DROP TABLE " + table)
		must(t, err)
	}
	_, err = s.db.Exec("PRAGMA user_version=1")
	must(t, err)
	identity := s.Identity()
	must(t, s.Close())
	return dir, identity
}

func TestAdministrativeSchemaFenceAndLegacyMigration(t *testing.T) {
	s, _, path := fixture(t)
	ctx := context.Background()
	id, c := s.Identity(), s.config
	_, err := s.db.Exec("PRAGMA user_version=9")
	must(t, err)
	must(t, s.Close())
	if managed, e := OpenManagedVerification(path, c, id); e == nil {
		managed.Close()
		t.Fatal("managed administration silently upgraded the state")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(path, "state.db")+"?mode=rw")
	must(t, err)
	var version int
	must(t, db.QueryRow("PRAGMA user_version").Scan(&version))
	if version != 9 {
		t.Fatal("managed refusal changed storage version", version)
	}
	must(t, db.Close())
	// The existing unmanaged staging upgrade path can migrate an explicitly
	// opened old state, while the managed runtime/helper intersection stays fenced.
	s, err = Open(path, c, false)
	must(t, err)
	defer s.Close()
	must(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	if version != 10 || s.Identity() != id {
		t.Fatal("administrative storage fence missing or identity changed")
	}
	_, err = s.Snapshot(ctx, filepath.Join(t.TempDir(), "new-snapshot"))
	must(t, err)
}

func TestMigrationPreservesLegacyQueueAndIdentity(t *testing.T) {
	dir, id := legacyFixture(t)
	s, err := Open(dir, configuration(), false)
	must(t, err)
	defer s.Close()
	if s.Identity() != id {
		t.Fatal("migration changed identity")
	}
	j, err := s.Get(context.Background(), producer, "old-report")
	must(t, err)
	if j.Message.ID != "old-report" {
		t.Fatal("queue lost")
	}
	var version int
	must(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	if version != currentSchema {
		t.Fatal(version)
	}
}

func TestMigrationFailureRollsBackDDLAndVersion(t *testing.T) {
	dir, _ := legacyFixture(t)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state.db")+"?mode=rw")
	must(t, err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE command_inbox(blocker TEXT)")
	must(t, err)
	if s, err := Open(dir, configuration(), false); err == nil {
		s.Close()
		t.Fatal("conflicting migration succeeded")
	}
	var version, count int
	must(t, db.QueryRow("PRAGMA user_version").Scan(&version))
	must(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='fault_transitions'").Scan(&count))
	if version != 1 || count != 0 {
		t.Fatal("partial migration survived", version, count)
	}
	_, err = db.Exec("DROP TABLE command_inbox")
	must(t, err)
	s, err := Open(dir, configuration(), false)
	must(t, err)
	s.Close()
}

func TestMigrationFromInteractiveV3PreservesCommandsAndRateHistory(t *testing.T) {
	s, now, path, in := commandFixture(t)
	downgradeLEDLayout(t, s)
	v, err := s.AdmitCommand(context.Background(), in)
	must(t, err)
	for _, table := range []string{"notification_references", "notification_deliveries", "command_aliases", "gateway_queries", "gateway_pages", "command_admissions"} {
		_, err = s.db.Exec("DROP TABLE " + table)
		must(t, err)
	}
	_, err = s.db.Exec("DROP INDEX reply_native_lookup; PRAGMA user_version=3")
	must(t, err)
	c := s.config
	must(t, s.Close())
	r, err := Open(path, c, false)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return *now }
	current := getCommand(t, r, v.Command.ID)
	if protocol.Hash(current) != protocol.Hash(v) {
		t.Fatal("migration rewrote command authority")
	}
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM command_admissions WHERE request_key=?", in.RequestKey()).Scan(&count))
	if count != 1 {
		t.Fatal("migration forgot recent admission")
	}
}

// Tests constructing pre-v7 fixtures must remove the v7-only columns and pin.
func downgradeLEDLayout(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.db.Exec("DROP TABLE led_migration_operations; DROP TABLE led_migration_heads; DROP TABLE led_migrations; DROP TABLE led_operations; DROP TABLE led_operation_heads; DROP INDEX led_binding_states; ALTER TABLE led_states DROP COLUMN binding; ALTER TABLE led_states DROP COLUMN identity; DELETE FROM metadata WHERE key='led-layout'")
	must(t, err)
}
