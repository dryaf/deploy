package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/safefs"
)

// Test-only publication of a previously recovered relay. Production staging
// cannot perform this promotion or bypass the managed activation gate.
func priorBaselineRelay(t *testing.T, f signalRecoveryFixture) (string, interaction.SignalRecoveryCursor) {
	t.Helper()
	ctx := context.Background()
	source, j := prepareRestoredSignalSource(t, f)
	op, err := safefs.NewID("n")
	must(t, err)
	nonce, err := safefs.NewID("n")
	must(t, err)
	now := time.Now().UTC()
	ch := interaction.SignalRecoveryChallenge{Operation: op, Nonce: nonce, Gateway: protocol.Identity{Worker: "old-gateway", Recovery: "old-epoch"}, Relay: source.Cursor(), IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute), BootID: "old-boot", Uptime: 1}
	m, err := source.Capture(ctx, ch)
	must(t, err)
	summary := interaction.SignalRecoverySummary{Challenge: ch, Manifest: m, Unmapped: m.EventCount(), GatewayHighest: 4, MissingRelayFrom: 3, MissingRelayThrough: 4}
	must(t, source.RecordSummary(ctx, summary))
	p := interaction.SignalRecoveryHandoff{Version: 1, Summary: summary, RequestsHash: protocol.Hash("previous seal"), Before: source.Cursor(), Target: source.Cursor()}
	p.Target.Identity = j.After
	p.Target.Position = 4
	proof, err := source.StageSignalCursor(ctx, p)
	must(t, err)
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	raw, err := os.ReadFile(filepath.Join(f.relayPath, "restore", j.Operation, "signal-cursor.db"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(path, "inbox.db"), raw, 0600))
	pin, _ := json.Marshal(map[string]any{"identity": p.Target.Identity, "account": p.Target.Account, "baseline": protocol.Hash(proof)})
	raw, _ = json.Marshal(proof)
	db, err := sql.Open("sqlite", filepath.Join(path, "inbox.db"))
	must(t, err)
	_, err = db.Exec("CREATE TABLE signal_runtime(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL); PRAGMA user_version=5")
	must(t, err)
	_, err = db.Exec("INSERT INTO signal_runtime VALUES(1,?)", string(raw))
	must(t, err)
	_, err = db.Exec("UPDATE identity SET body=?", string(pin))
	must(t, err)
	must(t, db.Close())
	must(t, os.WriteFile(filepath.Join(path, "identity.json"), pin, 0600))
	must(t, os.WriteFile(filepath.Join(path, "signal-runtime.json"), raw, 0600))
	must(t, os.WriteFile(filepath.Join(path, "inbox.lock"), nil, 0600))
	must(t, source.Close())
	// Simulate the matching identity in a gateway snapshot taken after the prior
	// activation; its retained original request mappings are left untouched.
	cursor, _ := json.Marshal(p.Target)
	_, err = f.r.db.Exec("UPDATE signal_imports SET cursor=? WHERE account=?", string(cursor), p.Target.Account)
	must(t, err)
	return path, proof
}

