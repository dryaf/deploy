package worker

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestOwnershipObservationFollowsCanonicalLifecycle(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	read := func(g, state string, active bool) {
		t.Helper()
		reply := s.Handle(ctx, protocol.Request{Version: protocol.Version, Expected: s.Identity(), Operation: "producer.ownership", Producer: producer, Generation: g})
		if !reply.OK {
			t.Fatal(reply.Error)
		}
		var status protocol.OwnershipStatus
		must(t, protocol.Decode(reply.Result, &status, 4096))
		if status.Producer != producer || status.Generation != g || status.State != state || status.Active != active {
			t.Fatal(status)
		}
	}
	read("generation-one", "active", true)
	read("generation-two", "unknown", false)
	must(t, s.Stage(producer, "generation-two"))
	read("generation-two", "staged", false)
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	m := report(*now, "inflight", 1, 1, 1)
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	attempt, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("attempt not granted")
	}
	if err = s.Revoke(ctx, producer, "generation-one"); err == nil {
		t.Fatal("in-flight generation revoked prematurely")
	}
	read("generation-one", "draining", false)
	must(t, s.Finish(ctx, attempt, Result{Accepted: true}))
	must(t, s.Revoke(ctx, producer, "generation-one"))
	read("generation-one", "revoked", false)
	must(t, s.Activate(producer, "generation-two"))
	read("generation-two", "active", true)
	read("generation-one", "revoked", false)
}

func TestOwnershipObservationRejectsWrongScopeAndUnexpectedFields(t *testing.T) {
	s, _, _ := fixture(t)
	base := protocol.Request{Version: protocol.Version, Expected: s.Identity(), Operation: "producer.ownership", Producer: producer, Generation: "generation-one"}
	for _, kind := range []string{"epoch", "identity", "empty", "extra", "mutation"} {
		r := base
		switch kind {
		case "epoch":
			r.Producer.Epoch = "wrong"
		case "identity":
			r.Expected.Recovery = "different"
		case "empty":
			r.Generation = ""
		case "extra":
			r.EventID = "unexpected"
		case "mutation":
			r.Operation = "producer.activate"
		}
		if reply := s.Handle(context.Background(), r); reply.OK {
			t.Fatal("ownership query allowed invalid request", kind)
		}
	}
	state, err := s.OwnershipStatus(producer, "generation-one")
	must(t, err)
	if !state.Active {
		t.Fatal("invalid query changed ownership")
	}
}
