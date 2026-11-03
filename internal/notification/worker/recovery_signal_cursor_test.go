package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
)

func signalCursorFixture(t *testing.T) (signalRecoveryFixture, interaction.SignalRecoveryCursor) {
	t.Helper()
	f, _ := signalHandoffFixture(t)
	ctx := context.Background()
	p, err := interaction.PrepareSignalRecoveryHandoff(ctx, f.source, f.r)
	must(t, err)
	must(t, f.r.initializeSignalCursor(ctx))
	proof, err := f.source.StageSignalCursor(ctx, p)
	must(t, err)
	return f, proof
}
func TestSignalCursorInstallation(t *testing.T) {
	for _, snapshot := range []int{-1, 4, 6} {
		t.Run(fmt.Sprint(snapshot), func(t *testing.T) {
			ctx := context.Background()
			f := newSignalRecoveryFixtureWithRelaySnapshot(t, snapshot)
			var source interaction.SignalCursorSource = f.source
			if snapshot >= 0 {
				source, _ = prepareRestoredSignalSource(t, f)
			}
			_, err := interaction.ReconcileSignal(ctx, source, f.r)
			must(t, err)
			must(t, f.r.initializeRequestRecovery(ctx))
			seal, err := f.r.SealRecoveryRequests(ctx)
			must(t, err)
			must(t, f.r.initializeSignalHandoff(ctx))
			p, err := interaction.PrepareSignalRecoveryHandoff(ctx, source, f.r)
			must(t, err)
			must(t, f.r.initializeSignalCursor(ctx))
			proof, err := interaction.InstallSignalRecoveryCursor(ctx, source, f.r)
			must(t, err)
			if proof.IntakeEnabled || protocol.Hash(proof.Handoff) != protocol.Hash(p) {
				t.Fatal(proof)
			}
			var raw string
			var cursor interaction.SpoolCursor
			must(t, f.r.db.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", p.Target.Account).Scan(&raw))
			must(t, protocol.Decode([]byte(raw), &cursor, 4096))
			if cursor != p.Target {
				t.Fatal("cursor not installed", cursor)
			}
			after, err := f.r.SealRecoveryRequests(ctx)
			must(t, err)
			if after != seal {
				t.Fatal("cursor changed sealed request inventory")
			}
			var n int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE state!='recovery_quarantined'").Scan(&n))
			if n != 0 {
				t.Fatal("historical inbox reactivated")
			}
			must(t, source.Close())
			must(t, f.r.Close())
			if live, e := Open(f.path, f.r.config, false); e == nil {
				live.Close()
				t.Fatal("gateway activated")
			} else if !strings.Contains(e.Error(), "recovery is quarantined") {
				t.Fatal(e)
			}
			gateway, err := OpenSignalCursorSession(ctx, f.path, f.r.config)
			must(t, err)
			defer gateway.Close()
			if snapshot < 0 {
				source, err = relay.OpenRecoveryInbox(f.relayPath, p.Target.Account)
			} else {
				source, err = relay.OpenRestoredRecoveryInbox(ctx, f.relayPath, p.Target.Account)
			}
			must(t, err)
			defer source.Close()
			again, err := interaction.InstallSignalRecoveryCursor(ctx, source, gateway)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(proof) {
				t.Fatal("restart changed cursor proof")
			}
		})
	}
}
func TestSignalCursorRejectsCorruption(t *testing.T) {
	for _, kind := range []string{"intent", "missing_intent", "row", "cursor", "handoff", "handoff_file", "history", "seal", "schema", "table", "lock", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f, p := signalCursorFixture(t)
			ctx := context.Background()
			must(t, f.r.InstallSignalCursor(ctx, p))
			var err error
			switch kind {
			case "intent":
				bad := p
				bad.DatabaseHash = protocol.Hash("other")
				err = f.r.stage.WriteJSON(signalCursorFile(p.Handoff.Target.Account), bad, true)
			case "missing_intent":
				err = os.Remove(filepath.Join(f.path, "recovery", f.r.journal.Operation, signalCursorFile(p.Handoff.Target.Account)))
			case "row":
				_, err = f.r.db.Exec("UPDATE reconciliation_signal_cursors SET hash=?", protocol.Hash("other"))
			case "cursor":
				raw, _ := json.Marshal(p.Handoff.Before)
				_, err = f.r.db.Exec("UPDATE signal_imports SET cursor=?", string(raw))
			case "handoff":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_signal_handoffs")
			case "handoff_file":
				err = os.Remove(filepath.Join(f.path, "recovery", f.r.journal.Operation, signalHandoffFile(p.Handoff.Target.Account)))
			case "history":
				_, err = f.r.db.Exec("UPDATE reconciliation_signal_events SET hash=? WHERE position=6", protocol.Hash("other"))
			case "seal":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_request_fences")
			case "schema":
				_, err = f.r.db.Exec("UPDATE metadata SET value='2' WHERE key='signal_cursor_recovery_schema'")
			case "table":
				_, err = f.r.db.Exec("DROP TABLE reconciliation_signal_cursors")
			case "lock":
				must(t, os.Rename(filepath.Join(f.path, "worker.lock"), filepath.Join(f.path, "old.lock")))
				err = os.WriteFile(filepath.Join(f.path, "worker.lock"), nil, 0600)
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			must(t, err)
			if err = f.r.InstallSignalCursor(ctx, p); err == nil {
				t.Fatal("corrupted cursor accepted")
			}
			if _, err = f.r.SignalCursorHandoff(ctx, p.Handoff.Target.Account); err == nil {
				t.Fatal("corrupted handoff resumed")
			}
		})
	}
}
func TestSignalCursorLostResponses(t *testing.T) {
	for _, stage := range []string{"signal_cursor_intent", "signal_cursor_transaction", "signal_cursor_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, p := signalCursorFixture(t)
			ctx := context.Background()
			f.r.fault = func(at string) error {
				if at == stage {
					return fmt.Errorf("response lost")
				}
				return nil
			}
			if err := f.r.InstallSignalCursor(ctx, p); err == nil {
				t.Fatal("missing fault")
			}
			f.r.fault = nil
			got, err := interaction.InstallSignalRecoveryCursor(ctx, f.source, f.r)
			must(t, err)
			if protocol.Hash(got) != protocol.Hash(p) {
				t.Fatal("cursor changed after retry")
			}
		})
	}
}
func TestSignalCursorRequiresAgreement(t *testing.T) {
	f, p := signalCursorFixture(t)
	ctx := context.Background()
	_, err := f.r.db.Exec("DELETE FROM reconciliation_signal_handoffs")
	must(t, err)
	if err = f.r.InstallSignalCursor(ctx, p); err == nil {
		t.Fatal("missing agreement accepted")
	}
	var raw string
	var cursor interaction.SpoolCursor
	must(t, f.r.db.QueryRow("SELECT cursor FROM signal_imports").Scan(&raw))
	must(t, protocol.Decode([]byte(raw), &cursor, 4096))
	if cursor != p.Handoff.Before {
		t.Fatal("cursor changed without agreement")
	}
}
func TestSignalCursorRechecksIntent(t *testing.T) {
	f, p := signalCursorFixture(t)
	f.r.fault = func(at string) error {
		if at == "signal_cursor_transaction" {
			bad := p
			bad.DatabaseHash = protocol.Hash("other")
			return f.r.stage.WriteJSON(signalCursorFile(p.Handoff.Target.Account), bad, true)
		}
		return nil
	}
	if err := f.r.InstallSignalCursor(context.Background(), p); err == nil {
		t.Fatal("changed intent committed")
	}
	var raw string
	var cursor interaction.SpoolCursor
	must(t, f.r.db.QueryRow("SELECT cursor FROM signal_imports").Scan(&raw))
	must(t, protocol.Decode([]byte(raw), &cursor, 4096))
	if cursor != p.Handoff.Before {
		t.Fatal("partial cursor commit")
	}
}

