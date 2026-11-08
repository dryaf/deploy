package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestCommandProducerPolicyQueueAndIdentityGuards(t *testing.T) {
	for _, field := range []string{"quota", "undersized-quota", "required", "routes", "health", "context", "identity", "global-budget", "receiver"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner, head, command := commandDeliveryLimitsFixture(t)
			s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
			must(t, err)
			for i := range 2 {
				m := report(time.Now().UTC(), fmt.Sprint("pending-policy-", i), uint64(i+1), 1, 1)
				m.Generation = "delivery-generation"
				m.ReportKey = fmt.Sprint("policy-report-", i)
				_, err = s.Enqueue(ctx, m)
				must(t, err)
			}
			must(t, s.Close())
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			switch field {
			case "quota":
				after.Producers[0].MaxMessages++
			case "undersized-quota":
				after.Producers[0].MaxMessages = 1
			case "required":
				after.Producers[0].Required = !after.Producers[0].Required
			case "routes":
				after.Producers[0].Routes = nil
			case "health":
				after.Producers[0].PolicyHealth = !after.Producers[0].PolicyHealth
			case "context":
				after.Producers[0].InteractiveContext = true
				after.Producers[0].ContextKinds = []string{"incident"}
			case "identity":
				after.Producers[0].Producer.Epoch = "different"
			case "global-budget":
				after.MaxMessages++
			case "receiver":
				p := *after.Interaction
				p.Revision = "changed"
				after.Interaction = &p
			}
			err = PreflightCommandDeliveryProducerPolicy(ctx, path, id, before, after, head)
			op, e := safefs.NewID("n")
			must(t, e)
			if field != "quota" {
				if err == nil {
					t.Fatal("unsafe review accepted")
				}
				if _, err = MigrateCommandDeliveryProducerPolicy(ctx, path, id, before, after, op, head); err == nil {
					t.Fatal("unsafe policy migrated")
				}
				if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
					t.Fatal("refusal retained intent", err)
				}
				return
			}
			must(t, err)
			_, err = MigrateCommandDeliveryProducerPolicy(ctx, path, id, before, after, op, head)
			must(t, err)
			s, err = OpenManagedDelivery(path, after, id, revision, owner, protocol.Hash(owner), protocol.Hash("renewal"))
			must(t, err)
			if !reflect.DeepEqual(getCommand(t, s, command.Command.ID), command) {
				t.Fatal("quota changed pending request")
			}
			must(t, s.Close())
		})
	}
}

func TestCommandProducerAccessRetainsPendingCommandsWhenMessagesSettled(t *testing.T) {
	for _, field := range []string{"required", "routes", "health", "context"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner, head, pending := commandDeliveryLimitsFixture(t)
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			switch field {
			case "required":
				after.Producers[0].Required = !after.Producers[0].Required
			case "routes":
				after.Producers[0].Routes = nil
			case "health":
				after.Producers[0].PolicyHealth = !after.Producers[0].PolicyHealth
			case "context":
				after.Producers[0].InteractiveContext = true
				after.Producers[0].ContextKinds = []string{"incident"}
			}
			must(t, PreflightCommandDeliveryProducerPolicy(ctx, path, id, before, after, head))
			op, err := safefs.NewID("n")
			must(t, err)
			_, err = MigrateCommandDeliveryProducerPolicy(ctx, path, id, before, after, op, head)
			must(t, err)
			s, err := OpenManagedDelivery(path, after, id, revision, owner, protocol.Hash(owner), protocol.Hash("renewal"))
			must(t, err)
			must(t, s.RequireManagedCommandHistory(head))
			if !reflect.DeepEqual(getCommand(t, s, pending.Command.ID), pending) {
				t.Fatal("access update changed queued command")
			}
			must(t, s.Close())
		})
	}
}

