package worker

import (
	"context"
	"errors"
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

func commandDeliveryLimitsFixture(t *testing.T) (string, Config, protocol.Identity, string, ManagedDeliveryOwner, string, interaction.View) {
	t.Helper()
	ctx := context.Background()
	s, _, path, in := commandFixture(t)
	c, id := s.config, s.Identity()
	must(t, s.Revoke(ctx, producer, "generation-one"))
	must(t, s.Stage(producer, "delivery-generation"))
	must(t, s.Close())
	revision, err := safefs.NewID("n")
	must(t, err)
	installation, err := safefs.NewID("n")
	must(t, err)
	owner := ManagedDeliveryOwner{Version: 1, Root: t.TempDir(), Installation: installation, Identity: id, ConfigHash: protocol.Hash(c)}
	s, err = OpenManagedRuntime(path, c, id, revision)
	must(t, err)
	must(t, s.EnrollManagedDelivery(owner))
	must(t, s.Close())
	s, err = OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
	must(t, err)
	must(t, s.Activate(producer, "delivery-generation"))
	must(t, s.CommitManagedCommandAdmission("read", protocol.Hash("read admission"), ""))
	must(t, s.ConfigureManagedCommands("read", protocol.Hash("read admission")))
	must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
	in.SentAt = time.Now().UTC()
	pending, err := s.AdmitCommand(ctx, in)
	must(t, err)
	head := protocol.Hash("disabled admission")
	must(t, s.CommitManagedCommandAdmission("drain", head, protocol.Hash("read admission")))
	must(t, s.Close())
	return path, c, id, revision, owner, head, pending
}

func TestCommandDeliveryLimitsPreserveQueuedCommandsAcrossInterruptions(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner, head, pending := commandDeliveryLimitsFixture(t)
			ownerBytes, err := os.ReadFile(filepath.Join(path, managedDeliveryOwnerFile))
			must(t, err)
			for cycle := range 2 {
				after := before
				after.MaxMessages += 10
				must(t, PreflightCommandDeliveryLimits(ctx, path, id, before, after, head))
				op, err := safefs.NewID("n")
				must(t, err)
				lost := errors.New("lost publication reply")
				_, err = migrateEnrolledPolicy(ctx, path, id, before, after, op, 31, func(at string) error {
					if at == boundary {
						return lost
					}
					return nil
				}, head)
				if !errors.Is(err, lost) {
					t.Fatal("missed boundary", err)
				}
				if _, err = MigrateCommandDeliveryLimits(ctx, path, id, before, after, op, protocol.Hash("different head")); err == nil {
					t.Fatal("changed pending authority")
				}
				receipt, err := MigrateCommandDeliveryLimits(ctx, path, id, before, after, op, head)
				must(t, err)
				repeated, err := MigrateCommandDeliveryLimits(ctx, path, id, before, after, op, head)
				must(t, err)
				if receipt.Version != 31 || receipt.CommandHistoryHash != head || protocol.Hash(receipt) != protocol.Hash(repeated) {
					t.Fatal("lost typed command authority")
				}
				if stale, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation")); err == nil {
					stale.Close()
					t.Fatal("stale config opened")
				}
				s, err := OpenManagedDelivery(path, after, id, revision, owner, protocol.Hash(owner), protocol.Hash(fmt.Sprint("renewal", cycle)))
				must(t, err)
				must(t, s.RequireManagedCommandHistory(head))
				got := getCommand(t, s, pending.Command.ID)
				if !reflect.DeepEqual(got, pending) {
					t.Fatal("budget update changed queued command")
				}
				next := protocol.Hash(fmt.Sprint("read", cycle))
				disabled := protocol.Hash(fmt.Sprint("drain", cycle))
				must(t, s.CommitManagedCommandAdmission("read", next, head))
				must(t, s.CommitManagedCommandAdmission("drain", disabled, next))
				head = disabled
				must(t, s.Close())
				actual, err := os.ReadFile(filepath.Join(path, managedDeliveryOwnerFile))
				must(t, err)
				if string(actual) != string(ownerBytes) {
					t.Fatal("budget update replaced original owner")
				}
				before = after
			}
		})
	}
}

