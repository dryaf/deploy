package worker

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedCommandAdmissionSeparatesDeliveryEffectsAndDrain(t *testing.T) {
	for _, mode := range []string{"off", "read", "all", "drain"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, in := commandFixture(t)
			defer s.Close()
			ctx := context.Background()
			mutation := in
			mutation.Text = "backup/storage retry subject=a target=incident-one"
			old, err := s.AdmitCommand(ctx, mutation)
			must(t, err)
			must(t, s.ImportCommand(ctx, producer, old.Command.ID, protocol.Hash(old.Command)))
			s.installationRevision, err = safefs.NewID("n")
			must(t, err)
			s.deliveryActivationHash = protocol.Hash("delivery")
			if mode != "off" {
				must(t, s.CommitManagedCommandAdmission(mode, protocol.Hash("admission"), ""))
				must(t, s.ConfigureManagedCommands(mode, protocol.Hash("admission")))
				must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
			}
			_, err = s.GrantCommand(ctx, producer, old.Command.ID)
			if (err == nil) != (mode == "all") {
				t.Fatal("wrong old mutation grant admission", mode, err)
			}
			_, err = s.AdmitCommand(ctx, mutation)
			if (err == nil) != (mode == "all") {
				t.Fatal("wrong mutation replay admission", mode, err)
			}
			in.NativeID = "update-2"
			v, err := s.AdmitCommand(ctx, in)
			if mode == "read" || mode == "all" {
				must(t, err)
				must(t, s.ImportCommand(ctx, producer, v.Command.ID, protocol.Hash(v.Command)))
				grant, err := s.GrantCommand(ctx, producer, v.Command.ID)
				must(t, err)
				if grant.ID == "" {
					t.Fatal("admitted read did not receive grant")
				}
			} else if err == nil {
				t.Fatal("disabled/draining intake admitted command")
			}
			calls := 0
			must(t, s.DispatchCommandReplies(ctx, replySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
				calls++
				return provider.ReplyResult{Result: provider.Result{Accepted: true}}
			})))
			if (calls > 0) != (mode != "off") {
				t.Fatal("reply drain differs", mode, calls)
			}
			if mode != "off" && s.ConfigureManagedCommands("all", protocol.Hash("changed")) == nil {
				t.Fatal("live gate changed without lifecycle transition")
			}
			if mode == "off" || mode == "drain" {
				if s.PersistTelegram(ctx, provider.IntakePage{}) == nil || s.ConvertIntake(ctx) == nil {
					t.Fatal("disabled intake changed persistent state")
				}
				query := in
				query.Text = "workers"
				if _, err = s.AdmitGatewayQuery(ctx, query); err == nil {
					t.Fatal("disabled query admitted")
				}
			}
		})
	}
}

func TestManagedCommandsCannotEnableVerificationOrClosedStore(t *testing.T) {
	s, _, _, _ := commandFixture(t)
	if s.ConfigureManagedCommands("all", protocol.Hash("admission")) == nil {
		t.Fatal("unmanaged runtime acquired managed admission")
	}
	s.installationRevision = "n-01234567890123456789012345678901"
	s.deliveryActivationHash = protocol.Hash("delivery")
	s.verificationOnly = true
	if s.ConfigureManagedCommands("all", protocol.Hash("admission")) == nil {
		t.Fatal("verification runtime activated commands")
	}
	s.verificationOnly = false
	must(t, s.Close())
	if s.ConfigureManagedCommands("all", protocol.Hash("admission")) == nil {
		t.Fatal("closed store acquired command authority")
	}
}
