package install

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
)

type runtimeTestSender func(context.Context, string, protocol.Message) worker.Result

func (f runtimeTestSender) Send(ctx context.Context, channel string, message protocol.Message) worker.Result {
	return f(ctx, channel, message)
}

func TestRuntimeGroupIntakeFailureDoesNotBlockDelivery(t *testing.T) {
	c, _ := runtimeFixture(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(dir, c.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := c.Worker.Producers[0].Producer
	for _, err := range []error{s.Stage(p, "one"), s.Activate(p, "one")} {
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if err = s.Replace(ctx, protocol.Snapshot{Producer: p, Sequence: 1, BasisRevision: 1, ConfigRevision: "one", EvaluatedAt: now, Subjects: []protocol.Subject{{Key: "backup", Condition: "ok", Reason: "test evidence", ValidUntil: now.Add(time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	m := protocol.Message{Producer: p, Sequence: 1, ID: "report", Generation: "one", Subject: "backup", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "Fixture report", Route: "ops", ReportKey: "daily", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
	if _, err = s.Enqueue(ctx, m); err != nil {
		t.Fatal(err)
	}
	observe, err := s.MonitorRuntimeLoop("signal-intake")
	if err != nil {
		t.Fatal(err)
	}
	inBackoff := make(chan struct{})
	sent := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runRuntimeGroup(ctx, []runtimeService{
			func(ctx context.Context) error {
				return superviseInteractiveLoop(ctx, func(context.Context) error { return errors.New("offline") }, observe, func(ctx context.Context, _ time.Duration) error {
					close(inBackoff)
					<-ctx.Done()
					return ctx.Err()
				})
			},
			func(ctx context.Context) error {
				return s.RunDispatcher(ctx, runtimeTestSender(func(ctx context.Context, _ string, _ protocol.Message) worker.Result {
					select {
					case <-inBackoff:
					case <-ctx.Done():
						return worker.Result{Ambiguous: true}
					}
					close(sent)
					return worker.Result{Accepted: true}
				}))
			},
		})
	}()
	// Always join the group before closing its database, including test failures.
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-sent:
	case <-ctx.Done():
		t.Fatal("notification did not progress during intake outage")
	}
	for {
		j, err := s.Get(ctx, p, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == "provider_accepted" {
			break
		}
		if err = waitRuntimeLoop(ctx, time.Millisecond); err != nil {
			t.Fatal("acceptance was not durably recorded", err)
		}
	}
	h, err := s.Health(ctx)
	if err != nil || h.Condition != "unknown" || h.Loops["signal-intake"].Condition != "unknown" {
		t.Fatal("outage did not remain visible alongside successful delivery", h, err)
	}
}

func TestRuntimeGroupFatalExitCancelsAndJoinsPeers(t *testing.T) {
	for _, cleanExit := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "unexpected-clean-exit"}[cleanExit], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started, stopped := make(chan struct{}), make(chan struct{})
			failure := errors.New("service failed")
			err := runRuntimeGroup(ctx, []runtimeService{
				func(ctx context.Context) error {
					select {
					case <-started:
					case <-ctx.Done():
						return ctx.Err()
					}
					if cleanExit {
						return nil
					}
					return failure
				},
				func(ctx context.Context) error {
					close(started)
					<-ctx.Done()
					close(stopped)
					return ctx.Err()
				},
			})
			if err == nil || (!cleanExit && !errors.Is(err, failure)) {
				t.Fatal("service exit was lost", err)
			}
			select {
			case <-stopped:
			default:
				t.Fatal("returned before peer shutdown")
			}
		})
	}
}

func TestRuntimeGroupCancelledBeforeStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runRuntimeGroup(ctx, []runtimeService{func(context.Context) error { t.Error("cancelled runtime started I/O"); return nil }})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
