package relay

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func cursorRelayFixture(t *testing.T, restored bool) (interaction.SignalCursorSource, string, interaction.SignalRecoveryHandoff) {
	t.Helper()
	source, path, p := handoffRelayFixture(t, restored)
	if _, err := source.PrepareSignalHandoff(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return source.(interaction.SignalCursorSource), path, p
}
func cursorRelayDirectory(t *testing.T, source interaction.SignalCursorSource) *safefs.Directory {
	t.Helper()
	switch s := source.(type) {
	case *RecoveryInbox:
		return s.inbox.dir
	case *RestoredRecoveryInbox:
		return s.stage
	}
	t.Fatal("unknown source")
	return nil
}
func TestSignalCursorRelayCandidate(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			source, path, p := cursorRelayFixture(t, restored)
			ctx := context.Background()
			proof, err := source.StageSignalCursor(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if err = proof.Validate(); err != nil {
				t.Fatal(err)
			}
			d := cursorRelayDirectory(t, source)
			db, err := inboxArtifactDB(d, signalCursorDatabase, true)
			if err != nil {
				t.Fatal(err)
			}
			if err = verifySignalCursorCandidate(ctx, db, p); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if restored && proof.Handoff.Target.Position != 5 {
				t.Fatal("missing suffix positions reused")
			}
			// The staged sequence reserves the gap: a first new event cannot reuse it.
			// Perform this in a throwaway copy, preserving the immutable candidate.
			copyPath := t.TempDir()
			if err = os.Chmod(copyPath, 0700); err != nil {
				t.Fatal(err)
			}
			copyDir, err := privateInboxDirectory(copyPath)
			if err != nil {
				t.Fatal(err)
			}
			defer copyDir.Close()
			if err = copyInboxArtifact(d, signalCursorDatabase, copyDir, "copy.db", InboxSnapshotManifest{DatabaseBytes: proof.DatabaseBytes, DatabaseHash: proof.DatabaseHash}); err != nil {
				t.Fatal(err)
			}
			copyDB, err := inboxArtifactDB(copyDir, "copy.db", false)
			if err != nil {
				t.Fatal(err)
			}
			result, err := copyDB.Exec("INSERT INTO events(key,hash,body) VALUES('fresh','hash','{}')")
			if err != nil {
				t.Fatal(err)
			}
			id, err := result.LastInsertId()
			if err != nil || uint64(id) != p.Target.Position+1 {
				t.Fatal("baseline reused", id, err)
			}
			copyDB.Close()
			if old, ok := source.(*RecoveryInbox); ok {
				var ack, listed, seq uint64
				if err = old.inbox.db.QueryRow("SELECT acked,listed,(SELECT seq FROM sqlite_sequence WHERE name='events') FROM identity").Scan(&ack, &listed, &seq); err != nil {
					t.Fatal(err)
				}
				if ack != p.Summary.Manifest.Acknowledged || listed != p.Summary.Manifest.Listed || seq != p.Summary.Manifest.Highest {
					t.Fatal("live relay watermarks changed")
				}
			}
			if err = source.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := reopenHandoffRelay(ctx, path, p.Target.Account, restored)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err := reopened.(interaction.SignalCursorSource).StageSignalCursor(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if protocol.Hash(got) != protocol.Hash(proof) {
				t.Fatal("candidate changed after reopen")
			}
		})
	}
}
func TestSignalCursorRelayRejectsDamage(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, kind := range []string{"missing_file", "database", "intent", "manifest", "handoff", "cancel"} {
			t.Run(fmt.Sprintf("%t/%s", restored, kind), func(t *testing.T) {
				source, _, p := cursorRelayFixture(t, restored)
				ctx := context.Background()
				proof, err := source.StageSignalCursor(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				d := cursorRelayDirectory(t, source)
				switch kind {
				case "missing_file":
					err = os.Remove(filepath.Join(d.File().Name(), signalCursorManifest))
				case "database":
					var f *os.File
					f, err = d.Open(signalCursorDatabase, os.O_WRONLY, 0)
					if err == nil {
						_, err = f.WriteAt([]byte("bad"), 0)
						f.Close()
					}
				case "intent":
					p.RequestsHash = protocol.Hash("other")
					err = d.WriteJSON(signalCursorIntent, p, true)
				case "manifest":
					proof.DatabaseHash = protocol.Hash("other")
					err = d.WriteJSON(signalCursorManifest, proof, true)
				case "handoff":
					p.RequestsHash = protocol.Hash("other")
				case "cancel":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err = source.StageSignalCursor(ctx, p); err == nil {
					t.Fatal("damaged candidate accepted")
				}
			})
		}
	}
}
func TestSignalCursorRelayLostResponses(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, stage := range []string{"signal_cursor_source_intent", "signal_cursor_source_transaction", "signal_cursor_source_commit", "signal_cursor_source_manifest", "signal_cursor_source_published"} {
			t.Run(fmt.Sprintf("%t/%s", restored, stage), func(t *testing.T) {
				source, path, p := cursorRelayFixture(t, restored)
				ctx := context.Background()
				fault := func(at string) error {
					if at == stage {
						return fmt.Errorf("lost response")
					}
					return nil
				}
				switch s := source.(type) {
				case *RecoveryInbox:
					s.fault = fault
				case *RestoredRecoveryInbox:
					s.fault = fault
				}
				if _, err := source.StageSignalCursor(ctx, p); err == nil {
					t.Fatal("missing fault")
				}
				if err := source.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := reopenHandoffRelay(ctx, path, p.Target.Account, restored)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				source = reopened.(interaction.SignalCursorSource)
				a, err := source.StageSignalCursor(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				b, err := source.StageSignalCursor(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				if protocol.Hash(a) != protocol.Hash(b) {
					t.Fatal("restart changed candidate")
				}
			})
		}
	}
}
func TestSignalCursorExhaustedSequence(t *testing.T) {
	_, _, p := cursorRelayFixture(t, true)
	p.Target.Position = math.MaxInt64
	p.Summary.GatewayHighest = math.MaxInt64
	p.Summary.MissingRelayThrough = math.MaxInt64
	proof := interaction.SignalRecoveryCursor{Version: 1, Handoff: p, DatabaseBytes: 4096, DatabaseHash: protocol.Hash("db")}
	if err := proof.Validate(); err == nil {
		t.Fatal("exhausted sequence accepted")
	}
}

func TestSignalCursorRelayRechecksIntentBeforePublication(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			source, _, p := cursorRelayFixture(t, restored)
			fault := func(at string) error {
				if at == "signal_cursor_source_manifest" {
					bad := p
					bad.RequestsHash = protocol.Hash("other")
					return cursorRelayDirectory(t, source).WriteJSON(signalCursorIntent, bad, true)
				}
				return nil
			}
			switch s := source.(type) {
			case *RecoveryInbox:
				s.fault = fault
			case *RestoredRecoveryInbox:
				s.fault = fault
			}
			if _, err := source.StageSignalCursor(context.Background(), p); err == nil {
				t.Fatal("changed intent published")
			}
		})
	}
}
func TestSignalCursorRelayEmptyHistory(t *testing.T) {
	r, _, ch := relayRecoveryFixture(t, 0)
	ctx := context.Background()
	m, err := r.Capture(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	summary := interaction.SignalRecoverySummary{Challenge: ch, Manifest: m}
	if err = r.RecordSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	p := interaction.SignalRecoveryHandoff{Version: 1, Summary: summary, RequestsHash: protocol.Hash("empty requests"), Before: m.Relay, Target: m.Relay}
	proof, err := r.StageSignalCursor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Handoff.Target.Position != 0 {
		t.Fatal("empty baseline changed")
	}
	if err = verifySignalCursorFiles(ctx, r.inbox.dir, proof); err != nil {
		t.Fatal(err)
	}
}

func TestSignalCursorCandidateCannotStartRelay(t *testing.T) {
	source, _, p := cursorRelayFixture(t, true)
	ctx := context.Background()
	proof, err := source.StageSignalCursor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
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
	if err = d.WriteJSON("identity.json", inboxPin{Identity: p.Target.Identity, Account: p.Target.Account}, false); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "inbox.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Even in a new directory with no root restore marker, the staged schema
	// cannot become an operational inbox by merely copying it into place.
	if live, e := OpenInbox(path, p.Target.Account, false); e == nil {
		live.Close()
		t.Fatal("candidate activated as live relay")
	}
}
func TestSignalCursorRelayChecksLeaseDuringCopy(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			source, path, p := cursorRelayFixture(t, restored)
			fault := func(at string) error {
				if at == "signal_cursor_source_transaction" {
					if err := os.Rename(filepath.Join(path, "inbox.lock"), filepath.Join(path, "old.lock")); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(path, "inbox.lock"), nil, 0600)
				}
				return nil
			}
			switch s := source.(type) {
			case *RecoveryInbox:
				s.fault = fault
			case *RestoredRecoveryInbox:
				s.fault = fault
			}
			if _, err := source.StageSignalCursor(context.Background(), p); err == nil {
				t.Fatal("lost lease committed candidate")
			}
		})
	}
}

func TestSignalCursorRelayRefusesUnownedArtifacts(t *testing.T) {
	for _, name := range []string{signalCursorDatabase, signalCursorManifest, signalCursorDatabase + "-journal", signalCursorDatabase + "-wal", signalCursorDatabase + "-shm"} {
		t.Run(name, func(t *testing.T) {
			source, _, p := cursorRelayFixture(t, false)
			d := cursorRelayDirectory(t, source)
			file, err := d.Open(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = file.Write([]byte("unowned")); err != nil {
				t.Fatal(err)
			}
			file.Close()
			if _, err = source.StageSignalCursor(context.Background(), p); err == nil {
				t.Fatal("unowned artifact adopted")
			}
			data, err := os.ReadFile(filepath.Join(d.File().Name(), name))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "unowned" {
				t.Fatal("unowned artifact changed")
			}
			var intent interaction.SignalRecoveryHandoff
			if err = d.ReadJSON(signalCursorIntent, &intent); !os.IsNotExist(err) {
				t.Fatal("unowned artifact claimed", err)
			}
		})
	}
}
