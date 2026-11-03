package worker

import (
	"context"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/provider"
)

type replySender func(context.Context, interaction.Destination, string) provider.ReplyResult

func (f replySender) SendReply(c context.Context, d interaction.Destination, s string) provider.ReplyResult {
	return f(c, d, s)
}

func TestCommandReplyAttemptPersistsAcceptanceAcrossRevocation(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	_, err := s.AdmitCommand(ctx, in)
	must(t, err)
	calls := 0
	must(t, s.DispatchCommandReplies(ctx, replySender(func(_ context.Context, d interaction.Destination, text string) provider.ReplyResult {
		calls++
		if d != s.config.Interaction.Destinations[0] {
			t.Fatal("wrong conversation")
		}
		s.config.Interaction.Permissions = nil
		return provider.ReplyResult{Result: provider.Result{Accepted: true}, NativeID: "123"}
	})))
	var state, native string
	must(t, s.db.QueryRow("SELECT state,native_id FROM command_reply_attempts").Scan(&state, &native))
	if state != "provider_accepted" || native != "123" {
		t.Fatal("acceptance evidence lost", state, native)
	}
	must(t, s.DispatchCommandReplies(ctx, replySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
		t.Fatal("accepted reply repeated")
		return provider.ReplyResult{}
	})))
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestCommandReplyRestartAndDestinationFencing(t *testing.T) {
	s, now, dir, in := commandFixture(t)
	ctx := context.Background()
	_, err := s.AdmitCommand(ctx, in)
	must(t, err)
	replies, err := s.PendingCommandReplies(ctx)
	must(t, err)
	_, ok, err := s.grantReply(ctx, replies[0])
	must(t, err)
	if !ok {
		t.Fatal("no attempt")
	}
	config := s.config
	must(t, s.Close())
	reopened, err := Open(dir, config, false)
	must(t, err)
	defer reopened.Close()
	reopened.now = func() time.Time { return *now }
	must(t, reopened.DispatchCommandReplies(ctx, replySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
		t.Fatal("ambiguous restart retried")
		return provider.ReplyResult{}
	})))
	in.NativeID = "second"
	_, err = reopened.AdmitCommand(ctx, in)
	must(t, err)
	reopened.config.Interaction.Destinations[0].NativeID = "other"
	must(t, reopened.DispatchCommandReplies(ctx, replySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
		t.Fatal("redirected conversation")
		return provider.ReplyResult{}
	})))
}
