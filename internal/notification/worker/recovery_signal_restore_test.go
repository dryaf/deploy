package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
)

func prepareRestoredSignalSource(t *testing.T, f signalRecoveryFixture) (*relay.RestoredRecoveryInbox, relay.InboxRestoreState) {
	t.Helper()
	ctx := context.Background()
	account := f.source.Cursor().Account
	must(t, f.source.Close())
	j, err := relay.PrepareInboxRestore(ctx, f.relayPath, account, f.relayArtifact)
	must(t, err)
	source, err := relay.OpenRestoredRecoveryInbox(ctx, f.relayPath, account)
	must(t, err)
	t.Cleanup(func() { source.Close() })
	return source, j
}

func TestRestoredSignalHistoryExchange(t *testing.T) {
	for _, highest := range []int{0, 2, 4, 6} {
		for _, lost := range []string{"none", "import", "finish"} {
			if highest == 0 && lost == "import" {
				continue
			}
			t.Run(fmt.Sprintf("relay_%d/%s", highest, lost), func(t *testing.T) {
				f := newSignalRecoveryFixtureWithRelaySnapshot(t, highest)
				source, j := prepareRestoredSignalSource(t, f)
				ctx := context.Background()
				if highest < 4 {
					// The original command mapping and later gateway rows are
					// evidence, even when absent from the older relay snapshot.
					var n int
					must(t, f.r.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE id>?", highest).Scan(&n))
					if n != 4-highest {
						t.Fatal(n)
					}
				}
				if lost != "none" {
					broken := &lostSignalRecovery{SignalRecoverySession: f.r, stage: lost}
					if _, err := interaction.ReconcileSignal(ctx, source, broken); err == nil {
						t.Fatal("missing lost response")
					}
				}
				summary, err := interaction.ReconcileSignal(ctx, source, f.r)
				must(t, err)
				must(t, summary.Validate())
				if summary.Manifest.Highest != uint64(highest) || summary.Manifest.Restore.After != j.After ||
					summary.Manifest.Restore.Operation != j.Operation || summary.Manifest.Relay.Identity != j.Before ||
					summary.GatewayHighest != 4 || summary.IntakeEnabled || summary.HistoryComplete {
					t.Fatal(summary)
				}
				if highest < 4 {
					if summary.MissingRelayFrom != uint64(highest+1) || summary.MissingRelayThrough != 4 {
						t.Fatal(summary)
					}
				} else if summary.MissingRelayFrom != 0 || summary.MissingRelayThrough != 0 {
					t.Fatal(summary)
				}
				var commands, events, freshMappings int
				must(t, f.r.db.QueryRow("SELECT count(*) FROM commands").Scan(&commands))
				must(t, f.r.db.QueryRow("SELECT count(*) FROM provider_inbox").Scan(&events))
				must(t, f.r.db.QueryRow("SELECT count(*) FROM command_inbox WHERE request_key=?", f.unknown.RequestKey()).Scan(&freshMappings))
				if commands != 1 || events != max(4, highest) || freshMappings != 0 {
					t.Fatal(commands, events, freshMappings)
				}
				var id, raw string
				must(t, f.r.db.QueryRow("SELECT command_id FROM command_inbox WHERE request_key=?", f.first.RequestKey()).Scan(&id))
				if id != f.first.CommandID() {
					t.Fatal("old mapping changed")
				}
				must(t, f.r.db.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", source.Cursor().Account).Scan(&raw))
				var cursor interaction.SpoolCursor
				must(t, protocol.Decode([]byte(raw), &cursor, 4096))
				if cursor.Identity != j.Before || cursor.Position != 4 {
					t.Fatal("operational cursor changed", cursor)
				}
				must(t, source.Close())
				must(t, f.r.Close())
				resumed, err := relay.ResumeInboxRestore(ctx, f.relayPath, cursor.Account)
				must(t, err)
				if resumed.After != j.After || resumed.CandidateHash != j.CandidateHash || resumed.GatewayExport != summary.Challenge.Operation {
					t.Fatal("candidate changed during exchange", resumed)
				}
				source, err = relay.OpenRestoredRecoveryInbox(ctx, f.relayPath, cursor.Account)
				must(t, err)
				defer source.Close()
				session, err := OpenSignalRecoverySession(ctx, f.path, f.r.config)
				must(t, err)
				defer session.Close()
				again, err := interaction.ReconcileSignal(ctx, source, session)
				must(t, err)
				if protocol.Hash(again) != protocol.Hash(summary) {
					t.Fatal("completion changed on restart")
				}
				if normal, err := relay.OpenInbox(f.relayPath, cursor.Account, false); err == nil {
					normal.Close()
					t.Fatal("relay intake reopened")
				}
				if normal, err := Open(f.path, f.r.config, false); err == nil {
					normal.Close()
					t.Fatal("gateway intake reopened")
				}
			})
		}
	}
}

func TestRestoredSignalRejectsConflictsAndChangedBinding(t *testing.T) {
	for _, mode := range []string{"event", "epoch", "manifest", "lost_completion"} {
		t.Run(mode, func(t *testing.T) {
			f := newSignalRecoveryFixtureWithRelaySnapshot(t, 2)
			source, j := prepareRestoredSignalSource(t, f)
			ctx := context.Background()
			switch mode {
			case "event":
				_, err := f.r.db.Exec("UPDATE provider_inbox SET hash=? WHERE id=1", protocol.Hash("conflict"))
				must(t, err)
				if _, err = interaction.ReconcileSignal(ctx, source, f.r); err == nil {
					t.Fatal("conflict accepted")
				}
				var n int
				must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_events").Scan(&n))
				if n != 0 {
					t.Fatal("partial page committed")
				}
			case "epoch":
				wrong := source.Cursor()
				wrong.Identity = j.After
				if _, err := f.r.SignalChallenge(ctx, wrong); err == nil {
					t.Fatal("new epoch adopted without handoff")
				}
			case "manifest":
				ch, err := f.r.SignalChallenge(ctx, source.Cursor())
				must(t, err)
				m, err := source.Capture(ctx, ch)
				must(t, err)
				must(t, f.r.BeginSignal(ctx, ch, m))
				m.Restore.After.Recovery = "another-epoch"
				if err = f.r.BeginSignal(ctx, ch, m); err == nil {
					t.Fatal("binding changed")
				}
			case "lost_completion":
				_, err := interaction.ReconcileSignal(ctx, source, f.r)
				must(t, err)
				_, err = f.r.db.Exec("UPDATE reconciliation_signal_sessions SET body=json_remove(body,'$.Summary')")
				must(t, err)
				if _, err = interaction.ReconcileSignal(ctx, source, f.r); err == nil {
					t.Fatal("lost completion restarted")
				}
			}
		})
	}
}
