package worker

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestNextRecoveryRetainsCompactedRequestFence(t *testing.T) {
	ctx := context.Background()
	f, _, parent, revision, s := managedTelegramHandoffFixture(t)
	var raw, target string
	must(t, s.db.QueryRow("SELECT body FROM provider_inbox WHERE account=? AND id=400", f.Original.Drain.Account).Scan(&raw))
	var old interaction.Inbound
	must(t, protocol.Decode([]byte(raw), &old, 8192))
	must(t, s.db.QueryRow("SELECT target_id FROM reconciliation_request_fences WHERE key=?", old.RequestKey()).Scan(&target))
	if target != "" {
		t.Fatal("fixture request already converted")
	}
	// Model a provider retention policy removing an already sealed, unconverted
	// inbox row. Both the restored and latest copies now lack that raw input.
	_, err := s.db.Exec("DELETE FROM provider_inbox")
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	snapshot, err := s.Snapshot(ctx, artifact)
	must(t, err)
	must(t, s.Close())
	_, err = QuarantineNextManagedDeliveryRecovery(ctx, f.Path, artifact, f.Config, snapshot.ID, snapshot.DatabaseHash, revision, parent)
	must(t, err)
	_, err = ReconcileTelegramHistory(ctx, f.Path, artifact, old.Account, f.Config)
	must(t, err)
	_, err = drainTelegramRecovery(ctx, f.Path, f.Config, old.Account, &fakeTelegramDrain{Account: old.Account, Batch: 16}, nil)
	must(t, err)
	for _, enrollment := range f.Config.Producers {
		_, err = FenceRecoveryOwnership(ctx, f.Path, f.Config, enrollment.Producer)
		must(t, err)
	}
	requests, err := OpenRequestRecoverySession(ctx, f.Path, f.Config)
	must(t, err)
	r := requests.(*recoveryReconciler)
	defer r.Close()
	_, err = r.SealRecoveryRequests(ctx)
	must(t, err)
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM provider_inbox").Scan(&count))
	if count != 0 {
		t.Fatal("fixture unexpectedly retained provider input")
	}
	must(t, r.db.QueryRow("SELECT target_id FROM reconciliation_request_fences WHERE key=?", old.RequestKey()).Scan(&target))
	if target != "" {
		t.Fatal("recovery minted a command mapping")
	}
	must(t, r.transaction(ctx, func(tx *sql.Tx) error {
		var rejected *interaction.Rejection
		if err := guardRequestAdmission(tx, old); !errors.As(err, &rejected) || !rejected.Silent {
			t.Fatalf("old input regained authority: %v", err)
		}
		fresh := old
		fresh.NativeID = "update-90000"
		// The unpublished recovery still blocks fresh intake. Unlike the old
		// input, it must not be classified as a permanently fenced request.
		if err := guardRequestAdmission(tx, fresh); !errors.Is(err, protocol.ErrRecoveryIncomplete) {
			t.Fatalf("fresh input acquired a historical fence: %v", err)
		}
		return nil
	}))
}
