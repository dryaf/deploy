package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestRecoveryReviewReadsPendingIntentWithoutApplyingIt(t *testing.T) {
	f, d := dispositionFixture(t, "retry")
	ctx := context.Background()
	first, err := f.r.ReviewRecoveryMessage(ctx, producer, d.MessageID)
	must(t, err)
	must(t, first.Validate())
	if first.DecisionState != "none" || first.Decision != nil || first.MessageHash != d.MessageHash || first.SummaryHash != d.SummaryHash {
		t.Fatal(first)
	}
	d.ID = first.RequestID
	f.r.fault = func(stage string) error {
		if stage == "disposition_intent" {
			return errors.New("lost request before projection")
		}
		return nil
	}
	if _, err = f.r.DecideRecoveryMessage(ctx, d); err == nil {
		t.Fatal("fault missing")
	}
	f.r.fault = nil
	review, err := f.r.ReviewRecoveryMessage(ctx, producer, d.MessageID)
	must(t, err)
	if review.DecisionState != "intent" || review.Decision == nil || review.Decision.Decision != d {
		t.Fatal(review)
	}
	record, err := f.r.RecoveryMessageDisposition(ctx, producer, d.MessageID)
	if !errors.Is(err, protocol.ErrRecoveryIncomplete) || record.Decision != d {
		t.Fatal("pending record unavailable", record, err)
	}
	var count int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_dispositions").Scan(&count))
	if count != 0 {
		t.Fatal("inspection applied a choice")
	}
	_, err = f.r.DecideRecoveryMessage(ctx, review.Decision.Decision)
	must(t, err)
	review, err = f.r.ReviewRecoveryMessage(ctx, producer, d.MessageID)
	must(t, err)
	if review.DecisionState != "recorded" || review.RequestID != d.ID {
		t.Fatal(review)
	}
}

func TestRecoveryReviewRejectsUnreviewableOrChangedEvidence(t *testing.T) {
	for _, mode := range []string{"accepted", "summary", "hash", "producer", "file"} {
		t.Run(mode, func(t *testing.T) {
			f, d := dispositionFixture(t, "cancel")
			p, id := producer, d.MessageID
			switch mode {
			case "accepted":
				id = f.records[0].Message.ID
			case "summary":
				_, err := f.r.db.Exec("UPDATE reconciliation_producers SET completion=''")
				must(t, err)
			case "hash":
				_, err := f.r.db.Exec("UPDATE messages SET hash=? WHERE id=?", protocol.Hash("changed"), id)
				must(t, err)
			case "producer":
				p.Epoch = "foreign"
			case "file":
				must(t, f.r.stage.WriteJSON(dispositionFile(p, id), map[string]string{"unknown": "value"}, false))
			}
			if _, err := f.r.ReviewRecoveryMessage(context.Background(), p, id); err == nil {
				t.Fatal("unreviewable evidence accepted")
			}
		})
	}
}
