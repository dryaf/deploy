package interaction

import (
	"context"
	"fmt"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandLedgerRepeatedRecovery(t *testing.T) {
	ctx := context.Background()
	l, db, _, gateway := commandCommitFixture(t)
	_, err := CommitCommandRecovery(ctx, l, gateway)
	testutil.Must(t, err)
	original := l.identity
	for epoch := 2; epoch <= 4; epoch++ {
		before := gateway.proof.Summary.Challenge.Identity
		temp := &Ledger{db: db, producer: l.producer, identity: before}
		ch := recoveryChallenge(t, temp)
		ch.Identity.Recovery = fmt.Sprintf("epoch-%d", epoch)
		next, err := OpenNextRecoveryLedger(ctx, db, l.producer, before, ch.Identity, ch.Operation)
		testutil.Must(t, err)
		if _, err = next.PendingResults(ctx); err == nil {
			t.Fatal("next preparation enabled old result outbox")
		}
		next, err = OpenNextRecoveryLedger(ctx, db, l.producer, before, ch.Identity, ch.Operation)
		testutil.Must(t, err)
		manifest, err := next.CaptureRecovery(ctx, ch)
		testutil.Must(t, err)
		page, err := next.RecoveryPage(ctx, ch, 0)
		testutil.Must(t, err)
		if len(page.Records) != 1 || page.Records[0].HistoricalIdentity == nil || *page.Records[0].HistoricalIdentity != original || !page.Records[0].Fenced {
			t.Fatal("original receipt epoch or execution fence lost")
		}
		summary := CommandRecoverySummary{Challenge: ch, Manifest: manifest, Terminal: 1}
		testutil.Must(t, next.recordRecoverySummary(ctx, summary))
		gateway = commitGatewayFixture{proof: CommandRecoveryCommit{Version: 1, Summary: summary, ResultFloorsHash: CommandRecoveryDigest([]string{protocol.Hash(RecoveryResultFloor(page.Records[0]))})}}
		_, err = CommitCommandRecovery(ctx, next, gateway)
		testutil.Must(t, err)
		_, err = OpenNextRecoveryLedger(ctx, db, l.producer, before, ch.Identity, ch.Operation)
		testutil.Must(t, err)
		if _, err = next.PendingResults(ctx); err == nil {
			t.Fatal("recovery admitted old result outbox")
		}
	}
	_, err = db.Exec("DELETE FROM interactive_recovery_ancestors WHERE rowid=(SELECT min(rowid) FROM interactive_recovery_ancestors)")
	testutil.Must(t, err)
	before := gateway.proof.Summary.Challenge.Identity
	ch := recoveryChallenge(t, &Ledger{db: db, producer: l.producer, identity: before})
	if _, err = OpenNextRecoveryLedger(ctx, db, l.producer, before, ch.Identity, ch.Operation); err == nil {
		t.Fatal("missing original epoch accepted")
	}
}