func TestCommandProducerDeclarationPreflightRetainsPendingWorkAndRequiresHead(t *testing.T) {
	ctx := context.Background()
	path, c, id, revision, owner, head, pending := commandDeliveryLimitsFixture(t)
	s, err := OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
	must(t, err)
	m := report(time.Now().UTC(), "pending-declaration", 1, 1, 1)
	m.Generation = "delivery-generation"
	queued, err := s.Enqueue(ctx, m)
	must(t, err)
	must(t, s.Close())
	original, err := os.ReadFile(filepath.Join(path, managedDeliveryOwnerFile))
	must(t, err)
	for range 2 {
		must(t, PreflightCommandDeliveryDeclaration(ctx, path, id, c, head))
	}
	if err = PreflightCommandDeliveryDeclaration(ctx, path, id, c, protocol.Hash("wrong-head")); err == nil {
		t.Fatal("wrong head accepted")
	}
	if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
		t.Fatal("declaration wrote migration intent", err)
	}
	s, err = OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
	must(t, err)
	got, err := s.Get(ctx, producer, m.ID)
	must(t, err)
	if !reflect.DeepEqual(got, queued) || !reflect.DeepEqual(getCommand(t, s, pending.Command.ID), pending) {
		t.Fatal("declaration modified work")
	}
	next := protocol.Hash("active")
	must(t, s.CommitManagedCommandAdmission("read", next, head))
	must(t, s.Close())
	if err = PreflightCommandDeliveryDeclaration(ctx, path, id, c, next); err == nil {
		t.Fatal("declaration accepted active intake")
	}
	after, err := os.ReadFile(filepath.Join(path, managedDeliveryOwnerFile))
	must(t, err)
	if string(original) != string(after) {
		t.Fatal("declaration replaced owner")
	}
}

func TestCommandProducerContextCapabilityPreservesReceiverScope(t *testing.T) {
	ctx := context.Background()
	path, before, id, revision, owner, head, _ := commandDeliveryLimitsFixture(t, func(c *Config) { c.Interaction.Workers[0].Generation = "delivery-generation" })
	after := before
	after.Producers = append([]Enrollment(nil), before.Producers...)
	after.Producers[0].InteractiveContext = true
	after.Producers[0].ContextKinds = []string{"incident"}
	op, err := safefs.NewID("n")
	must(t, err)
	_, err = MigrateCommandDeliveryProducerPolicy(ctx, path, id, before, after, op, head)
	must(t, err)
	s, err := OpenManagedDelivery(path, after, id, revision, owner, protocol.Hash(owner), protocol.Hash("renewal"))
	must(t, err)
	defer s.Close()
	active := protocol.Hash("active-read")
	must(t, s.CommitManagedCommandAdmission("read", active, head))
	must(t, s.ConfigureManagedCommands("read", active))
	must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
	must(t, s.Replace(ctx, snapshot(time.Now().UTC(), 1, 1)))
	m := report(time.Now().UTC(), "context-after-policy", 1, 1, 1)
	m.Generation = "delivery-generation"
	m.Context = &protocol.InteractionContext{Version: 1, ID: "policy-context", Revision: 1, Kind: "summary", Targets: []protocol.Target{{Subject: "a", Kind: "incident", ID: "allowed", Occurrence: "one", EvidenceRevision: 1, DecisionRevision: 1}, {Subject: "b", Kind: "incident", ID: "private", Occurrence: "two", EvidenceRevision: 1, DecisionRevision: 1}}}
	_, err = s.Enqueue(ctx, m)
	must(t, err)
	grant, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("no context delivery grant")
	}
	dst := after.Interaction.Destinations[0]
	must(t, s.Finish(ctx, grant, Result{Accepted: true, Binding: &interaction.DeliveryBinding{Destination: dst, Native: interaction.NativeReply{ID: "71", Sender: dst.Account}}}))
	in := interaction.Inbound{Provider: dst.Provider, Account: dst.Account, NativeID: "context-request", Actor: "actor-1", Conversation: dst.ID, DestinationFingerprint: dst.Fingerprint(), SentAt: time.Now().UTC(), Text: "status", Reply: &interaction.NativeReply{ID: "71", Sender: dst.Account}}
	got, err := s.AdmitCommand(ctx, in)
	must(t, err)
	if got.Command.Context == nil || len(got.Command.Context.Context.Targets) != 1 || got.Command.Scope.Contains("b") || got.Command.Context.Context.Targets[0].ID != "allowed" {
		t.Fatal("context capability expanded receiver scope", got)
	}
	in.NativeID = "unauthorized-context"
	in.Actor = "stranger"
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("context enabled a new actor")
	}
	if protocol.Hash(before.Interaction) != protocol.Hash(after.Interaction) {
		t.Fatal("producer update rewrote receiver policy")
	}
}
