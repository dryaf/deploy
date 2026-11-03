package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func commandFixture(t *testing.T) (*Store, *time.Time, string, interaction.Inbound) {
	s, now, path := fixture(t)
	dest := interaction.Destination{ID: "private", Generation: "one", Provider: "telegram", Account: "bot-1", NativeID: "-100"}
	args := map[string]interaction.Argument{"subject": {Kind: "token", Required: true, Max: 256}, "target": {Kind: "token", Required: true, Max: 256}}
	c := interaction.Config{Revision: "policy-one", Workers: []interaction.Worker{{Alias: "backup/storage", Producer: producer, Generation: "commands-one", Commands: map[string]interaction.Descriptor{
		"status":      {ID: "backup.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "Show authorized subjects"},
		"retry":       {ID: "backup.retry", Version: 1, Permission: "job.request", Effect: "mutation", Scoped: true, TargetRequired: true, Arguments: args, Help: "Retry selected subject"},
		"accept-risk": {ID: "vuln.accept", Version: 1, Permission: "risk.accept", Effect: "mutation", Confirm: true, Scoped: true, TargetRequired: true, Arguments: args, Help: "Prepare acceptance"},
	}}}, Destinations: []interaction.Destination{dest}, Permissions: []interaction.Permission{{Actor: "actor-1", Conversation: dest.ID, Worker: producer.ID, Permissions: []string{"read", "job.request", "risk.accept"}, Scope: interaction.Scope{Subjects: []string{"a"}}}}}
	must(t, c.Validate())
	s.config.Interaction = &c
	in := interaction.Inbound{Provider: dest.Provider, Account: dest.Account, NativeID: "update-1", Actor: "actor-1", Conversation: dest.ID, DestinationFingerprint: dest.Fingerprint(), SentAt: *now, Text: "backup/storage status"}
	return s, now, path, in
}
func getCommand(t *testing.T, s *Store, id string) interaction.View {
	t.Helper()
	var v interaction.View
	must(t, s.transaction(context.Background(), func(tx *sql.Tx) error { var err error; v, err = readCommand(tx, id); return err }))
	return v
}

func TestSameWorkerTypeInstancesRemainIsolated(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	second := s.config.Interaction.Workers[0]
	second.Alias = "backup/second"
	second.Producer = protocol.Producer{ID: "second-backup", Epoch: "second-install"}
	s.config.Interaction.Workers = append(s.config.Interaction.Workers, second)
	enrollment := s.config.Producers[0]
	enrollment.Producer = second.Producer
	s.config.Producers = append(s.config.Producers, enrollment)
	// A configured second worker also needs its independent durable enrollment.
	_, err := s.db.Exec("INSERT INTO producers(id,epoch) VALUES(?,?)", second.Producer.ID, second.Producer.Epoch)
	must(t, err)
	lease, err := s.dir.Lock(ownershipName(second.Producer)+".lock", true, true)
	must(t, err)
	must(t, s.prepareProducerOwnership(second.Producer, true, false))
	lease.Close()
	in.Text = "backup/second status"
	if _, err := s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("first worker permission authorized second instance")
	}
	p := s.config.Interaction.Permissions[0]
	p.Worker = second.Producer.ID
	p.Scope = interaction.Scope{Subjects: []string{"b"}}
	s.config.Interaction.Permissions = append(s.config.Interaction.Permissions, p)
	v2, err := s.AdmitCommand(ctx, in)
	must(t, err)
	in.NativeID = "update-2"
	in.Text = "backup/storage status"
	v1, err := s.AdmitCommand(ctx, in)
	must(t, err)
	if v1.Command.Producer != producer || v2.Command.Producer != second.Producer || !v1.Command.Scope.Contains("a") || v1.Command.Scope.Contains("b") || !v2.Command.Scope.Contains("b") || v2.Command.Scope.Contains("a") {
		t.Fatal("instance or scope mixed")
	}
	if s.ImportCommand(ctx, producer, v2.Command.ID, protocol.Hash(v2.Command)) == nil {
		t.Fatal("different instance imported command")
	}
	in.Text = "backup/second status"
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("replay redirected first command to second instance")
	}
}
func TestCommandIdentityRestartAndConflict(t *testing.T) {
	s, now, path, in := commandFixture(t)
	ctx := context.Background()
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	copy, err := s.AdmitCommand(ctx, in)
	must(t, err)
	if protocol.Hash(v) != protocol.Hash(copy) {
		t.Fatal("duplicate command changed")
	}
	changed := in
	changed.Text = "backup/storage retry subject=a target=incident-one"
	if _, err = s.AdmitCommand(ctx, changed); err == nil {
		t.Fatal("conflicting inbound reused")
	}
	config := s.config
	must(t, s.Close())
	s2, err := Open(path, config, false)
	must(t, err)
	defer s2.Close()
	s2.now = func() time.Time { return *now }
	copy, err = s2.AdmitCommand(ctx, in)
	must(t, err)
	if copy.Command.ID != v.Command.ID {
		t.Fatal("restart minted new command")
	}
	var count int
	must(t, s2.db.QueryRow("SELECT count(*) FROM commands").Scan(&count))
	if count != 1 {
		t.Fatal(count)
	}
}
func TestCommandGrantsAndOrderedResults(t *testing.T) {
	s, now, _, in := commandFixture(t)
	ctx := context.Background()
	in.Text = "backup/storage retry subject=a target=incident-one"
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	g, err := s.GrantCommand(ctx, producer, v.Command.ID)
	must(t, err)
	if g.ID != "" {
		t.Fatal("grant before durable import")
	}
	must(t, s.ImportCommand(ctx, producer, v.Command.ID, protocol.Hash(v.Command)))
	g, err = s.GrantCommand(ctx, producer, v.Command.ID)
	must(t, err)
	if g.ID == "" {
		t.Fatal("missing grant")
	}
	*now = now.Add(time.Second)
	g2, err := s.GrantCommand(ctx, producer, v.Command.ID)
	must(t, err)
	if g != g2 {
		t.Fatal("grant identity/deadline renewed")
	}
	r := interaction.Result{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), Revision: 2, Phase: "execute", State: "succeeded", Scope: v.Command.Scope, Text: "done"}
	if _, err = s.SubmitCommandResult(ctx, producer, r); err == nil {
		t.Fatal("revision gap accepted")
	}
	r.Revision = 1
	r.State = "running"
	r.JobID = "job-one"
	a, err := s.SubmitCommandResult(ctx, producer, r)
	must(t, err)
	must(t, s.AckCommandResult(ctx, producer, a))
	r.Revision = 2
	r.State = "succeeded"
	a, err = s.SubmitCommandResult(ctx, producer, r)
	must(t, err)
	again, err := s.SubmitCommandResult(ctx, producer, r)
	must(t, err)
	if a != again {
		t.Fatal("result receipt replay changed")
	}
	replies, err := s.PendingCommandReplies(ctx)
	must(t, err)
	if len(replies) != 1 || replies[0].Revision != 2 {
		t.Fatalf("obsolete progress: %+v", replies)
	}
	a.Identity.Recovery = "older"
	if s.AckCommandResult(ctx, producer, a) == nil {
		t.Fatal("old recovery ack accepted")
	}
}
func TestLatePreparationIsAcknowledgedWithoutReopening(t *testing.T) {
	s, now, _, in := commandFixture(t)
	ctx := context.Background()
	in.Text = "backup/storage accept-risk subject=a target=finding-one"
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	*now = now.Add(16 * time.Minute)
	r := interaction.Result{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), Revision: 1, Phase: "prepare", State: "needs_confirmation", Scope: v.Command.Scope, Preparation: &interaction.Preparation{Hash: protocol.Hash("proposal"), Summary: "Accept A", ExpiresAt: now.Add(time.Minute)}}
	a, err := s.SubmitCommandResult(ctx, producer, r)
	must(t, err)
	if a.Disposition != "superseded_no_effect" || a.TerminalState != "expired" {
		t.Fatalf("late receipt: %+v", a)
	}
	again, err := s.SubmitCommandResult(ctx, producer, r)
	must(t, err)
	if again != a {
		t.Fatal("late receipt changed")
	}
	must(t, s.AckCommandResult(ctx, producer, a))
	stored := getCommand(t, s, v.Command.ID)
	if stored.State != "expired" || stored.Prepared != nil || stored.Grant != nil {
		t.Fatal("late preparation reopened command")
	}
	replies, err := s.PendingCommandReplies(ctx)
	must(t, err)
	if len(replies) != 0 {
		t.Fatal("late preparation sent")
	}
}
func TestConfirmationReplayKeepsOriginalCommand(t *testing.T) {
	s, now, _, in := commandFixture(t)
	ctx := context.Background()
	in.Text = "backup/storage accept-risk subject=a target=finding-one"
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	r := interaction.Result{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), Revision: 1, Phase: "prepare", State: "needs_confirmation", Scope: v.Command.Scope, Preparation: &interaction.Preparation{Hash: protocol.Hash("proposal"), Summary: "Accept A", ExpiresAt: now.Add(5 * time.Minute)}}
	_, err = s.SubmitCommandResult(ctx, producer, r)
	must(t, err)
	replies, err := s.PendingCommandReplies(ctx)
	must(t, err)
	var confirm string
	for _, r := range replies {
		if _, tail, ok := strings.Cut(r.Text, "\nconfirm "); ok {
			confirm = "confirm " + tail
		}
	}
	if confirm == "" {
		t.Fatal("no confirmation")
	}
	in.Text = confirm
	in.NativeID = "update-2"
	c, err := s.AdmitCommand(ctx, in)
	must(t, err)
	if c.Command.ID != v.Command.ID || c.State != "queued" {
		t.Fatal("confirmation minted command")
	}
	in.NativeID = "update-3"
	c, err = s.AdmitCommand(ctx, in)
	must(t, err)
	if c.Command.ID != v.Command.ID {
		t.Fatal("concurrent confirmation minted command")
	}
	in.NativeID = "update-4"
	in.Actor = "other"
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("foreign actor confirmed")
	}
}
func TestCommandScopeAndDestinationFencing(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	bad := in
	bad.Text = "backup/storage retry subject=b target=incident-one"
	if _, err := s.AdmitCommand(ctx, bad); err == nil {
		t.Fatal("subject escape")
	}
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	must(t, s.ImportCommand(ctx, producer, v.Command.ID, protocol.Hash(v.Command)))
	_, err = s.GrantCommand(ctx, producer, v.Command.ID)
	must(t, err)
	r := interaction.Result{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), Revision: 1, Phase: "execute", State: "succeeded", Scope: interaction.Scope{WholeWorker: true}, Text: "A and secret B"}
	if _, err = s.SubmitCommandResult(ctx, producer, r); err == nil {
		t.Fatal("overbroad result accepted")
	}
	r.Scope = v.Command.Scope
	r.Text = "A only"
	_, err = s.SubmitCommandResult(ctx, producer, r)
	must(t, err)
	s.config.Interaction.Destinations[0].NativeID = "different-chat"
	replies, err := s.PendingCommandReplies(ctx)
	must(t, err)
	if len(replies) != 0 {
		t.Fatal("reply redirected after destination edit")
	}
}
func TestCommandAPIRejectsCrossProducerAndUnexpectedFields(t *testing.T) {
	s, _, _, in := commandFixture(t)
	body, _ := json.Marshal(in)
	reply := s.Handle(context.Background(), protocol.Request{Version: 1, Expected: s.Identity(), Operation: "command.ingest", Interaction: body})
	if !reply.OK {
		t.Fatal(reply.Error)
	}
	var view interaction.View
	must(t, protocol.Decode(reply.Result, &view, protocol.MaxResponse))
	body, _ = json.Marshal(commandRef{ID: view.Command.ID})
	reply = s.Handle(context.Background(), protocol.Request{Version: 1, Expected: s.Identity(), Operation: "command.get", Producer: protocol.Producer{ID: "foreign", Epoch: "one"}, Interaction: body})
	if reply.OK {
		t.Fatal("foreign producer read")
	}
	reply = s.Handle(context.Background(), protocol.Request{Version: 1, Expected: s.Identity(), Operation: "command.get", Producer: producer, Interaction: body, EventID: "unexpected"})
	if reply.OK {
		t.Fatal("cross-operation field accepted")
	}
}
