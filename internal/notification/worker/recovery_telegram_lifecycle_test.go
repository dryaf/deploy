package worker

import (
	"context"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestTelegramRenewalManagedAuditAndLease(t *testing.T) {
	f := newTelegramRenewalFixture(t)
	ctx := context.Background()
	commit, account := f.Publication.Commit.CommitHash, f.Original.Drain.Account
	head, err := ReviewTelegramRenewalHead(ctx, f.Path, f.Config, commit, account)
	must(t, err)
	if !head.Complete || head.ID != "" || head.Hash != f.Request.PreviousHash {
		t.Fatal(head)
	}
	one, err := f.run(nil)
	must(t, err)
	head, err = ReviewTelegramRenewalHead(ctx, f.Path, f.Config, commit, account)
	must(t, err)
	if !head.Complete || head.ID != one.Binding.ID || head.Hash != one.RenewalHash {
		t.Fatal(head)
	}
	f.At = f.At.Add(time.Hour)
	f.Up += 3600
	if _, err = f.verify(one.RenewalHash); err == nil {
		t.Fatal("expired handoff activated")
	}
	audited, err := AuditTelegramHandoffRenewal(ctx, f.Path, f.Config, commit, account, one.RenewalHash)
	must(t, err)
	if audited != one {
		t.Fatal("audit refreshed proof")
	}
	review, err := AdoptPublishedRecoveryVerification(ctx, f.Path, f.Config, commit)
	must(t, err)
	revision, err := safefs.NewID("n")
	must(t, err)
	for _, kind := range []string{"hash", "id"} {
		id, hash := one.Binding.ID, one.RenewalHash
		if kind == "hash" {
			hash = protocol.Hash("other")
		} else {
			id, err = safefs.NewID("n")
			must(t, err)
		}
		if s, e := OpenManagedTelegramVerification(f.Path, f.Config, review.After, revision, commit, account, id, hash); e == nil {
			s.Close()
			t.Fatal("wrong pin accepted", kind)
		}
	}
	s, err := OpenManagedTelegramVerification(f.Path, f.Config, review.After, revision, commit, account, one.Binding.ID, one.RenewalHash)
	must(t, err)
	defer s.Close()
	h, err := s.Health(ctx)
	must(t, err)
	if !h.VerificationOnly || h.TelegramRenewalID != one.Binding.ID {
		t.Fatal(h)
	}
	// Closing the borrowed audit session must not release the runtime's lock.
	if _, err = ReviewTelegramRenewalHead(ctx, f.Path, f.Config, commit, account); err == nil {
		t.Fatal("audit released runtime worker lease")
	}
	if _, err = f.run(nil); err == nil {
		t.Fatal("renewal ran concurrently with verification runtime")
	}
}
