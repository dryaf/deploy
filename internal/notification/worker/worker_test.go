package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

var producer = protocol.Producer{ID: "backup", Epoch: "epoch-one"}

func configuration() Config {
	return Config{Producers: []Enrollment{{Producer: producer, Required: true, Routes: []string{"operations"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 100}}, Routes: map[string]Route{"operations": {Revision: "route-one", Primary: "signal", Fallback: "telegram"}}, MaxMessages: 100, MaxPayloadBytes: 1 << 20, MaxDatabaseBytes: 16 << 20, MinFreeBytes: 0}
}
func fixture(t *testing.T) (*Store, *time.Time, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, configuration(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err = s.Stage(producer, "generation-one"); err != nil {
		t.Fatal(err)
	}
	if err = s.Activate(producer, "generation-one"); err != nil {
		t.Fatal(err)
	}
	return s, &now, dir
}
func snapshot(now time.Time, seq, basis uint64) protocol.Snapshot {
	return protocol.Snapshot{Producer: producer, Sequence: seq, BasisRevision: basis, ConfigRevision: "config-one", EvaluatedAt: now, Subjects: []protocol.Subject{{Key: "backup", Condition: "ok", Reason: "current evaluation", ValidUntil: now.Add(time.Hour)}}}
}
func report(now time.Time, id string, seq, rev, basis uint64) protocol.Message {
	return protocol.Message{Producer: producer, Sequence: seq, ID: id, Generation: "generation-one", Subject: "backup", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(5 * time.Minute), Text: "Current backup report", Route: "operations", ReportKey: "daily-2026-10-31", ReportType: "daily", ReportRevision: rev, BasisRevision: basis}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestStatusOrderingAndIdempotence(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 10)))
	m := report(*now, "report-11", 1, 1, 11)
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	_, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if ok {
		t.Fatal("future basis sent")
	}
	j, err := s.Get(ctx, producer, m.ID)
	must(t, err)
	if j.State != "waiting_status" || j.Tries != 0 {
		t.Fatalf("waiting job: %+v", j)
	}
	_, err = s.Enqueue(ctx, m)
	must(t, err)
	must(t, s.Replace(ctx, snapshot(*now, 2, 11)))
	j, ok, err = s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("matching status did not release job")
	}
	must(t, s.Finish(ctx, j, Result{Accepted: true}))
	m.Text = "changed"
	if _, err = s.Enqueue(ctx, m); err == nil {
		t.Fatal("changed immutable message accepted")
	}
	if err = s.Replace(ctx, snapshot(*now, 3, 10)); err == nil {
		t.Fatal("decreasing basis accepted")
	}
}

func TestDelayedReceiptKeepsAcceptanceEvidence(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	m := report(*now, "daily", 1, 1, 1)
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	j, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("not granted")
	}
	*now = now.Add(10 * time.Second)
	accepted := *now
	must(t, s.Finish(ctx, j, Result{Accepted: true}))
	*now = now.Add(10 * time.Minute)
	must(t, s.Replace(ctx, snapshot(*now, 2, 2)))
	c := protocol.Cursor{Identity: s.Identity(), Producer: producer}
	page, err := s.Receipts(ctx, c, 128)
	must(t, err)
	last := page.Receipts[len(page.Receipts)-1]
	if last.Acceptance == nil || !last.Acceptance.Eligible || !last.Acceptance.ObservedAt.Equal(accepted) {
		t.Fatalf("lost historical evidence: %+v", last)
	}
	must(t, s.Ack(ctx, page.Cursor))
	must(t, s.Ack(ctx, page.Cursor))
	bad := page.Cursor
	bad.Identity.Recovery = "old"
	if s.Ack(ctx, bad) == nil {
		t.Fatal("old epoch acknowledged")
	}
	bad = page.Cursor
	bad.Position++
	if s.Ack(ctx, bad) == nil {
		t.Fatal("future cursor acknowledged")
	}
}

func TestCancellationConsumesSequenceWithoutSending(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	m := report(*now, "cancelled", 1, 1, 1)
	must(t, s.Cancel(ctx, producer, m.ID, m.Sequence, protocol.Hash(m)))
	j, err := s.Enqueue(ctx, m)
	must(t, err)
	if j.State != "cancelled" {
		t.Fatal(j.State)
	}
	next := report(*now, "next", 2, 2, 1)
	_, err = s.Enqueue(ctx, next)
	must(t, err)
	if s.Cancel(ctx, producer, "gap", 4, protocol.Hash(next)) == nil {
		t.Fatal("sequence gap accepted")
	}
}

func TestBatchSupersessionPreservesInflightReservations(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	m := report(*now, "batch", 1, 1, 1)
	m.ReportKey = "backlog-group"
	m.ReportType = "backlog"
	m.Incidents = []protocol.Incident{{Subject: "backup", ID: "A", Revision: 1}, {Subject: "backup", ID: "B", Revision: 1}}
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	j, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("batch not granted")
	}
	r := m
	r.ID = "recover-A"
	r.Sequence = 2
	r.Kind = "recovery"
	r.ReportKey = ""
	r.ReportType = ""
	r.ReportRevision = 0
	r.Incidents = []protocol.Incident{{Subject: "backup", ID: "A", Revision: 2}}
	_, err = s.Enqueue(ctx, r)
	must(t, err)
	_, ok, err = s.Grant(ctx, producer, r.ID)
	must(t, err)
	if ok {
		t.Fatal("recovery overtook running batch")
	}
	must(t, s.Finish(ctx, j, Result{Accepted: true}))
	_, ok, err = s.Grant(ctx, producer, r.ID)
	must(t, err)
	if !ok {
		t.Fatal("settled batch retained reservation")
	}
	page, err := s.Receipts(ctx, protocol.Cursor{Identity: s.Identity(), Producer: producer}, 128)
	must(t, err)
	for _, r := range page.Receipts {
		if r.EventID == m.ID && r.Acceptance != nil && r.Acceptance.Eligible {
			t.Fatal("obsolete batch granted coverage")
		}
	}
}

