package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func TestRejectedProviderInputIsTerminalAcrossConversionCrashAndPolicyChange(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	in.NativeID = "update-1"
	in.Text = "newworker status"
	must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: in.Account, Next: 2, Events: []provider.IntakeEvent{{ID: 1, Hash: protocol.Hash(in), Inbound: &in}}}))
	must(t, s.ConvertIntake(ctx))
	var replyBody string
	must(t, s.db.QueryRow("SELECT body FROM command_replies WHERE command_id=?", "Q-"+in.RequestKey()).Scan(&replyBody))
	if !strings.Contains(replyBody, "Request rejected:") {
		t.Fatal(replyBody)
	}
	// Recreate the crash window after the durable rejection but before conversion
	// marks the provider row. A newly available alias must not revive this request.
	_, err := s.db.Exec("UPDATE provider_inbox SET state='pending'")
	must(t, err)
	s.config.Interaction.Workers[0].Alias = "newworker"
	must(t, s.ConvertIntake(ctx))
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM commands").Scan(&count))
	if count != 0 {
		t.Fatal("rejected request executed")
	}
	must(t, s.db.QueryRow("SELECT count(*) FROM command_replies").Scan(&count))
	if count != 1 {
		t.Fatal("rejection duplicated")
	}
	var state string
	must(t, s.db.QueryRow("SELECT state FROM provider_inbox WHERE id=1").Scan(&state))
	if state != "rejected" {
		t.Fatal(state)
	}
	must(t, s.DispatchCommandReplies(ctx, replySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
		t.Fatal("policy-stale rejection disclosed")
		return provider.ReplyResult{}
	})))
}

func TestMalformedCommandRepliesAreBoundedAndNeverExecute(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	page := provider.IntakePage{Account: in.Account, Next: 9}
	for i := 1; i <= 8; i++ {
		v := in
		v.NativeID = fmt.Sprintf("update-%d", i)
		v.Text = "bad syntax with extra arguments"
		page.Events = append(page.Events, provider.IntakeEvent{ID: int64(i), Hash: protocol.Hash(v), Inbound: &v})
	}
	must(t, s.PersistTelegram(ctx, page))
	must(t, s.ConvertIntake(ctx))
	must(t, s.ConvertIntake(ctx))
	var replies, rejected, commands int
	must(t, s.db.QueryRow("SELECT count(*) FROM command_replies").Scan(&replies))
	must(t, s.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE state='rejected'").Scan(&rejected))
	must(t, s.db.QueryRow("SELECT count(*) FROM commands").Scan(&commands))
	if replies != 5 || rejected != 8 || commands != 0 {
		t.Fatalf("replies=%d rejected=%d commands=%d", replies, rejected, commands)
	}
	for i := 1; i <= 8; i++ {
		v := in
		v.NativeID = fmt.Sprintf("update-%d", i)
		v.Text = "bad syntax with extra arguments"
		if _, err := s.AdmitCommand(ctx, v); err == nil {
			t.Fatal("rejected input replayed")
		}
	}
}
