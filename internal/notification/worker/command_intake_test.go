package worker

import (
	"context"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func TestIntakeCursorCommitsBeforeRestartAndResumesConversion(t *testing.T) {
	s, now, dir, in := commandFixture(t)
	ctx := context.Background()
	in.NativeID = "update-5"
	page := provider.IntakePage{Account: in.Account, Next: 6, Events: []provider.IntakeEvent{{ID: 5, Hash: protocol.Hash(in), Inbound: &in}}}
	must(t, s.PersistTelegram(ctx, page))
	config := s.config
	must(t, s.Close())
	r, err := Open(dir, config, false)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return *now }
	offset, err := r.TelegramOffset(ctx, in.Account)
	must(t, err)
	if offset != 6 {
		t.Fatal("cursor lost")
	}
	must(t, r.PersistTelegram(ctx, page))
	must(t, r.ConvertIntake(ctx))
	must(t, r.ConvertIntake(ctx))
	commands, err := r.Commands(ctx, producer)
	must(t, err)
	if len(commands) != 1 || commands[0].Command.ID != in.CommandID() {
		t.Fatal("request converted more than once", len(commands))
	}
	var converted int
	must(t, r.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE state='converted'").Scan(&converted))
	if converted != 1 {
		t.Fatal(converted)
	}
}

func TestIntakePageConflictRollsBackOffsetAndUnauthenticatedText(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	in.NativeID = "update-5"
	page := provider.IntakePage{Account: in.Account, Next: 6, Events: []provider.IntakeEvent{{ID: 5, Hash: protocol.Hash(in), Inbound: &in}}}
	must(t, s.PersistTelegram(ctx, page))
	page.Next = 7
	page.Events = append(page.Events, provider.IntakeEvent{ID: 6, Hash: protocol.Hash("ignored"), Rejection: "unsupported_event"})
	page.Events[0].Hash = protocol.Hash("conflict")
	if err := s.PersistTelegram(ctx, page); err == nil {
		t.Fatal("conflicting page accepted")
	}
	offset, err := s.TelegramOffset(ctx, in.Account)
	must(t, err)
	if offset != 6 {
		t.Fatal("offset acknowledged uncommitted page")
	}
	in.Actor = "stranger"
	in.NativeID = "update-6"
	in.Text = "private text must not be retained"
	page.Events = []provider.IntakeEvent{{ID: 6, Hash: protocol.Hash(in), Inbound: &in}}
	must(t, s.PersistTelegram(ctx, page))
	var body, state string
	must(t, s.db.QueryRow("SELECT body,state FROM provider_inbox WHERE id=6").Scan(&body, &state))
	if body != "" || state != "rejected" {
		t.Fatal("unauthenticated text retained")
	}
}