func TestRevokeDrainsAndNeverReactivates(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	m := report(*now, "r", 1, 1, 1)
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	j, _, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if s.Revoke(ctx, producer, m.Generation) == nil {
		t.Fatal("revoked an active attempt")
	}
	must(t, s.Finish(ctx, j, Result{}))
	must(t, s.Revoke(ctx, producer, m.Generation))
	if s.Activate(producer, m.Generation) == nil {
		t.Fatal("revoked generation reactivated")
	}
	*now = now.Add(time.Minute)
	_, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if ok {
		t.Fatal("revoked job sent")
	}
}

func TestRestartDoesNotRenewStatusAndPreservesAmbiguity(t *testing.T) {
	s, now, dir := fixture(t)
	ctx := context.Background()
	snap := snapshot(*now, 1, 1)
	must(t, s.Replace(ctx, snap))
	m := report(*now, "interrupted", 1, 1, 1)
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	_, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("not granted")
	}
	must(t, s.Close())
	reopened, err := Open(dir, configuration(), false)
	must(t, err)
	defer reopened.Close()
	reopened.now = func() time.Time { return *now }
	j, err := reopened.Get(ctx, producer, m.ID)
	must(t, err)
	if j.State != "ambiguous" {
		t.Fatal(j.State)
	}
	must(t, reopened.Replace(ctx, snap))
	h, err := reopened.Health(ctx)
	must(t, err)
	if h.Condition == "ok" {
		t.Fatal("replay renewed startup health")
	}
	_, ok, err = reopened.Grant(ctx, producer, m.ID)
	must(t, err)
	if ok {
		t.Fatal("ambiguous job resent")
	}
}

func TestCriticalSurvivesMissingSubjectAndStaleStatus(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	v = snapshot(*now, 2, 2)
	v.Subjects[0].Key = "other"
	must(t, s.Replace(ctx, v))
	*now = now.Add(5 * time.Minute)
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "critical" {
		t.Fatal(h.Condition)
	}
}

func TestUnixRoundTripAndIdentityFence(t *testing.T) {
	s, _, _ := fixture(t)
	socket := filepath.Join(t.TempDir(), "api.sock")
	must(t, os.Chmod(filepath.Dir(socket), 0700))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, socket) }()
	for i := 0; i < 100; i++ {
		select {
		case err := <-done:
			t.Fatalf("server failed: %v", err)
		default:
		}
		c, err := net.DialTimeout("unix", socket, 10*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	c := client.Client{Socket: socket, Identity: s.Identity()}
	var ping struct {
		Alive bool `json:"alive"`
	}
	_, err := c.Call(ctx, protocol.Request{Operation: "system.ping"}, &ping)
	must(t, err)
	if !ping.Alive {
		t.Fatal("not alive")
	}
	c.Identity.Recovery = "old"
	_, err = c.Call(ctx, protocol.Request{Operation: "system.status"}, nil)
	if err == nil {
		t.Fatal("changed identity accepted")
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStateMissingNeverInitializes(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, configuration(), false); err == nil {
		t.Fatal("missing database initialized")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); !os.IsNotExist(err) {
		t.Fatal("database created")
	}
}

type senderFunc func(context.Context, string, protocol.Message) Result

func (f senderFunc) Send(ctx context.Context, p string, m protocol.Message) Result {
	return f(ctx, p, m)
}

func TestWaitingPrefixCannotStarveLaterIncident(t *testing.T) {
	s, now, _ := fixture(t)
	s.config.MaxMessages = 200
	s.config.Producers[0].MaxMessages = 200
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	for i := 1; i <= 129; i++ {
		m := report(*now, fmt.Sprintf("waiting-%d", i), uint64(i), 1, 2)
		m.ReportKey = m.ID
		_, err := s.Enqueue(ctx, m)
		must(t, err)
	}
	m := report(*now, "urgent", 130, 1, 1)
	m.Kind = "failure"
	m.Severity = "critical"
	m.ReportKey = ""
	m.ReportType = ""
	m.ReportRevision = 0
	m.Incidents = []protocol.Incident{{Subject: "backup", ID: "urgent", Revision: 1}}
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	count := 0
	send := senderFunc(func(_ context.Context, _ string, got protocol.Message) Result {
		count++
		if got.ID != "urgent" {
			t.Fatal("sent a future-basis report")
		}
		return Result{Accepted: true}
	})
	_, err = s.DispatchOne(ctx, producer, send)
	must(t, err)
	_, err = s.DispatchOne(ctx, producer, send)
	must(t, err)
	if count != 1 {
		t.Fatal("ready event starved behind waiting prefix")
	}
}
