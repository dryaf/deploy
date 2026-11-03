package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRuntimeLoopHealthStartupFailureAndExpiry(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	observe, err := s.MonitorRuntimeLoop("signal-intake")
	must(t, err)
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "unknown" || h.Loops["signal-intake"].Condition != "unknown" {
		t.Fatal("unstarted loop claimed health", h)
	}
	observe(nil)
	h, err = s.Health(ctx)
	must(t, err)
	if h.Condition != "ok" || !h.ValidUntil.Equal(now.Add(90*time.Second)) {
		t.Fatal("loop deadline not enforced", h)
	}
	*now = now.Add(91 * time.Second)
	h, err = s.Health(ctx)
	must(t, err)
	if h.Condition != "unknown" {
		t.Fatal("stalled loop stayed healthy", h)
	}
	observe(errors.New("secret-provider-token"))
	h, err = s.Health(ctx)
	must(t, err)
	raw, err := json.Marshal(h)
	must(t, err)
	if h.Condition != "unknown" || strings.Contains(string(raw), "secret-provider-token") {
		t.Fatal("loop failure leaked or claimed health")
	}
	observe(nil)
	h, err = s.Health(ctx)
	must(t, err)
	if h.Condition != "ok" || !h.ValidUntil.Equal(now.Add(59*time.Second)) {
		t.Fatal("loop recovery renewed producer evidence", h)
	}
	if _, err = s.MonitorRuntimeLoop("signal-intake"); err == nil {
		t.Fatal("duplicate loop replaced health")
	}
}

func TestRuntimeLoopFailureKeepsCriticalFault(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	observe, err := s.MonitorRuntimeLoop("command-replies")
	must(t, err)
	observe(errors.New("offline"))
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "critical" {
		t.Fatal("intake failure concealed critical fault", h)
	}
}

func TestAmbiguousCommandReplyDegradesHealth(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	_, err := s.db.Exec("INSERT INTO command_replies(command_id,kind,revision,body,state) VALUES('reply','result',1,'{}','ambiguous')")
	must(t, err)
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "warning" {
		t.Fatal("ambiguous reply disappeared from health", h)
	}
}

func TestRuntimeLoopClockRollbackCannotRenewHealth(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	observe, err := s.MonitorRuntimeLoop("signal-intake")
	must(t, err)
	observe(nil)
	before, err := s.Health(ctx)
	must(t, err)
	*now = now.Add(-5 * time.Second)
	observe(nil)
	after, err := s.Health(ctx)
	must(t, err)
	if after.Condition != "unknown" || after.Loops["signal-intake"].Condition != "unknown" || after.Loops["signal-intake"].Revision <= before.Loops["signal-intake"].Revision {
		t.Fatal("successful poll concealed clock uncertainty", after)
	}
}