func TestCommandDeliveryLimitsRefuseLostOrChangedAuthority(t *testing.T) {
	for _, damage := range []string{"head-file", "head-sql", "active-intake", "wrong-head", "changed-policy", "database-budget"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, before, id, revision, owner, head, _ := commandDeliveryLimitsFixture(t)
			after := before
			after.MaxMessages++
			s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
			must(t, err)
			switch damage {
			case "head-sql":
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", managedCommandHeadKey)
				must(t, err)
			case "active-intake":
				must(t, s.CommitManagedCommandAdmission("read", protocol.Hash("active again"), head))
				head = protocol.Hash("active again")
			case "wrong-head":
				head = protocol.Hash("unretained head")
			case "changed-policy":
				c := *after.Interaction
				c.Revision = "different"
				after.Interaction = &c
			case "database-budget":
				after.MaxDatabaseBytes = 1
			}
			must(t, s.Close())
			if damage == "head-file" {
				must(t, os.Remove(filepath.Join(path, "managed-commands", "head.json")))
			}
			if err = PreflightCommandDeliveryLimits(ctx, path, id, before, after, head); err == nil {
				t.Fatal("unsafe preflight accepted")
			}
			op, err := safefs.NewID("n")
			must(t, err)
			if _, err = MigrateCommandDeliveryLimits(ctx, path, id, before, after, op, head); err == nil {
				t.Fatal("unsafe migration accepted")
			}
			if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
				t.Fatal("rejected migration retained intent", err)
			}
		})
	}
}

func TestCommandDeliveryLimitsRetainSavedResultAndPendingReply(t *testing.T) {
	ctx := context.Background()
	path, before, id, revision, owner, head, pending := commandDeliveryLimitsFixture(t)
	s, err := OpenManagedDelivery(path, before, id, revision, owner, protocol.Hash(owner), protocol.Hash("activation"))
	must(t, err)
	readHead := protocol.Hash("result admission")
	must(t, s.CommitManagedCommandAdmission("read", readHead, head))
	must(t, s.ConfigureManagedCommands("read", readHead))
	must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
	must(t, s.ImportCommand(ctx, producer, pending.Command.ID, protocol.Hash(pending.Command)))
	grant, err := s.GrantCommand(ctx, producer, pending.Command.ID)
	must(t, err)
	if grant.ID == "" {
		t.Fatal("missing result grant")
	}
	result := interaction.Result{CommandID: pending.Command.ID, CommandHash: protocol.Hash(pending.Command), Revision: 1, Phase: "execute", State: "succeeded", Scope: pending.Command.Scope, Text: "retained result"}
	ack, err := s.SubmitCommandResult(ctx, producer, result)
	must(t, err)
	head = protocol.Hash("result disabled")
	must(t, s.CommitManagedCommandAdmission("drain", head, readHead))
	saved := getCommand(t, s, pending.Command.ID)
	replies, err := s.PendingCommandReplies(ctx)
	must(t, err)
	if len(replies) != 1 {
		t.Fatal("missing reply", replies)
	}
	must(t, s.Close())
	after := before
	after.MaxDatabaseBytes += 1 << 20
	op, err := safefs.NewID("n")
	must(t, err)
	_, err = MigrateCommandDeliveryLimits(ctx, path, id, before, after, op, head)
	must(t, err)
	s, err = OpenManagedDelivery(path, after, id, revision, owner, protocol.Hash(owner), protocol.Hash("renewal"))
	must(t, err)
	defer s.Close()
	if !reflect.DeepEqual(saved, getCommand(t, s, pending.Command.ID)) {
		t.Fatal("changed saved result")
	}
	got, err := s.PendingCommandReplies(ctx)
	must(t, err)
	if !reflect.DeepEqual(replies, got) {
		t.Fatal("changed pending replies")
	}
	again, err := s.SubmitCommandResult(ctx, producer, result)
	must(t, err)
	if ack != again {
		t.Fatal("changed result acknowledgment")
	}
}
