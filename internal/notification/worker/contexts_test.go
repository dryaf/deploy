package worker

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestNotificationContextEnrollmentAndImmutability(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	m := report(*now, "context-one", 1, 1, 1)
	m.Context = &protocol.InteractionContext{Version: 1, ID: "context", Revision: 1, Kind: "single", Targets: []protocol.Target{{Subject: "backup", Kind: "incident", ID: "incident-one", Occurrence: "occurrence-one", EvidenceRevision: 1, DecisionRevision: 1}}}
	if _, err := s.Enqueue(ctx, m); err == nil {
		t.Fatal("unenrolled extension accepted")
	}
	s.config.Producers[0].InteractiveContext = true
	s.config.Producers[0].ContextKinds = []string{"incident"}
	must(t, s.config.Validate())
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	m.ID = "context-two"
	m.Sequence = 2
	m.ReportRevision = 2
	m.Context.Targets[0].Occurrence = "replacement"
	if _, err = s.Enqueue(ctx, m); err == nil {
		t.Fatal("context revision retargeted")
	}
	m.Context.Revision = 2
	_, err = s.Enqueue(ctx, m)
	must(t, err)
	old, err := s.Get(ctx, producer, "context-one")
	must(t, err)
	if old.Message.Context.Targets[0].Occurrence != "occurrence-one" {
		t.Fatal("old reference changed")
	}
	m.ID = "context-three"
	m.Sequence = 3
	m.ReportRevision = 3
	m.Context.Targets[0].Kind = "unknown"
	if _, err = s.Enqueue(ctx, m); err == nil {
		t.Fatal("unapproved target kind accepted")
	}
}
