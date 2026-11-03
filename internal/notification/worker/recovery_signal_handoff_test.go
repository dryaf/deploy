package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
)

func signalHandoffFixture(t *testing.T) (signalRecoveryFixture, interaction.SignalRecoveryHandoff) {
	t.Helper()
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	_, err := interaction.ReconcileSignal(ctx, f.source, f.r)
	must(t, err)
	must(t, f.r.initializeRequestRecovery(ctx))
	_, err = f.r.SealRecoveryRequests(ctx)
	must(t, err)
	must(t, f.r.initializeSignalHandoff(ctx))
	p, err := f.r.PrepareSignalHandoff(ctx, f.source.Cursor().Account)
	must(t, err)
	return f, p
}
func assertSignalHandoffQuarantined(t *testing.T, r *recoveryReconciler, path string, p interaction.SignalRecoveryHandoff) {
	t.Helper()
	var raw string
	must(t, r.db.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", p.Target.Account).Scan(&raw))
	var cursor interaction.SpoolCursor
	must(t, protocol.Decode([]byte(raw), &cursor, 4096))
	if cursor != p.Before {
		t.Fatal("preparation installed operational cursor", cursor)
	}
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE state!='recovery_quarantined'").Scan(&count))
	if count != 0 {
		t.Fatal("historical inbox reactivated")
	}
	if s, err := Open(path, r.config, false); err == nil {
		s.Close()
		t.Fatal("live gateway opened during recovery")
	}
}
func TestSignalHandoffAgreement(t *testing.T) {
	for _, snapshot := range []int{-1, 4, 6} {
		t.Run(fmt.Sprint(snapshot), func(t *testing.T) {
			ctx := context.Background()
			f := newSignalRecoveryFixtureWithRelaySnapshot(t, snapshot)
			var source interaction.SignalHandoffSource = f.source
			if snapshot >= 0 {
				source, _ = prepareRestoredSignalSource(t, f)
			}
			summary, err := interaction.ReconcileSignal(ctx, source, f.r)
			must(t, err)
			must(t, f.r.initializeRequestRecovery(ctx))
			seal, err := f.r.SealRecoveryRequests(ctx)
			must(t, err)
			must(t, f.r.initializeSignalHandoff(ctx))
			p, err := interaction.PrepareSignalRecoveryHandoff(ctx, source, f.r)
			must(t, err)
			must(t, p.Validate())
			if p.RequestsHash != protocol.Hash(seal) || p.Target.Position != summary.Manifest.Highest || p.Before.Position != 4 || p.IntakeEnabled {
				t.Fatal(p)
			}
			if snapshot < 0 && p.Target.Identity != source.Cursor().Identity {
				t.Fatal("ordinary relay identity changed")
			}
			if snapshot >= 0 && p.Target.Identity != summary.Manifest.Restore.After {
				t.Fatal("restored pin not reserved")
			}
			assertSignalHandoffQuarantined(t, f.r, f.path, p)
			must(t, source.Close())
			must(t, f.r.Close())
			if live, e := Open(f.path, f.r.config, false); e == nil {
				live.Close()
				t.Fatal("prepared handoff lifted quarantine")
			} else if !strings.Contains(e.Error(), "recovery is quarantined") {
				t.Fatal("recovery fence was not checked", e)
			}
			gateway, err := OpenSignalHandoffSession(ctx, f.path, f.r.config)
			must(t, err)
			defer gateway.Close()
			if snapshot < 0 {
				source, err = relay.OpenRecoveryInbox(f.relayPath, p.Target.Account)
			} else {
				source, err = relay.OpenRestoredRecoveryInbox(ctx, f.relayPath, p.Target.Account)
			}
			must(t, err)
			defer source.Close()
			again, err := interaction.PrepareSignalRecoveryHandoff(ctx, source, gateway)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(p) {
				t.Fatal("restart changed agreement")
			}
			assertSignalHandoffQuarantined(t, gateway.(*recoveryReconciler), f.path, p)
		})
	}
}
func TestSignalHandoffRequiresCompletedEvidence(t *testing.T) {
	f := newSignalRecoveryFixture(t)
	ctx := context.Background()
	must(t, f.r.initializeSignalHandoff(ctx))
	if _, err := f.r.PrepareSignalHandoff(ctx, f.source.Cursor().Account); err == nil {
		t.Fatal("missing history accepted")
	}
	_, err := interaction.ReconcileSignal(ctx, f.source, f.r)
	must(t, err)
	if _, err = f.r.PrepareSignalHandoff(ctx, f.source.Cursor().Account); err == nil {
		t.Fatal("missing request seal accepted")
	}
}
func TestSignalHandoffRejectsCorruption(t *testing.T) {
	for _, kind := range []string{"intent", "missing_intent", "row", "schema", "table", "event", "mapping", "inbox", "cursor", "seal", "lock", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f, p := signalHandoffFixture(t)
			ctx := context.Background()
			_, err := interaction.PrepareSignalRecoveryHandoff(ctx, f.source, f.r)
			must(t, err)
			switch kind {
			case "intent":
				bad := p
				bad.RequestsHash = protocol.Hash("another seal")
				must(t, f.r.stage.WriteJSON(signalHandoffFile(p.Target.Account), bad, true))
			case "missing_intent":
				must(t, os.Remove(filepath.Join(f.path, "recovery", f.r.journal.Operation, signalHandoffFile(p.Target.Account))))
			case "row":
				_, err = f.r.db.Exec("UPDATE reconciliation_signal_handoffs SET hash=?", protocol.Hash("other"))
			case "schema":
				_, err = f.r.db.Exec("UPDATE metadata SET value='2' WHERE key='signal_handoff_schema'")
			case "table":
				_, err = f.r.db.Exec("DROP TABLE reconciliation_signal_handoffs")
			case "event":
				_, err = f.r.db.Exec("UPDATE reconciliation_signal_events SET hash=? WHERE position=6", protocol.Hash("other"))
			case "mapping":
				_, err = f.r.db.Exec("UPDATE reconciliation_signal_events SET command_id='C-wrong' WHERE position=1")
			case "inbox":
				_, err = f.r.db.Exec("UPDATE provider_inbox SET state='pending' WHERE id=6")
			case "cursor":
				_, err = f.r.db.Exec("DELETE FROM signal_imports")
			case "seal":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_request_fences WHERE key=?", f.unknown.RequestKey())
			case "lock":
				must(t, os.Rename(filepath.Join(f.path, "worker.lock"), filepath.Join(f.path, "old.lock")))
				must(t, os.WriteFile(filepath.Join(f.path, "worker.lock"), nil, 0600))
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			must(t, err)
			if err = f.r.RecordSignalHandoff(ctx, p); err == nil {
				t.Fatal("damaged handoff accepted")
			}
		})
	}
}
func TestSignalHandoffLostGatewayResponses(t *testing.T) {
	for _, stage := range []string{"signal_handoff_intent", "signal_handoff_transaction", "signal_handoff_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, p := signalHandoffFixture(t)
			ctx := context.Background()
			f.r.fault = func(at string) error {
				if at == stage {
					return fmt.Errorf("response lost")
				}
				return nil
			}
			if _, err := interaction.PrepareSignalRecoveryHandoff(ctx, f.source, f.r); err == nil {
				t.Fatal("missing fault")
			}
			f.r.fault = nil
			again, err := interaction.PrepareSignalRecoveryHandoff(ctx, f.source, f.r)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(p) {
				t.Fatal("lost response changed agreement")
			}
			assertSignalHandoffQuarantined(t, f.r, f.path, p)
		})
	}
}
func TestSignalHandoffRejectsRequestInventoryMutationAtCommit(t *testing.T) {
	f, p := signalHandoffFixture(t)
	f.r.fault = func(at string) error {
		if at == "signal_handoff_intent" {
			return os.Remove(filepath.Join(f.path, "recovery", f.r.journal.Operation, "request-seal.json"))
		}
		return nil
	}
	if err := f.r.RecordSignalHandoff(context.Background(), p); err == nil {
		t.Fatal("request seal lost during transaction")
	}
	var n int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_handoffs").Scan(&n))
	if n != 0 {
		t.Fatal("unsealed handoff committed")
	}
}
func TestSignalHandoffBoundsAndMissingSourceHistory(t *testing.T) {
	f, p := signalHandoffFixture(t)
	for _, mutate := range []func(*interaction.SignalRecoveryHandoff){
		func(p *interaction.SignalRecoveryHandoff) { p.Target.Position = 0 },
		func(p *interaction.SignalRecoveryHandoff) { p.Target.Position++ },
		func(p *interaction.SignalRecoveryHandoff) { p.Target.Identity = p.Summary.Challenge.Gateway },
		func(p *interaction.SignalRecoveryHandoff) { p.Before.Position = p.Target.Position + 1 },
		func(p *interaction.SignalRecoveryHandoff) { p.IntakeEnabled = true },
		func(p *interaction.SignalRecoveryHandoff) { p.RequestsHash = "" },
	} {
		bad := p
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid handoff", bad)
		}
	}
	// A source completion is required independently of the gateway's completed import.
	must(t, f.source.Close())
	source, err := relay.OpenRecoveryInbox(f.relayPath, p.Target.Account)
	must(t, err)
	defer source.Close()
	bad := p
	bad.Summary.Challenge.Gateway.Recovery = "another"
	if _, err = source.PrepareSignalHandoff(context.Background(), bad); err == nil {
		t.Fatal("different gateway accepted")
	}
	// An ordinary transaction still rechecks the sealed inventory.
	must(t, f.r.transaction(context.Background(), func(tx *sql.Tx) error { return nil }))
}

