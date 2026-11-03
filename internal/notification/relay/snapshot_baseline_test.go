package relay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func baselineSnapshotFixture(t *testing.T, restored bool, tail int, ack bool) (string, string, InboxSnapshotManifest, interaction.SignalRecoveryCursor) {
	t.Helper()
	ctx := context.Background()
	s, path, p := baselineInboxFixture(t, restored)
	for i := 0; i < tail; i++ {
		if err := s.Enqueue(ctx, interaction.SpoolEvent{Key: fmt.Sprintf("new-%d", i), Hash: protocol.Hash(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if ack {
		page, err := s.List(ctx, s.Cursor())
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Ack(ctx, page.Cursor); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	m, err := SnapshotInbox(ctx, path, p.Handoff.Target.Account, artifact)
	if err != nil {
		t.Fatal(err)
	}
	return path, artifact, m, p
}
func TestBaselineSnapshotRoundTrip(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, tail := range []int{0, 2, 20} {
			for _, ack := range []bool{false, true} {
				t.Run(fmt.Sprintf("%t/%d/%t", restored, tail, ack), func(t *testing.T) {
					path, artifact, m, p := baselineSnapshotFixture(t, restored, tail, ack)
					ctx := context.Background()
					if m.Version != 2 || m.Schema != 5 || m.BaselineHash != protocol.Hash(p) || m.BaselinePosition != p.Handoff.Target.Position || m.HistoricalHighest != 3 || m.RetainedEvents != 3+uint64(tail) || m.Highest != m.BaselinePosition+uint64(tail) {
						t.Fatal(m)
					}
					expectedAck := uint64(0)
					if ack && tail > 0 {
						expectedAck = m.BaselinePosition + uint64(min(tail, 16))
					}
					if m.Acknowledged != expectedAck || m.Listed != expectedAck {
						t.Fatal("snapshot invented ACK evidence", m)
					}
					verified, err := VerifyInboxSnapshot(ctx, artifact, m.Account)
					if err != nil || verified != m {
						t.Fatal(verified, err)
					}
					// The backup has all proof evidence; it does not need the live runtime files.
					if err = os.Remove(filepath.Join(path, signalRuntimeFile)); err != nil {
						t.Fatal(err)
					}
					if _, err = VerifyInboxSnapshot(ctx, artifact, m.Account); err != nil {
						t.Fatal("snapshot depends on live runtime proof", err)
					}
				})
			}
		}
	}
}
func TestBaselineSnapshotPreservesLargeReservedRange(t *testing.T) {
	checkBaselineSnapshotBounds(t, 3, 100000, true)
}

func TestBaselineSnapshotEmptyHistory(t *testing.T) {
	for _, floor := range []uint64{0, 5} {
		for _, tail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", floor, tail), func(t *testing.T) {
				checkBaselineSnapshotBounds(t, 0, floor, tail)
			})
		}
	}
}

func checkBaselineSnapshotBounds(t *testing.T, historical int, floor uint64, tail bool) {
	t.Helper()
	ctx := context.Background()
	source, _, ch := restoredExchangeFixture(t, historical)
	manifest, err := source.Capture(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	summary := interaction.SignalRecoverySummary{Challenge: ch, Manifest: manifest, Unmapped: uint64(historical), GatewayHighest: floor}
	if floor > uint64(historical) {
		summary.MissingRelayFrom, summary.MissingRelayThrough = uint64(historical)+1, floor
	}
	if err = source.RecordSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	p := interaction.SignalRecoveryHandoff{Version: 1, Summary: summary, RequestsHash: protocol.Hash("sealed requests"), Before: manifest.Relay, Target: manifest.Relay}
	p.Target.Identity = manifest.Restore.After
	p.Target.Position = floor
	proof, err := source.StageSignalCursor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	s, path, _ := baselineInboxFromCandidate(t, source, proof)
	var fresh uint64
	if tail {
		fresh = 1
		if err = s.Enqueue(ctx, interaction.SpoolEvent{Key: "fresh", Hash: protocol.Hash("fresh")}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	m, err := SnapshotInbox(ctx, path, p.Target.Account, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if m.Highest != floor+fresh || m.RetainedEvents != uint64(historical)+fresh || m.HistoricalHighest != uint64(historical) || m.BaselinePosition != floor {
		t.Fatal(m)
	}
	j, err := PrepareInboxRestore(ctx, path, m.Account, artifact)
	if err != nil {
		t.Fatal(err)
	}
	verifyBaselineRestoreForTest(t, path, j)
}
func rewriteBaselineSnapshotHash(t *testing.T, d *safefs.Directory, m *InboxSnapshotManifest) {
	t.Helper()
	f, err := inboxArtifactFile(d, "inbox.db", inboxSnapshotBudget(m.Schema))
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	m.DatabaseBytes = info.Size()
	m.DatabaseHash, err = safefs.DigestFile(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = d.WriteJSON("snapshot.json", m, true); err != nil {
		t.Fatal(err)
	}
}
func TestBaselineSnapshotRejectsChangedEvidence(t *testing.T) {
	for _, kind := range []string{"version", "schema", "hash", "floor", "count", "prefix", "missing_proof", "handoff", "old_event", "new_gap", "reserved", "ack", "sequence", "sidecar"} {
		t.Run(kind, func(t *testing.T) {
			_, artifact, m, _ := baselineSnapshotFixture(t, true, 3, true)
			ctx := context.Background()
			d, err := privateInboxDirectory(artifact)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			switch kind {
			case "version":
				m.Version = 1
			case "schema":
				m.Schema = 2
			case "hash":
				m.BaselineHash = protocol.Hash("other")
			case "floor":
				m.BaselinePosition++
				m.Highest++
			case "count":
				m.RetainedEvents++
			case "prefix":
				m.HistoricalHighest--
				m.RetainedEvents--
			case "sidecar":
				err = os.WriteFile(filepath.Join(artifact, "inbox.db-wal"), nil, 0600)
			default:
				db, e := inboxArtifactDB(d, "inbox.db", false)
				if e != nil {
					t.Fatal(e)
				}
				switch kind {
				case "missing_proof":
					_, err = db.Exec("DELETE FROM signal_runtime")
				case "handoff":
					_, err = db.Exec("UPDATE signal_cursor SET body='{}'")
				case "old_event":
					_, err = db.Exec("UPDATE events SET hash=? WHERE position=1", protocol.Hash("other"))
				case "new_gap":
					_, err = db.Exec("DELETE FROM events WHERE position=7")
				case "reserved":
					_, err = db.Exec("INSERT INTO events VALUES(4,'forged','hash','{}')")
				case "ack":
					_, err = db.Exec("UPDATE identity SET acked=5")
				case "sequence":
					_, err = db.Exec("UPDATE sqlite_sequence SET seq=9")
				}
				if err != nil {
					t.Fatal(err)
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
				// Check semantic validation as well as the outer file checksum.
				rewriteBaselineSnapshotHash(t, d, &m)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = d.WriteJSON("snapshot.json", m, true); err != nil {
				t.Fatal(err)
			}
			if _, err = VerifyInboxSnapshot(ctx, artifact, m.Account); err == nil {
				t.Fatal("changed evidence accepted")
			}
		})
	}
}
