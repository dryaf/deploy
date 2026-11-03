package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestSnapshotIncludesCommittedWALAndImmutableIdentity(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	_, err := s.Enqueue(ctx, report(*now, "captured", 1, 1, 1))
	must(t, err)
	destination := filepath.Join(t.TempDir(), "snapshot with % and $")
	m, err := s.Snapshot(ctx, destination)
	must(t, err)
	_, err = s.Enqueue(ctx, report(*now, "later", 2, 2, 1))
	must(t, err)
	got, dir, err := readSnapshot(ctx, destination, s.config)
	must(t, err)
	defer dir.Close()
	if m != got || m.Identity != s.Identity() {
		t.Fatal("snapshot identity changed")
	}
	db, err := snapshotDB(dir, "state.db", true)
	must(t, err)
	defer db.Close()
	var n int
	must(t, db.QueryRow("SELECT count(*) FROM messages WHERE id='captured'").Scan(&n))
	if n != 1 {
		t.Fatal("committed WAL data absent")
	}
	must(t, db.QueryRow("SELECT count(*) FROM messages WHERE id='later'").Scan(&n))
	if n != 0 {
		t.Fatal("snapshot changed with live database")
	}
	if _, err = s.Snapshot(ctx, destination); err == nil {
		t.Fatal("snapshot overwritten")
	}
}

func TestSnapshotRejectsCorruptionSidecarsAndConfigDrift(t *testing.T) {
	for _, damage := range []string{"checksum", "sidecar", "config", "permissions", "manifest", "symlink"} {
		t.Run(damage, func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "snapshot")
			_, err := s.Snapshot(ctx, path)
			must(t, err)
			c := s.config
			switch damage {
			case "checksum":
				must(t, os.WriteFile(filepath.Join(path, "state.db"), []byte("corrupt"), 0600))
			case "sidecar":
				must(t, os.WriteFile(filepath.Join(path, "state.db-wal"), []byte("orphan"), 0600))
			case "config":
				c.MaxMessages--
			case "permissions":
				must(t, os.Chmod(filepath.Join(path, "state.db"), 0644))
			case "manifest":
				must(t, os.Remove(filepath.Join(path, "snapshot.json")))
			case "symlink":
				old := filepath.Join(path, "state.db")
				must(t, os.Rename(old, old+".original"))
				must(t, os.Symlink(old+".original", old))
			}
			if _, dir, err := readSnapshot(ctx, path, c); err == nil {
				dir.Close()
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
}

func TestRecoveryGateAndDatabaseMarkerBlockOrdinaryOpen(t *testing.T) {
	for _, mode := range []string{"gate", "malformed", "database"} {
		t.Run(mode, func(t *testing.T) {
			s, _, path := fixture(t)
			if mode == "database" {
				_, err := s.db.Exec("INSERT INTO metadata VALUES('recovery','{}')")
				must(t, err)
			} else {
				body := []byte("{}")
				if mode == "malformed" {
					body = []byte("{")
				}
				must(t, os.WriteFile(filepath.Join(path, "recovery.json"), body, 0600))
			}
			id := s.Identity()
			must(t, s.Close())
			if r, err := Open(path, configuration(), false); err == nil {
				r.Close()
				t.Fatal("recovery fence ignored")
			}
			var got protocol.Identity
			// No fallback identity is created by a failed open.
			body, err := os.ReadFile(filepath.Join(path, "identity.json"))
			must(t, err)
			must(t, protocol.Decode(body, &got, 4096))
			if got != id {
				t.Fatal("identity changed")
			}
		})
	}
}
