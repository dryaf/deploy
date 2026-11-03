package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func deliveredContext(t *testing.T, kind string) (*Store, *time.Time, string, interaction.Inbound, protocol.Message) {
	t.Helper()
	s, now, path, in := commandFixture(t)
	s.config.Producers[0].InteractiveContext = true
	s.config.Producers[0].ContextKinds = []string{"incident"}
	m := report(*now, "notification-one", 1, 1, 1)
	m.Context = &protocol.InteractionContext{Version: 1, ID: "incident-context", Revision: 1, Kind: kind, Targets: []protocol.Target{{Subject: "a", Kind: "incident", ID: "incident-one", Occurrence: "original-occurrence", EvidenceRevision: 7, DecisionRevision: 3}}}
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	j, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("delivery not granted")
	}
	d := s.config.Interaction.Destinations[0]
	must(t, s.Finish(ctx, j, Result{Accepted: true, Binding: &interaction.DeliveryBinding{Destination: d, Native: interaction.NativeReply{ID: "42", Sender: d.Account}}}))
	in.Reply = &interaction.NativeReply{ID: "42", Sender: d.Account}
	in.Text = "retry"
	return s, now, path, in, m
}

func TestNativeReferenceFreezesTargetAcrossRestartAndAliasRename(t *testing.T) {
	s, now, path, in, m := deliveredContext(t, "single")
	ctx := context.Background()
	c := s.config
	must(t, s.Close())
	c.Interaction.Workers[0].Alias = "backup/renamed"
	r, err := Open(path, c, false)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return *now }
	v, err := r.AdmitCommand(ctx, in)
	must(t, err)
	if v.Command.Producer != producer || v.Command.Arguments["target"] != "incident-one" || v.Command.Arguments["subject"] != "a" || v.Command.Context == nil || v.Command.Context.Context.Targets[0].Occurrence != "original-occurrence" || v.Command.Context.Context.Targets[0].EvidenceRevision != 7 {
		t.Fatal("target identity lost", v)
	}
	// A public reference identifies the same frozen revision without quoted prose.
	in.NativeID = "next-update"
	in.Reply = nil
	in.Text = "backup/renamed retry ref=" + protocol.NotificationReference(m)
	v2, err := r.AdmitCommand(ctx, in)
	must(t, err)
	if protocol.Hash(v.Command.Context) != protocol.Hash(v2.Command.Context) {
		t.Fatal("opaque reference resolved differently")
	}
}

func TestNativeReferenceRejectsForgedExpiredAndConflictingContexts(t *testing.T) {
	for _, name := range []string{"sender", "native-id", "conversation", "explicit-worker", "explicit-reference", "target", "subject", "permission", "generation", "expired"} {
		t.Run(name, func(t *testing.T) {
			s, now, _, in, _ := deliveredContext(t, "single")
			switch name {
			case "sender":
				in.Reply.Sender = "somebody-else"
			case "native-id":
				in.Reply.ID = "99"
			case "conversation":
				in.DestinationFingerprint = "different-conversation"
			case "explicit-worker":
				in.Text = "backup/other retry"
			case "explicit-reference":
				in.Text = "retry ref=N-wrong"
			case "target":
				in.Text = "retry target=other"
			case "subject":
				in.Text = "retry subject=b"
			case "permission":
				s.config.Interaction.Permissions = nil
			case "generation":
				s.config.Interaction.Workers[0].Generation = "replacement"
			case "expired":
				*now = now.Add(contextRetention + time.Second)
				in.SentAt = *now
			}
			if _, err := s.AdmitCommand(context.Background(), in); err == nil {
				t.Fatal("unsafe reference accepted")
			}
			var count int
			must(t, s.db.QueryRow("SELECT count(*) FROM commands").Scan(&count))
			if count != 0 {
				t.Fatal("rejection left a command")
			}
		})
	}
}

func TestSummaryRequiresSelectionEvenWithOneVisibleTarget(t *testing.T) {
	s, _, _, in, _ := deliveredContext(t, "summary")
	ctx := context.Background()
	if _, err := s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("summary silently selected target")
	}
	in.Text = "retry target=incident-one"
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	if v.Command.Context.Context.Kind != "summary" || v.Command.Arguments["subject"] != "a" {
		t.Fatal("summary selection lost")
	}
}

func TestContextReadFiltersUnauthorizedSubjects(t *testing.T) {
	s, _, _, in, m := deliveredContext(t, "summary")
	ctx := context.Background()
	// Extend only the test fixture's retained context, keeping a private B target.
	c := *m.Context
	c.Targets = append(c.Targets, protocol.Target{Subject: "b", Kind: "incident", ID: "private-b", Occurrence: "b-occurrence", EvidenceRevision: 1, DecisionRevision: 1})
	body, err := json.Marshal(c)
	must(t, err)
	_, err = s.db.Exec("UPDATE notification_references SET body=?", string(body))
	must(t, err)
	in.Text = "status"
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	if v.Command.Scope.Contains("b") || len(v.Command.Context.Context.Targets) != 1 {
		t.Fatal("private target disclosed")
	}
}

func TestNativeIdentityCollisionQuarantinesRatherThanRebinds(t *testing.T) {
	s, _, _, in, m := deliveredContext(t, "single")
	ctx := context.Background()
	m.ID = "another-event"
	m.Context.ID = "another-context"
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.freezeReference(tx, m); err != nil {
			return err
		}
		d := s.config.Interaction.Destinations[0]
		return s.acceptReference(tx, m, &interaction.DeliveryBinding{Destination: d, Native: interaction.NativeReply{ID: "42", Sender: d.Account}})
	}))
	if _, err := s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("conflicting native identity routed")
	}
}

func TestReservedAliasCannotBeReassignedByEditingConfiguration(t *testing.T) {
	s, _, _, in, _ := deliveredContext(t, "single")
	ctx := context.Background()
	_, err := s.AdmitCommand(ctx, in)
	must(t, err)
	w := &s.config.Interaction.Workers[0]
	w.Producer = protocol.Producer{ID: "replacement", Epoch: "replacement"}
	in.NativeID = "second-request"
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("old alias reassigned")
	}
}
