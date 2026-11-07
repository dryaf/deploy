package install

import (
	"context"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"testing"
	"time"
)

func TestDeliveryDeclarationReviewDoesNotPauseLiveRuntime(t *testing.T) {
	ctx := context.Background()
	spec, i, services, c, before, q := deliveryFixture(t)
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(q), false)
	testutil.Must(t, err)
	s, err := openManagedStore(active, c)
	testutil.Must(t, err)
	defer s.Close()
	producer := c.Worker.Producers[0].Producer
	testutil.Must(t, s.Activate(producer, "one"))
	now := time.Now().UTC()
	msg := protocol.Message{Producer: producer, Sequence: 1, ID: "pending-during-declaration", Generation: "one", Subject: "subject", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "pending report", Route: "ops", ReportKey: "day", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
	queued, err := s.Enqueue(ctx, msg)
	testutil.Must(t, err)
	raw, err := os.ReadFile(spec.RuntimeFile)
	testutil.Must(t, err)
	starts, stops := services.starts, services.stops
	i.deliveryLimitsProbe = func(context.Context, *safefs.Directory, string, string) error {
		t.Fatal("declaration required a migration capability")
		return nil
	}
	review, err := i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(active), raw, "declaration")
	testutil.Must(t, err)
	if protocol.Hash(review.Before) != protocol.Hash(active) || starts != services.starts || stops != services.stops {
		t.Fatal("review changed delivery")
	}
	got, err := s.Get(ctx, producer, msg.ID)
	testutil.Must(t, err)
	if protocol.Hash(got) != protocol.Hash(queued) {
		t.Fatal("review changed pending work")
	}
	var input RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
	input.Worker.MaxMessages++
	raw, err = input.Bytes()
	testutil.Must(t, err)
	if _, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(active), raw, "declaration"); err == nil {
		t.Fatal("effective edit passed as unchanged declaration")
	}
	if starts != services.starts || stops != services.stops {
		t.Fatal("failed review changed service activity")
	}
}
