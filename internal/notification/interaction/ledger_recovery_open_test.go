package interaction

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestOpenRecoveryLedgerBeforeAndAfterCommit(t *testing.T) {
	l, db, _, gateway := commandCommitFixture(t)
	ch := gateway.proof.Summary.Challenge
	ctx := context.Background()
	for _, committed := range []bool{false, true} {
		if committed {
			_, err := CommitCommandRecovery(ctx, l, gateway)
			testutil.Must(t, err)
		}
		opened, err := OpenRecoveryLedger(ctx, db, l.producer, l.identity, ch.Identity, ch.Operation)
		testutil.Must(t, err)
		want := l.identity
		if committed {
			want = ch.Identity
		}
		if opened.identity != want {
			t.Fatal("wrong recovery identity", opened.identity)
		}
		proof, err := CommitCommandRecovery(ctx, opened, gateway)
		testutil.Must(t, err)
		if protocol.Hash(proof) != protocol.Hash(gateway.proof) {
			t.Fatal("historical proof changed")
		}
	}
}

func TestOpenRecoveryLedgerRejectsMissingOrForeignAuthority(t *testing.T) {
	for _, kind := range []string{"producer", "operation", "before", "after", "unproved-new-pin", "lost-operation", "lost-proof", "damaged-capture", "missing-pin"} {
		t.Run(kind, func(t *testing.T) {
			l, db, _, gateway := commandCommitFixture(t)
			ch := gateway.proof.Summary.Challenge
			p, before, after, operation := l.producer, l.identity, ch.Identity, ch.Operation
			ctx := context.Background()
			var err error
			switch kind {
			case "producer":
				p.ID = "foreign"
			case "operation":
				operation, _ = safefs.NewID("n")
			case "before":
				before.Recovery = "foreign"
			case "after":
				after.Recovery = "foreign"
			case "unproved-new-pin":
				raw, _ := json.Marshal(ledgerIdentityPin{Producer: p, Identity: after, RecoveryOperation: operation})
				_, err = db.Exec("UPDATE interactive_identity SET body=?", string(raw))
			case "lost-operation":
				_, err = db.Exec("UPDATE interactive_identity SET body=json_remove(body,'$.recovery_operation')")
			case "lost-proof":
				_, err = CommitCommandRecovery(ctx, l, gateway)
				testutil.Must(t, err)
				_, err = db.Exec("UPDATE interactive_recovery_commit SET proof=''")
			case "damaged-capture":
				_, err = db.Exec("DELETE FROM interactive_recovery_records")
			case "missing-pin":
				_, err = db.Exec("DELETE FROM interactive_identity")
			}
			testutil.Must(t, err)
			if _, err = OpenRecoveryLedger(ctx, db, p, before, after, operation); err == nil {
				t.Fatal("invalid recovery authority accepted")
			}
		})
	}
}

func TestOpenRecoveryLedgerDoesNotInitializeMissingState(t *testing.T) {
	l, db, _, _, _ := ledgerFixture(t)
	ch := recoveryChallenge(t, l)
	_, err := OpenRecoveryLedger(context.Background(), db, l.producer, l.identity, ch.Identity, ch.Operation)
	testutil.Must(t, err)
	_, err = db.Exec("DROP TABLE interactive_identity")
	testutil.Must(t, err)
	if _, err = OpenRecoveryLedger(context.Background(), db, l.producer, l.identity, ch.Identity, ch.Operation); err == nil {
		t.Fatal("missing identity reconstructed")
	}
	var count int
	testutil.Must(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_identity'").Scan(&count))
	if count != 0 {
		t.Fatal("missing identity created")
	}
}
