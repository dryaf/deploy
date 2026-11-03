package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Simulates the file/SQL publication a future managed activator must perform.
// This is deliberately test-only; staging APIs cannot publish runtime authority.
func baselineInboxFixture(t *testing.T, restored bool) (*Inbox, string, interaction.SignalRecoveryCursor) {
	t.Helper()
	ctx := context.Background()
	source, _, p := cursorRelayFixture(t, restored)
	proof, err := source.StageSignalCursor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	return baselineInboxFromCandidate(t, source, proof)
}
func baselineInboxFromCandidate(t *testing.T, source interaction.SignalCursorSource, proof interaction.SignalRecoveryCursor) (*Inbox, string, interaction.SignalRecoveryCursor) {
	t.Helper()
	ctx := context.Background()
	p := proof.Handoff
	var err error
	path := t.TempDir()
	if err = os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	d, err := privateInboxDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = copyInboxArtifact(cursorRelayDirectory(t, source), signalCursorDatabase, d, "inbox.db", InboxSnapshotManifest{DatabaseBytes: proof.DatabaseBytes, DatabaseHash: proof.DatabaseHash}); err != nil {
		t.Fatal(err)
	}
	db, err := inboxArtifactDB(d, "inbox.db", false)
	if err != nil {
		t.Fatal(err)
	}
	pin := inboxPin{Identity: p.Target.Identity, Account: p.Target.Account, Baseline: protocol.Hash(proof)}
	pinJSON, _ := json.Marshal(pin)
	proofJSON, _ := json.Marshal(proof)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("CREATE TABLE signal_runtime(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL);PRAGMA user_version=5;"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("INSERT INTO signal_runtime VALUES(1,?)", string(proofJSON)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE identity SET body=?", string(pinJSON)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = d.WriteJSON("identity.json", pin, false); err != nil {
		t.Fatal(err)
	}
	if err = d.WriteJSON(signalRuntimeFile, proof, false); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "inbox.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	inbox, err := OpenInbox(path, p.Target.Account, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inbox.Close() })
	return inbox, path, proof
}
func TestInboxBaselineListAckAndRestart(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			s, path, proof := baselineInboxFixture(t, restored)
			ctx := context.Background()
			start := proof.Handoff.Target
			if s.Cursor() != start {
				t.Fatal("starting cursor not at baseline")
			}
			page, err := s.List(ctx, start)
			if err != nil || len(page.Events) != 0 {
				t.Fatal("historical events replayed", err)
			}
			if err = s.Ack(ctx, start); err != nil {
				t.Fatal(err)
			}
			var ack, listed uint64
			if err = s.db.QueryRow("SELECT acked,listed FROM identity").Scan(&ack, &listed); err != nil {
				t.Fatal(err)
			}
			if ack != 0 || listed != 0 {
				t.Fatal("baseline forged ACK/listed evidence")
			}
			stale := start
			stale.Position--
			if _, err = s.List(ctx, stale); err == nil {
				t.Fatal("cursor before baseline listed")
			}
			if err = s.Ack(ctx, stale); err == nil {
				t.Fatal("cursor before baseline acknowledged")
			}
			event := interaction.SpoolEvent{Key: "fresh", Hash: protocol.Hash("fresh")}
			if err = s.Enqueue(ctx, event); err != nil {
				t.Fatal(err)
			}
			page, err = s.List(ctx, start)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Events) != 1 || page.Cursor.Position != start.Position+1 {
				t.Fatal("reserved position reused", page)
			}
			again, err := s.List(ctx, start)
			if err != nil || protocol.Hash(again) != protocol.Hash(page) {
				t.Fatal("lost list response changed", err)
			}
			ahead := page.Cursor
			ahead.Position++
			if err = s.Ack(ctx, ahead); err == nil {
				t.Fatal("unlisted event acknowledged")
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = OpenInbox(path, start.Account, false)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			again, err = s.List(ctx, start)
			if err != nil || protocol.Hash(again) != protocol.Hash(page) {
				t.Fatal("restart changed replay", err)
			}
			if err = s.Ack(ctx, page.Cursor); err != nil {
				t.Fatal(err)
			}
			if err = s.Ack(ctx, page.Cursor); err != nil {
				t.Fatal("ACK replay", err)
			}
			if _, err = s.List(ctx, start); err == nil {
				t.Fatal("ACK boundary rewound")
			}
			if err = s.Enqueue(ctx, event); err != nil {
				t.Fatal(err)
			}
			again, err = s.List(ctx, page.Cursor)
			if err != nil || len(again.Events) != 0 {
				t.Fatal("native identity replay allocated new position", err)
			}
		})
	}
}
func TestInboxBaselineQueueCapacity(t *testing.T) {
	s, _, p := baselineInboxFixture(t, true)
	ctx := context.Background()
	for i := 0; i < 1024; i++ {
		if err := s.Enqueue(ctx, interaction.SpoolEvent{Key: fmt.Sprintf("new-%d", i), Hash: protocol.Hash(i)}); err != nil {
			t.Fatalf("history consumed pending capacity at %d: %v", i, err)
		}
	}
	if err := s.Enqueue(ctx, interaction.SpoolEvent{Key: "overflow", Hash: protocol.Hash("overflow")}); err == nil {
		t.Fatal("unbounded pending queue")
	}
	if err := s.CanReceive(ctx); err == nil {
		t.Fatal("full receive batch allowed")
	}
	page, err := s.List(ctx, p.Handoff.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 16 {
		t.Fatal("batch bound changed")
	}
	if err = s.Ack(ctx, page.Cursor); err != nil {
		t.Fatal(err)
	}
	if err = s.CanReceive(ctx); err != nil {
		t.Fatal("ACK did not release capacity", err)
	}
}
func TestInboxBaselineRefusesDamagedAuthority(t *testing.T) {
	for _, kind := range []string{"file", "missing_file", "pin", "sql_pin", "proof", "handoff", "table", "schema", "sequence", "ack", "listed", "root_fence", "lock", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, path, p := baselineInboxFixture(t, true)
			ctx := context.Background()
			var err error
			switch kind {
			case "file":
				p.DatabaseHash = protocol.Hash("other")
				err = s.dir.WriteJSON(signalRuntimeFile, p, true)
			case "missing_file":
				err = os.Remove(filepath.Join(path, signalRuntimeFile))
			case "pin":
				err = s.dir.WriteJSON("identity.json", inboxPin{Identity: s.identity, Account: s.account}, true)
			case "sql_pin":
				_, err = s.db.Exec("UPDATE identity SET body='{}'")
			case "proof":
				_, err = s.db.Exec("UPDATE signal_runtime SET body='{}'")
			case "handoff":
				_, err = s.db.Exec("UPDATE signal_cursor SET body='{}'")
			case "table":
				_, err = s.db.Exec("DROP TABLE signal_runtime")
			case "schema":
				_, err = s.db.Exec("PRAGMA user_version=1")
			case "sequence":
				_, err = s.db.Exec("UPDATE sqlite_sequence SET seq=0")
			case "ack":
				_, err = s.db.Exec("UPDATE identity SET acked=?", p.Handoff.Target.Position)
			case "listed":
				_, err = s.db.Exec("UPDATE identity SET listed=?", p.Handoff.Target.Position)
			case "root_fence":
				err = s.dir.WriteJSON("restore.json", map[string]string{"phase": "quarantined"}, false)
			case "lock":
				if err = os.Rename(filepath.Join(path, "inbox.lock"), filepath.Join(path, "old.lock")); err == nil {
					err = os.WriteFile(filepath.Join(path, "inbox.lock"), nil, 0600)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.CheckActive(ctx); err == nil {
				t.Fatal("damaged relay kept sending")
			}
			if _, err = s.List(ctx, p.Handoff.Target); err == nil {
				t.Fatal("damaged relay kept listing")
			}
			if err = s.Enqueue(ctx, interaction.SpoolEvent{Key: "new", Hash: protocol.Hash("new")}); err == nil {
				t.Fatal("damaged relay kept receiving")
			}
		})
	}
}
func TestInboxBaselineHistoricalAndNewEventIntegrity(t *testing.T) {
	for _, kind := range []string{"history", "gap", "reserved", "body"} {
		t.Run(kind, func(t *testing.T) {
			s, path, p := baselineInboxFixture(t, true)
			ctx := context.Background()
			for i := 0; i < 3; i++ {
				if err := s.Enqueue(ctx, interaction.SpoolEvent{Key: fmt.Sprintf("new-%d", i), Hash: protocol.Hash(i)}); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch kind {
			case "history":
				_, err = s.db.Exec("UPDATE events SET hash=? WHERE position=1", protocol.Hash("other"))
			case "gap":
				_, err = s.db.Exec("DELETE FROM events WHERE position=?", p.Handoff.Target.Position+2)
			case "reserved":
				_, err = s.db.Exec("INSERT INTO events VALUES(4,'missing','hash','{}')")
			case "body":
				_, err = s.db.Exec("UPDATE events SET body='{}' WHERE position=?", p.Handoff.Target.Position+1)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "gap" || kind == "body" {
				if _, err = s.List(ctx, p.Handoff.Target); err == nil {
					t.Fatal("corrupted new event listed")
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, e := OpenInbox(path, p.Handoff.Target.Account, false); e == nil {
				reopened.Close()
				t.Fatal("corrupted stream reopened")
			}
		})
	}
}
func TestInboxBaselineRecoveryExportAndSnapshot(t *testing.T) {
	s, path, p := baselineInboxFixture(t, true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := OpenRecoveryInbox(path, p.Handoff.Target.Account)
	if err != nil {
		t.Fatal(err)
	}
	if source.Cursor().Position != 0 {
		t.Fatal("export identity claims a cursor")
	}
	source.Close()
	if m, err := SnapshotInbox(context.Background(), path, p.Handoff.Target.Account, filepath.Join(t.TempDir(), "snapshot")); err != nil || m.Version != 2 || m.RetainedEvents != 3 || m.Highest != 5 {
		t.Fatal("baseline snapshot lost gap", m, err)
	}
	reopened, err := OpenInbox(path, p.Handoff.Target.Account, false)
	if err != nil {
		t.Fatal("failed recovery damaged runtime", err)
	}
	reopened.Close()
}

func TestInboxBaselineLostStateCannotBeReinitialized(t *testing.T) {
	s, path, p := baselineInboxFixture(t, true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"inbox.db", "identity.json"} {
		if err := os.Remove(filepath.Join(path, name)); err != nil {
			t.Fatal(err)
		}
	}
	if recreated, err := OpenInbox(path, p.Handoff.Target.Account, true); err == nil {
		recreated.Close()
		t.Fatal("lost recovered inbox was reinitialized")
	}
	for _, name := range []string{"inbox.db", "identity.json"} {
		if _, err := os.Stat(filepath.Join(path, name)); !os.IsNotExist(err) {
			t.Fatal("initialization changed recovery evidence", name, err)
		}
	}
}
