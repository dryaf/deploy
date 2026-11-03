package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func signalRuntimeFixture(t *testing.T) (signalRecoveryFixture, interaction.SignalRecoveryCursor, *Store, provider.SignalAccount) {
	t.Helper()
	f, p := signalCursorFixture(t)
	runtime := &Store{db: f.r.db, dir: f.r.dir, identity: f.r.journal.After, config: f.r.config, now: time.Now}
	profile := provider.SignalAccount{Account: p.Handoff.Target.Account, Socket: "/private/relay.sock", InboxIdentity: p.Handoff.Target.Identity}
	return f, p, runtime, profile
}
func TestSignalRuntimeRecoveryAdmission(t *testing.T) {
	f, p, s, profile := signalRuntimeFixture(t)
	ctx := context.Background()
	if _, err := s.signalCursor(ctx, profile); err == nil {
		t.Fatal("Signal intake before cursor commit")
	}
	must(t, f.r.InstallSignalCursor(ctx, p))
	cursor, err := s.signalCursor(ctx, profile)
	must(t, err)
	if cursor != p.Handoff.Target {
		t.Fatal("wrong starting cursor")
	}
	fresh := f.first
	fresh.NativeID = "fresh-after-recovery"
	fresh.Text = "help"
	fresh.SentAt = time.Now().UTC()
	if _, err = s.AdmitGatewayQuery(ctx, fresh); err == nil {
		t.Fatal("fresh input without post-baseline persistence accepted")
	}
	must(t, s.recordCommandRejection(ctx, fresh, &interaction.Rejection{Message: "test", Silent: true}))
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM command_inbox WHERE request_key=?", fresh.RequestKey()).Scan(&count))
	if count != 0 {
		t.Fatal("missing intake evidence created rejection reply")
	}
	cursor.Position++
	page := interaction.SpoolPage{Cursor: cursor, Events: []interaction.SpoolEvent{{Position: cursor.Position, Key: fresh.NativeID, Hash: protocol.Hash(fresh), Inbound: &fresh}}}
	must(t, s.PersistSignal(ctx, profile, page))
	must(t, s.ConvertIntake(ctx))
	if handled, err := s.AdmitGatewayQuery(ctx, fresh); err != nil || !handled {
		t.Fatal("persisted fresh query rejected", err)
	}
	must(t, s.db.QueryRow("SELECT count(*) FROM command_inbox WHERE request_key=?", fresh.RequestKey()).Scan(&count))
	if count != 1 {
		t.Fatal("fresh query identity changed", count)
	}
	var rejection *interaction.Rejection
	_, err = s.AdmitCommand(ctx, f.unknown)
	if !errors.As(err, &rejection) || !rejection.Silent {
		t.Fatal("historical request revived", err)
	}
}
func TestSignalRuntimeRecoveryRefusesOldPositions(t *testing.T) {
	f, p, s, profile := signalRuntimeFixture(t)
	ctx := context.Background()
	must(t, f.r.InstallSignalCursor(ctx, p))
	page := interaction.SpoolPage{Cursor: p.Handoff.Target, Events: []interaction.SpoolEvent{{Position: 1, Key: f.first.NativeID, Hash: protocol.Hash(f.first), Inbound: &f.first}}}
	if err := s.PersistSignal(ctx, profile, page); err == nil {
		t.Fatal("pre-baseline event accepted")
	}
	cursor, err := s.signalCursor(ctx, profile)
	must(t, err)
	if cursor != p.Handoff.Target {
		t.Fatal("baseline changed")
	}
}
func TestSignalRuntimeRecoveryProofRequired(t *testing.T) {
	for _, kind := range []string{"row", "schema", "table", "handoff", "handoff_schema", "seal", "cursor_missing", "cursor_before", "cursor_pin"} {
		t.Run(kind, func(t *testing.T) {
			f, p, s, profile := signalRuntimeFixture(t)
			ctx := context.Background()
			must(t, f.r.InstallSignalCursor(ctx, p))
			var err error
			switch kind {
			case "row":
				_, err = s.db.Exec("UPDATE reconciliation_signal_cursors SET hash=?", protocol.Hash("other"))
			case "schema":
				_, err = s.db.Exec("UPDATE metadata SET value='2' WHERE key='signal_cursor_recovery_schema'")
			case "table":
				_, err = s.db.Exec("DROP TABLE reconciliation_signal_cursors")
			case "handoff_schema":
				_, err = s.db.Exec("UPDATE metadata SET value='2' WHERE key='signal_handoff_schema'")
			case "handoff":
				_, err = s.db.Exec("DELETE FROM reconciliation_signal_handoffs")
			case "seal":
				_, err = s.db.Exec("DELETE FROM reconciliation_request_seal")
			case "cursor_missing":
				_, err = s.db.Exec("DELETE FROM signal_imports")
			case "cursor_before":
				raw, _ := json.Marshal(p.Handoff.Before)
				_, err = s.db.Exec("UPDATE signal_imports SET cursor=?", string(raw))
			case "cursor_pin":
				cursor := p.Handoff.Target
				cursor.Identity.Recovery = "wrong"
				raw, _ := json.Marshal(cursor)
				_, err = s.db.Exec("UPDATE signal_imports SET cursor=?", string(raw))
			}
			must(t, err)
			if _, err = s.signalCursor(ctx, profile); err == nil {
				t.Fatal("damaged proof allowed polling")
			}
			if err = s.PersistSignal(ctx, profile, interaction.SpoolPage{Cursor: p.Handoff.Target}); err == nil {
				t.Fatal("damaged proof allowed persistence")
			}
			fresh := f.first
			fresh.NativeID = "new"
			fresh.Text = "help"
			if _, err = s.AdmitGatewayQuery(ctx, fresh); err == nil {
				t.Fatal("damaged proof admitted input")
			}
		})
	}
}
func TestSignalRuntimeKeepsMappedHistoryReadable(t *testing.T) {
	f, _, s, _ := signalRuntimeFixture(t)
	must(t, s.transaction(context.Background(), func(tx *sql.Tx) error { return guardRequestAdmission(tx, f.first) }))
}