type changedSignalCursorSource struct {
	interaction.SignalCursorSource
	calls, changeAt int
}

func (s *changedSignalCursorSource) StageSignalCursor(ctx context.Context, p interaction.SignalRecoveryHandoff) (interaction.SignalRecoveryCursor, error) {
	got, err := s.SignalCursorSource.StageSignalCursor(ctx, p)
	s.calls++
	if err == nil && s.calls == s.changeAt {
		if s.changeAt == 1 {
			got.Handoff.RequestsHash = protocol.Hash("other seal")
		} else {
			got.DatabaseHash = protocol.Hash("other database")
		}
	}
	return got, err
}
func TestSignalCursorChecksBothSourceResponses(t *testing.T) {
	for _, at := range []int{1, 2} {
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			f, p := signalCursorFixture(t)
			ctx := context.Background()
			changed := &changedSignalCursorSource{SignalCursorSource: f.source, changeAt: at}
			if _, err := interaction.InstallSignalRecoveryCursor(ctx, changed, f.r); err == nil {
				t.Fatal("source changed cursor proof")
			}
			var n int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_cursors").Scan(&n))
			if n != at-1 {
				t.Fatal("unexpected partial installation", n)
			}
			got, err := interaction.InstallSignalRecoveryCursor(ctx, f.source, f.r)
			must(t, err)
			if protocol.Hash(got) != protocol.Hash(p) {
				t.Fatal("resume changed proof")
			}
		})
	}
}
