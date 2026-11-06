package worker

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestRecoveryIntakePinsRefuseWrongOperationAndSnapshot(t *testing.T) {
	f := newTelegramHistoryFixture(t)
	ctx := context.Background()
	r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
	must(t, err)
	operation := r.journal.Operation
	must(t, r.Close())
	m, err := VerifySnapshot(ctx, f.History, f.Config)
	must(t, err)
	wrong, err := safefs.NewID("n")
	must(t, err)
	for _, bad := range []struct{ op, id, hash string }{{wrong, m.ID, m.DatabaseHash}, {operation, wrong, m.DatabaseHash}, {operation, m.ID, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}} {
		if _, err = ReconcilePinnedTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config, bad.op, bad.id, bad.hash); err == nil {
			t.Fatal("changed history pin accepted", bad)
		}
	}
	first, err := ReconcilePinnedTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config, operation, m.ID, m.DatabaseHash)
	must(t, err)
	again, err := ReconcilePinnedTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config, operation, m.ID, m.DatabaseHash)
	must(t, err)
	if first != again {
		t.Fatal("pinned history replay changed")
	}
	// Wrong operation is rejected before provider preflight, even without valid
	// credentials. No fake or live transport is needed for this negative case.
	if _, err = DrainPinnedTelegramRecovery(ctx, f.Path, f.Config, wrong, provider.TelegramPoller{Profile: provider.TelegramAccount{Account: f.Account, TokenFile: "/no-credential"}}); err == nil || err.Error() != "Telegram drain recovery operation changed" {
		t.Fatal(err)
	}
	if _, err = SealPinnedRecoveryRequests(ctx, f.Path, f.Config, wrong); err == nil {
		t.Fatal("wrong seal operation accepted")
	}
	if _, err = CommitPinnedTelegramRecoveryCursor(ctx, f.Path, f.Config, wrong, f.Account); err == nil {
		t.Fatal("wrong cursor operation accepted")
	}
}
