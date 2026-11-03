package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func policyFixture(t *testing.T) (*Store, *time.Time, string) {
	s, now, dir := fixture(t)
	s.config.Producers[0].PolicyHealth = true
	s.config.Producers[0].MaxMessages = 1000
	return s, now, dir
}
func transition(now time.Time, id string, rev, seq uint64) protocol.FaultTransition {
	return protocol.FaultTransition{ID: id, Subject: "backup", FaultRevision: rev, Cause: "resolved", StatusSequence: seq, BasisRevision: seq, ObservedAt: now, ValidUntil: now.Add(time.Hour)}
}
func TestTransitionBacklogIndependentOfFullSupport(t *testing.T) {
	s, now, _ := policyFixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	v.Subjects[0].FaultRevision = 1
	must(t, s.Replace(ctx, v))
	for n := 0; n < 128; n++ {
		v.Support = append(v.Support, protocol.AcceptanceSupport{Subject: "backup", Decision: fmt.Sprintf("decision-%d", n), Revision: 1, Occurrence: fmt.Sprintf("finding-%d", n), Assumptions: 1, ValidUntil: now.Add(30 * time.Minute)})
	}
	// 160 pending records cannot fit in the full snapshot; they arrive first
	// through independent replayable batches while their required basis waits.
	for offset := 0; offset < 160; offset += 32 {
		batch := protocol.TransitionBatch{Producer: producer}
		for n := offset; n < offset+32; n++ {
			batch.Records = append(batch.Records, transition(*now, fmt.Sprintf("transition-%d", n), uint64(n+1), 2))
		}
		r, err := s.EnqueueTransitions(ctx, batch)
		must(t, err)
		if len(r.Records) != 32 || r.Identity != s.Identity() {
			t.Fatal("receipt lost identities")
		}
		again, err := s.EnqueueTransitions(ctx, batch)
		must(t, err)
		if protocol.Hash(r) != protocol.Hash(again) {
			t.Fatal("replay changed receipt")
		}
	}
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "critical" {
		t.Fatal("future transition cleared fault")
	}
	v.Sequence = 2
	v.BasisRevision = 2
	v.Subjects[0].Condition = "ok"
	must(t, s.Replace(ctx, v))
	h, err = s.Health(ctx)
	must(t, err)
	if h.Condition != "ok" {
		t.Fatalf("backlog did not drain: %s", h.Condition)
	}
	var pending int
	must(t, s.db.QueryRow("SELECT count(*) FROM fault_transitions WHERE state='pending'").Scan(&pending))
	if pending != 0 {
		t.Fatal(pending)
	}
	// A later incarnation cannot be cleared by replay of a consumed transition.
	v.Sequence = 3
	v.BasisRevision = 3
	v.Subjects[0].Condition = "critical"
	v.Subjects[0].FaultRevision = 2
	must(t, s.Replace(ctx, v))
	_, err = s.EnqueueTransitions(ctx, protocol.TransitionBatch{Producer: producer, Records: []protocol.FaultTransition{transition(*now, "transition-0", 1, 2)}})
	must(t, err)
	h, err = s.Health(ctx)
	must(t, err)
	if h.Condition != "critical" {
		t.Fatal("replay cleared new fault")
	}
}
func TestTransitionAtomicConflictAndQuota(t *testing.T) {
	s, now, _ := policyFixture(t)
	ctx := context.Background()
	s.config.Producers[0].MaxMessages = 1
	a := transition(*now, "a", 1, 2)
	_, err := s.EnqueueTransitions(ctx, protocol.TransitionBatch{Producer: producer, Records: []protocol.FaultTransition{a}})
	must(t, err)
	b := a
	b.ID = "b"
	bad := a
	bad.Cause = "retired"
	if _, err = s.EnqueueTransitions(ctx, protocol.TransitionBatch{Producer: producer, Records: []protocol.FaultTransition{b, bad}}); err == nil {
		t.Fatal("invalid batch admitted")
	}
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM fault_transitions").Scan(&count))
	if count != 1 {
		t.Fatal("partial admission")
	}
	// Transition quota pressure must not reject fresh current state.
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
}
func TestAcceptanceDeadlineAndSeparateFaults(t *testing.T) {
	s, now, dir := policyFixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	v.Subjects[0].FaultRevision = 1
	must(t, s.Replace(ctx, v))
	a := protocol.AcceptanceSupport{Subject: "backup", Decision: "accept-a", Revision: 1, Occurrence: "finding-a", Assumptions: 1, ValidUntil: now.Add(10 * time.Second)}
	v.Sequence = 2
	v.BasisRevision = 2
	v.Subjects[0].Condition = "ok"
	v.Support = []protocol.AcceptanceSupport{a}
	must(t, s.Replace(ctx, v))
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "critical" {
		t.Fatal("support alone cleared fault")
	}
	r := transition(*now, "accept-transition", 1, 2)
	r.Cause = "policy_accepted"
	r.Support = []protocol.DecisionRef{{ID: a.Decision, Revision: 1}}
	_, err = s.EnqueueTransitions(ctx, protocol.TransitionBatch{Producer: producer, Records: []protocol.FaultTransition{r}})
	must(t, err)
	h, err = s.Health(ctx)
	must(t, err)
	if h.Condition != "ok" || !h.ValidUntil.Equal(a.ValidUntil) {
		t.Fatalf("deadline not clamped: %+v", h)
	}
	*now = now.Add(11 * time.Second)
	h, err = s.Health(ctx)
	must(t, err)
	if h.Condition == "ok" {
		t.Fatal("expired acceptance still healthy")
	}
	c := s.config
	must(t, s.Close())
	reopened, err := Open(dir, c, false)
	must(t, err)
	defer reopened.Close()
	var count int
	must(t, reopened.db.QueryRow("SELECT count(*) FROM fault_transitions WHERE state='consumed'").Scan(&count))
	if count != 1 {
		t.Fatal("transition not durable")
	}
	// Restart does not reuse old freshness or lose the consumed transition.
	h, err = reopened.Health(ctx)
	must(t, err)
	if h.Condition == "ok" {
		t.Fatal("restart renewed evidence")
	}
}
