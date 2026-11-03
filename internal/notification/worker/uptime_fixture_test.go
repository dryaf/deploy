package worker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil/uptimefixture"
)

func TestSyntheticUptimeSharesGatewayWithoutClearingScanner(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	uptime := protocol.Producer{ID: "uptime", Epoch: "one"}
	scanner := protocol.Producer{ID: "scanner", Epoch: "one"}
	c := configuration()
	c.Producers = nil
	for _, p := range []protocol.Producer{uptime, scanner} {
		c.Producers = append(c.Producers, Enrollment{Producer: p, Required: true, Routes: []string{"operations"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 3600, MaxMessages: 100})
		c.LEDBindings = append(c.LEDBindings, LEDBinding{ID: p.ID, Producer: p, Subjects: []string{"*"}, LED: "green"})
	}
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	s, err := Open(path, c, true)
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return now }
	for _, p := range []protocol.Producer{uptime, scanner} {
		must(t, s.Stage(p, "generation-one"))
		must(t, s.Activate(p, "generation-one"))
	}
	u, err := uptimefixture.New(uptime, []string{"web", "api"})
	must(t, err)
	must(t, u.Observe(uptimefixture.Observation{Subject: "web", Revision: 1, At: now, Until: now.Add(time.Minute), Reachable: true}))
	must(t, u.Observe(uptimefixture.Observation{Subject: "api", Revision: 1, At: now, Until: now.Add(15 * time.Second), Reachable: false}))
	v, err := u.Evaluate(now)
	must(t, err)
	m, err := u.Report(v, "generation-one", "operations", "2026-11-01")
	must(t, err)
	_, err = s.Enqueue(ctx, m)
	must(t, err)
	_, ready, err := s.Grant(ctx, uptime, m.ID)
	must(t, err)
	if ready {
		t.Fatal("uptime report sent before status dependency")
	}
	must(t, s.Replace(ctx, v))
	job, ready, err := s.Grant(ctx, uptime, m.ID)
	must(t, err)
	if !ready {
		t.Fatal("fresh uptime status did not release its report")
	}
	must(t, s.Finish(ctx, job, Result{Accepted: true}))
	scan := protocol.Snapshot{Producer: scanner, Sequence: 1, BasisRevision: 1, ConfigRevision: "synthetic", EvaluatedAt: now, Subjects: []protocol.Subject{{Key: "host-one", Condition: "critical", Reason: "SYNTHETIC unresolved finding", ValidUntil: now.Add(time.Hour)}, {Key: "host-two", Condition: "ok", Reason: "SYNTHETIC clean scan", ValidUntil: now.Add(time.Hour)}}}
	must(t, s.Replace(ctx, scan))
	scanReport := report(now, "scanner-daily", 1, 1, 1)
	scanReport.Producer, scanReport.Subject = scanner, "host-one"
	_, err = s.Enqueue(ctx, scanReport)
	must(t, err)
	job, ready, err = s.Grant(ctx, scanner, scanReport.ID)
	must(t, err)
	if !ready {
		t.Fatal("independent scanner report blocked")
	}
	must(t, s.Finish(ctx, job, Result{Accepted: true}))
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: time.Second}
	must(t, r.Step(ctx))
	oldUptime, oldScan := bindingFault(t, s, "uptime", "critical"), bindingFault(t, s, "scanner", "critical")
	// Expired monitoring data cannot discharge the prior endpoint failure.
	now = now.Add(16 * time.Second)
	v, err = u.Evaluate(now)
	must(t, err)
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	if _, ok := api.states[oldUptime]; !ok {
		t.Fatal("stale probe cleared failure")
	}
	must(t, u.Observe(uptimefixture.Observation{Subject: "api", Revision: 2, At: now, Until: now.Add(time.Minute), Reachable: true}))
	v, err = u.Evaluate(now)
	must(t, err)
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	h, err := s.Health(ctx)
	must(t, err)
	if h.Bindings["uptime"].Condition != "ok" || h.Bindings["scanner"].Condition != "critical" || h.Condition != "critical" {
		t.Fatal(h)
	}
	if _, ok := api.states[oldUptime]; ok {
		t.Fatal("fresh reachability did not resolve uptime")
	}
	if _, ok := api.states[oldScan]; !ok {
		t.Fatal("uptime resolution cleared scanner")
	}
}
