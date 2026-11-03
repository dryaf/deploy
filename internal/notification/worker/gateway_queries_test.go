package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func queryReply(t *testing.T, s *Store, in interaction.Inbound) CommandReply {
	t.Helper()
	handled, err := s.AdmitGatewayQuery(context.Background(), in)
	must(t, err)
	if !handled {
		t.Fatal("query not handled")
	}
	var body string
	must(t, s.db.QueryRow("SELECT body FROM command_replies WHERE command_id=? AND kind='gateway'", "Q-"+in.RequestKey()).Scan(&body))
	var reply CommandReply
	must(t, protocol.Decode([]byte(body), &reply, protocol.MaxRequest))
	return reply
}

func TestGatewayQueriesFilterPermissionsAndPersistReply(t *testing.T) {
	s, now, path, in := commandFixture(t)
	w := s.config.Interaction.Workers[0]
	w.Alias = "secret/worker"
	w.Producer = protocol.Producer{ID: "secret", Epoch: "one"}
	s.config.Interaction.Workers = append(s.config.Interaction.Workers, w)
	in.Text = "workers"
	reply := queryReply(t, s, in)
	if strings.Contains(reply.Text, "secret") || !strings.Contains(reply.Text, "backup/storage") {
		t.Fatal("worker discovery disclosed unauthorized instance")
	}
	// Remove the synthetic non-enrolled worker before reopening this fixture.
	s.config.Interaction.Workers = s.config.Interaction.Workers[:1]
	in.NativeID = "help"
	in.Text = "backup/storage help"
	reply = queryReply(t, s, in)
	c := s.config
	must(t, s.Close())
	reopened, err := Open(path, c, false)
	must(t, err)
	defer reopened.Close()
	reopened.now = func() time.Time { return *now }
	if repeated := queryReply(t, reopened, in); protocol.Hash(reply) != protocol.Hash(repeated) {
		t.Fatal("query replay changed")
	}
	calls := 0
	must(t, reopened.DispatchCommandReplies(context.Background(), replySender(func(_ context.Context, d interaction.Destination, text string) provider.ReplyResult {
		calls++
		return provider.ReplyResult{Result: provider.Result{Accepted: true}, NativeID: "123"}
	})))
	if calls != 1 {
		t.Fatal("revoked query was sent or current query missing", calls)
	}
}

func TestHelpPaginationPinsActorConversationPolicyAndExpiry(t *testing.T) {
	s, now, _, in := commandFixture(t)
	w := &s.config.Interaction.Workers[0]
	w.Commands = map[string]interaction.Descriptor{}
	for i := 0; i < 10; i++ {
		w.Commands[fmt.Sprintf("read%d", i)] = interaction.Descriptor{ID: fmt.Sprintf("backup.read%d", i), Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: strings.Repeat("x", 1000)}
	}
	in.Text = "backup/storage help"
	first := queryReply(t, s, in)
	_, next, ok := strings.Cut(first.Text, "\nNext: ")
	if !ok {
		t.Fatal("large listing has no cursor")
	}
	in.NativeID = "next-page"
	in.Text = next
	second := queryReply(t, s, in)
	if len(second.Text) > 3000 || strings.Contains(second.Text, "read0:") {
		t.Fatal("unbounded or repeated page")
	}
	// A cursor does not confer permission to another actor, even with equal grants.
	p := s.config.Interaction.Permissions[0]
	p.Actor = "actor-two"
	s.config.Interaction.Permissions = append(s.config.Interaction.Permissions, p)
	in.NativeID = "different-actor"
	in.Actor = "actor-two"
	if _, err := s.AdmitGatewayQuery(context.Background(), in); err == nil {
		t.Fatal("cursor crossed actor/policy boundary")
	}
	s.config.Interaction.Permissions = s.config.Interaction.Permissions[:1]
	in.Actor = "actor-1"
	in.NativeID = "expired-page"
	*now = now.Add(11 * time.Minute)
	in.SentAt = *now
	if _, err := s.AdmitGatewayQuery(context.Background(), in); err == nil {
		t.Fatal("cursor extended listing lifetime")
	}
}

func TestCommandQueryRequiresOriginalAudienceAndCurrentScope(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	in.NativeID = "query"
	in.Text = "command " + v.Command.ID
	reply := queryReply(t, s, in)
	if !strings.Contains(reply.Text, "queued") {
		t.Fatal("missing authoritative state")
	}
	s.config.Interaction.Permissions[0].Scope = interaction.Scope{Subjects: []string{"b"}}
	in.NativeID = "narrowed-query"
	if _, err = s.AdmitGatewayQuery(ctx, in); err == nil {
		t.Fatal("query widened disclosure scope")
	}
	must(t, s.DispatchCommandReplies(ctx, replySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
		t.Fatal("queued reply disclosed revoked scope")
		return provider.ReplyResult{}
	})))
}

func TestQueryAndWorkerCommandsShareBurstBudget(t *testing.T) {
	s, _, _, in := commandFixture(t)
	for i := 0; i < 5; i++ {
		in.NativeID = fmt.Sprintf("help-%d", i)
		in.Text = "help"
		queryReply(t, s, in)
	}
	in.NativeID = "worker-request"
	in.Text = "backup/storage status"
	if _, err := s.AdmitCommand(context.Background(), in); err == nil {
		t.Fatal("gateway queries bypassed shared rate budget")
	}
}