func TestSignalBaselineGatewayExchangeAndCursor(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, lost := range []string{"none", "import", "finish"} {
			t.Run(fmt.Sprintf("%t/%s", restored, lost), func(t *testing.T) {
				ctx := context.Background()
				f := newSignalRecoveryFixtureWithRelaySnapshot(t, 2)
				path, prior := priorBaselineRelay(t, f)
				live, err := relay.OpenInbox(path, prior.Handoff.Target.Account, false)
				must(t, err)
				for _, key := range []string{"new-five", "new-six"} {
					must(t, live.Enqueue(ctx, interaction.SpoolEvent{Key: key, Hash: protocol.Hash(key)}))
				}
				must(t, live.Close())
				var source interaction.SignalCursorSource
				if restored {
					artifact := filepath.Join(t.TempDir(), "snapshot")
					_, err = relay.SnapshotInbox(ctx, path, prior.Handoff.Target.Account, artifact)
					must(t, err)
					_, err = relay.PrepareInboxRestore(ctx, path, prior.Handoff.Target.Account, artifact)
					must(t, err)
					source, err = relay.OpenRestoredRecoveryInbox(ctx, path, prior.Handoff.Target.Account)
				} else {
					source, err = relay.OpenRecoveryInbox(path, prior.Handoff.Target.Account)
				}
				must(t, err)
				defer source.Close()
				if lost != "none" {
					if _, err = interaction.ReconcileSignal(ctx, source, &lostSignalRecovery{SignalRecoverySession: f.r, stage: lost}); err == nil {
						t.Fatal("lost response missing")
					}
				}
				summary, err := interaction.ReconcileSignal(ctx, source, f.r)
				must(t, err)
				if summary.Manifest.EventCount() != 4 || summary.Manifest.Highest != 6 || summary.Manifest.BaselineHash != protocol.Hash(prior) || len(summary.Manifest.History) != 2 || summary.IntakeEnabled || summary.HistoryComplete {
					t.Fatal(summary)
				}
				must(t, f.r.initializeRequestRecovery(ctx))
				seal, err := f.r.SealRecoveryRequests(ctx)
				must(t, err)
				if seal.MissingSignalRows != 2 {
					t.Fatal("missing gateway evidence concealed", seal)
				}
				must(t, f.r.initializeSignalHandoff(ctx))
				must(t, f.r.initializeSignalCursor(ctx))
				_, err = interaction.PrepareSignalRecoveryHandoff(ctx, source, f.r)
				must(t, err)
				proof, err := interaction.InstallSignalRecoveryCursor(ctx, source, f.r)
				must(t, err)
				if proof.Handoff.Target.Position != 6 || proof.IntakeEnabled || !proof.Handoff.Summary.Manifest.Equal(summary.Manifest) {
					t.Fatal(proof)
				}
				again, err := interaction.InstallSignalRecoveryCursor(ctx, source, f.r)
				must(t, err)
				if protocol.Hash(again) != protocol.Hash(proof) {
					t.Fatal("cursor changed during replay")
				}
				var commands, fresh int
				must(t, f.r.db.QueryRow("SELECT count(*) FROM commands").Scan(&commands))
				must(t, f.r.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE state!='recovery_quarantined'").Scan(&fresh))
				if commands != 1 || fresh != 0 {
					t.Fatal("recovery created execution authority", commands, fresh)
				}
				_, err = f.r.db.Exec("DELETE FROM reconciliation_signal_events WHERE position=1")
				must(t, err)
				if _, err = f.r.SealRecoveryRequests(ctx); err == nil {
					t.Fatal("undeclared lost event was excused as a snapshot gap")
				}
			})
		}
	}
}

func TestSignalMissingSnapshotEvidence(t *testing.T) {
	for _, kind := range []string{"intact", "hash", "body", "outside_gap", "injected", "candidate", "sidecar"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newSignalRecoveryFixtureWithRelaySnapshot(t, 0)
			source, _ := prepareRestoredSignalSource(t, f)
			_, err := interaction.ReconcileSignal(ctx, source, f.r)
			must(t, err)
			must(t, f.r.initializeRequestRecovery(ctx))
			switch kind {
			case "hash":
				_, err = f.r.db.Exec("UPDATE provider_inbox SET hash=? WHERE id=1", protocol.Hash("changed"))
			case "body":
				_, err = f.r.db.Exec("UPDATE provider_inbox SET body='' WHERE id=1")
			case "outside_gap":
				// A missing row outside the declared suffix cannot be excused by
				// snapshot evidence, even when it has a valid stored identity.
				_, err = f.r.db.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,5,?,'','recovery_quarantined')", source.Cursor().Account, protocol.Hash("unexpected"))
			case "injected":
				_, err = f.r.db.Exec("UPDATE provider_inbox SET id=0 WHERE id=4")
			case "candidate":
				db, e := snapshotDB(f.r.stage, "candidate.db", false)
				must(t, e)
				_, err = db.Exec("UPDATE provider_inbox SET hash=? WHERE id=1", protocol.Hash("changed"))
				must(t, db.Close())
			case "sidecar":
				err = os.WriteFile(filepath.Join(f.path, "recovery", f.r.journal.Operation, "candidate.db-wal"), nil, 0600)
			}
			must(t, err)
			seal, err := f.r.SealRecoveryRequests(ctx)
			if kind != "intact" {
				if err == nil {
					t.Fatal("damaged missing-history evidence sealed")
				}
				return
			}
			must(t, err)
			if seal.MissingSignalRows != 4 || seal.InboxRows != 4 || seal.IntakeEnabled {
				t.Fatal(seal)
			}
			var id string
			must(t, f.r.db.QueryRow("SELECT command_id FROM command_inbox WHERE request_key=?", f.first.RequestKey()).Scan(&id))
			if id != f.first.CommandID() {
				t.Fatal("existing mapping changed")
			}
			again, err := f.r.SealRecoveryRequests(ctx)
			must(t, err)
			if again != seal {
				t.Fatal("seal changed on replay")
			}
		})
	}
}
