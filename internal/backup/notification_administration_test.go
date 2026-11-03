//go:build linux

package backup

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
)

// The fixture runs only the Unix API; there is no provider dispatcher. This
// exercises the real producer receipt importer against administrative outcomes.
func TestBackupAdministrativeCancellationRetainsDeliveryObligation(t *testing.T) {
	f := newPipelineFixture(t)
	f.observe(t, "a", "backup unavailable", f.now)
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	f.transfer(t, f.client)
	ctx := context.Background()
	v, err := f.gateway.ReviewMessage(ctx, f.binding.Producer, messages[0].ID)
	testutil.Must(t, err)
	d := worker.MessageDecision{Version: 1, Review: v, Action: "cancel", Reason: "Operator cancelled this delivery, not the incident"}
	record, err := f.gateway.DecideMessage(ctx, d)
	testutil.Must(t, err)
	if record.Receipt.Acceptance != nil {
		t.Fatal("cancellation fabricated acceptance")
	}
	f.transfer(t, f.client)
	var open, accepted int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	testutil.Must(t, f.state.db.QueryRow("SELECT accepted FROM notification_exports WHERE id=?", messages[0].ID).Scan(&accepted))
	if open != 1 || accepted != 0 {
		t.Fatal("administrative cancellation satisfied a producer obligation", open, accepted)
	}
	replacement := f.freeze(t, 2, 1)
	if len(replacement) != 1 || replacement[0].ID == messages[0].ID {
		t.Fatal("producer did not preserve obligation in a new event", replacement)
	}
	f.transfer(t, f.client)
	f.accept(t, replacement[0])
	f.transfer(t, f.client)
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	if open != 0 {
		t.Fatal("real replacement acceptance did not satisfy obligation")
	}
	// A lost administrative response cannot cancel the replacement later.
	_, err = f.gateway.DecideMessage(ctx, d)
	testutil.Must(t, err)
	j, err := f.gateway.Get(ctx, f.binding.Producer, replacement[0].ID)
	testutil.Must(t, err)
	if j.State != "provider_accepted" {
		t.Fatal("historical decision changed replacement", j.State)
	}
}

func TestBackupAdministrativeRetryWaitsForActualAcceptance(t *testing.T) {
	f := newPipelineFixture(t)
	f.observe(t, "a", "backup unavailable", f.now)
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	f.transfer(t, f.client)
	ctx := context.Background()
	j, granted, err := f.gateway.Grant(ctx, f.binding.Producer, messages[0].ID)
	testutil.Must(t, err)
	if !granted {
		t.Fatal("fixture grant unavailable")
	}
	testutil.Must(t, f.gateway.Finish(ctx, j, worker.Result{Ambiguous: true}))
	f.transfer(t, f.client)
	v, err := f.gateway.ReviewMessage(ctx, f.binding.Producer, messages[0].ID)
	testutil.Must(t, err)
	d := worker.MessageDecision{Version: 1, Review: v, Action: "retry", Reason: "Reviewed uncertain delivery", AcceptDuplicateRisk: true}
	_, err = f.gateway.DecideMessage(ctx, d)
	testutil.Must(t, err)
	f.transfer(t, f.client)
	var open, accepted int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	testutil.Must(t, f.state.db.QueryRow("SELECT accepted FROM notification_exports WHERE id=?", messages[0].ID).Scan(&accepted))
	if open != 1 || accepted != 0 {
		t.Fatal("pending retry fabricated delivery", open, accepted)
	}
	f.accept(t, messages[0])
	f.transfer(t, f.client)
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
	if open != 0 {
		t.Fatal("actual acceptance failed to satisfy delivery obligation")
	}
	_, err = f.gateway.DecideMessage(ctx, d)
	testutil.Must(t, err)
	j, err = f.gateway.Get(ctx, f.binding.Producer, messages[0].ID)
	testutil.Must(t, err)
	if j.State != "provider_accepted" || j.Tries != 2 {
		t.Fatal("replayed administrative retry changed accepted event", j)
	}
}