type changedSignalHandoffSource struct {
	interaction.SignalHandoffSource
	calls, changeAt int
}

func (s *changedSignalHandoffSource) PrepareSignalHandoff(ctx context.Context, p interaction.SignalRecoveryHandoff) (interaction.SignalRecoveryHandoff, error) {
	got, err := s.SignalHandoffSource.PrepareSignalHandoff(ctx, p)
	s.calls++
	if err == nil && s.calls == s.changeAt {
		got.RequestsHash = protocol.Hash("different seal")
	}
	return got, err
}
func TestSignalHandoffChecksBothSourceResponses(t *testing.T) {
	for _, at := range []int{1, 2} {
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			f, p := signalHandoffFixture(t)
			ctx := context.Background()
			changed := &changedSignalHandoffSource{SignalHandoffSource: f.source, changeAt: at}
			if _, err := interaction.PrepareSignalRecoveryHandoff(ctx, changed, f.r); err == nil {
				t.Fatal("source changed proof without detection")
			}
			var n int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_handoffs").Scan(&n))
			if n != at-1 {
				t.Fatal("unexpected agreement state", n)
			}
			assertSignalHandoffQuarantined(t, f.r, f.path, p)
			got, err := interaction.PrepareSignalRecoveryHandoff(ctx, f.source, f.r)
			must(t, err)
			if protocol.Hash(got) != protocol.Hash(p) {
				t.Fatal("retry changed proof")
			}
		})
	}
}
func TestSignalHandoffRechecksIntentBeforeCommit(t *testing.T) {
	f, p := signalHandoffFixture(t)
	f.r.fault = func(at string) error {
		if at == "signal_handoff_transaction" {
			bad := p
			bad.RequestsHash = protocol.Hash("other")
			return f.r.stage.WriteJSON(signalHandoffFile(p.Target.Account), bad, true)
		}
		return nil
	}
	if err := f.r.RecordSignalHandoff(context.Background(), p); err == nil {
		t.Fatal("changed intent committed")
	}
	var n int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_handoffs").Scan(&n))
	if n != 0 {
		t.Fatal("partial agreement committed")
	}
}
