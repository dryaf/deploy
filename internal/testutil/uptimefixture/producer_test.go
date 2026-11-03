package uptimefixture

import (
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestProbeEvidenceExpiresAndCannotRegress(t *testing.T) {
	now := time.Now().UTC()
	p, err := New(protocol.Producer{ID: "uptime", Epoch: "test-one"}, []string{"web", "api"})
	if err != nil {
		t.Fatal(err)
	}
	v := Observation{Subject: "web", Revision: 1, At: now, Until: now.Add(10 * time.Second), Reachable: true}
	if err = p.Observe(v); err != nil {
		t.Fatal(err)
	}
	a, err := p.Evaluate(now)
	if err != nil {
		t.Fatal(err)
	}
	if a.Subjects[0].Condition != "unknown" {
		t.Fatal("missing target considered healthy")
	}
	if err = p.Observe(v); err != nil {
		t.Fatal("identical observation retry", err)
	}
	v.Reachable = false
	if p.Observe(v) == nil {
		t.Fatal("conflicting probe replay")
	}
	v.Revision++
	if err = p.Observe(v); err != nil {
		t.Fatal(err)
	}
	b, err := p.Evaluate(now)
	if err != nil {
		t.Fatal(err)
	}
	if b.Subjects[0].Condition != "critical" || b.BasisRevision <= a.BasisRevision {
		t.Fatal(b)
	}
	c, err := p.Evaluate(now.Add(11 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if c.Subjects[0].Condition != "unknown" {
		t.Fatal("stale probe renewed")
	}
	if _, err = p.Evaluate(now); err == nil {
		t.Fatal("clock rollback accepted")
	}
	if _, err = p.Report(b, "one", "ops", "day"); err == nil {
		t.Fatal("old status made new report")
	}
	if err = p.Retire("web"); err != nil {
		t.Fatal(err)
	}
	if p.Observe(v) == nil {
		t.Fatal("retirement silently reversed")
	}
	if err = p.Retire("api"); err != nil {
		t.Fatal(err)
	}
	d, err := p.Evaluate(now.Add(12 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if d.Subjects[0].Condition != "retired" {
		t.Fatal(d)
	}
}
