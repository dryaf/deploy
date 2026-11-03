package relay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func handoffRelayFixture(t *testing.T, restored bool) (interaction.SignalHandoffSource, string, interaction.SignalRecoveryHandoff) {
	t.Helper()
	ctx := context.Background()
	var source interaction.SignalHandoffSource
	var path string
	var ch interaction.SignalRecoveryChallenge
	if restored {
		source, path, ch = restoredExchangeFixture(t, 3)
	} else {
		source, path, ch = relayRecoveryFixture(t, 3)
	}
	m, err := source.Capture(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	summary := interaction.SignalRecoverySummary{Challenge: ch, Manifest: m, Unmapped: m.Highest}
	// The larger gateway watermark reserves positions missing from the snapshot.
	if restored {
		summary.GatewayHighest = 5
		summary.MissingRelayFrom = 4
		summary.MissingRelayThrough = 5
	}
	if err = source.RecordSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	p := interaction.SignalRecoveryHandoff{Version: 1, Summary: summary, RequestsHash: protocol.Hash("request seal"), Before: m.Relay, Target: m.Relay}
	p.Target.Position = max(m.Highest, summary.GatewayHighest)
	if restored {
		p.Target.Identity = m.Restore.After
		p.Before.Position = 5
	}
	if err = p.Validate(); err != nil {
		t.Fatal(err)
	}
	return source, path, p
}
func reopenHandoffRelay(ctx context.Context, path, account string, restored bool) (interaction.SignalHandoffSource, error) {
	if restored {
		return OpenRestoredRecoveryInbox(ctx, path, account)
	}
	return OpenRecoveryInbox(path, account)
}
func TestSignalRelayHandoffKeepsOriginalEvidence(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprint(restored), func(t *testing.T) {
			source, path, p := handoffRelayFixture(t, restored)
			ctx := context.Background()
			got, err := source.PrepareSignalHandoff(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if protocol.Hash(got) != protocol.Hash(p) {
				t.Fatal("changed proof")
			}
			if ordinary, ok := source.(*RecoveryInbox); ok {
				var acked, listed, seq uint64
				if err = ordinary.inbox.db.QueryRow("SELECT acked,listed,(SELECT seq FROM sqlite_sequence WHERE name='events') FROM identity").Scan(&acked, &listed, &seq); err != nil {
					t.Fatal(err)
				}
				if acked != p.Summary.Manifest.Acknowledged || listed != p.Summary.Manifest.Listed || seq != p.Summary.Manifest.Highest {
					t.Fatal("preparation changed watermarks")
				}
			}
			if err = source.Close(); err != nil {
				t.Fatal(err)
			}
			source, err = reopenHandoffRelay(ctx, path, p.Target.Account, restored)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			got, err = source.PrepareSignalHandoff(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if protocol.Hash(got) != protocol.Hash(p) {
				t.Fatal("changed replay")
			}
			if restored && got.Target.Position != 5 {
				t.Fatal("missing suffix positions reused")
			}
		})
	}
}
func TestSignalRelayHandoffLostResponses(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, stage := range []string{"signal_handoff_source_intent", "signal_handoff_source_transaction", "signal_handoff_source_commit"} {
			if restored && stage == "signal_handoff_source_transaction" {
				continue
			}
			t.Run(fmt.Sprintf("%t/%s", restored, stage), func(t *testing.T) {
				source, path, p := handoffRelayFixture(t, restored)
				ctx := context.Background()
				fault := func(at string) error {
					if at == stage {
						return fmt.Errorf("response lost")
					}
					return nil
				}
				switch s := source.(type) {
				case *RecoveryInbox:
					s.fault = fault
				case *RestoredRecoveryInbox:
					s.fault = fault
				}
				if _, err := source.PrepareSignalHandoff(ctx, p); err == nil {
					t.Fatal("missing fault")
				}
				if err := source.Close(); err != nil {
					t.Fatal(err)
				}
				again, err := reopenHandoffRelay(ctx, path, p.Target.Account, restored)
				if err != nil {
					t.Fatal(err)
				}
				defer again.Close()
				got, err := again.PrepareSignalHandoff(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				if protocol.Hash(got) != protocol.Hash(p) {
					t.Fatal("changed replay")
				}
			})
		}
	}
}
func TestSignalRelayHandoffRejectsDamage(t *testing.T) {
	for _, kind := range []string{"file", "missing_file", "hash", "event", "ack", "listed", "sequence", "extra", "missing_summary", "lock", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			source, path, p := handoffRelayFixture(t, false)
			r := source.(*RecoveryInbox)
			ctx := context.Background()
			_, err := r.PrepareSignalHandoff(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				bad := p
				bad.RequestsHash = protocol.Hash("other")
				err = r.inbox.dir.WriteJSON(signalHandoffFile, bad, true)
			case "missing_file":
				err = os.Remove(filepath.Join(path, signalHandoffFile))
			case "hash":
				_, err = r.inbox.db.Exec("UPDATE recovery_records SET hash=? WHERE position=1", protocol.Hash("other"))
			case "event":
				_, err = r.inbox.db.Exec("UPDATE events SET body='{}' WHERE position=1")
			case "ack":
				_, err = r.inbox.db.Exec("UPDATE identity SET acked=0")
			case "listed":
				_, err = r.inbox.db.Exec("UPDATE identity SET listed=0")
			case "sequence":
				_, err = r.inbox.db.Exec("UPDATE sqlite_sequence SET seq=4 WHERE name='events'")
			case "extra":
				_, err = r.inbox.db.Exec("INSERT INTO events(key,hash,body) VALUES('extra','hash','{}')")
			case "missing_summary":
				_, err = r.inbox.db.Exec("DELETE FROM recovery_exports")
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
			if _, err = r.PrepareSignalHandoff(ctx, p); err == nil {
				t.Fatal("damaged evidence accepted")
			}
		})
	}
}
func TestRestoredSignalHandoffRejectsMissingProof(t *testing.T) {
	source, path, p := handoffRelayFixture(t, true)
	r := source.(*RestoredRecoveryInbox)
	ctx := context.Background()
	if _, err := r.PrepareSignalHandoff(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "restore", r.journal.Operation, signalHandoffFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PrepareSignalHandoff(ctx, p); err == nil {
		t.Fatal("lost prepared proof recreated")
	}
}
