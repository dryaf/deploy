package install

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInteractiveSupervisionBackoffAndRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	var states []error
	calls := 0
	err := superviseInteractiveLoop(ctx, func(context.Context) error {
		calls++
		if calls <= 7 {
			return errors.New("provider unavailable")
		}
		return nil
	}, func(err error) { states = append(states, err) }, func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		if len(delays) == 8 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 8 {
		t.Fatal("provider failure stopped loop", calls, err)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 1}
	for i, d := range want {
		if delays[i] != d*time.Second {
			t.Fatal("unbounded backoff or missing reset", delays)
		}
	}
	if len(states) != 9 || states[7] != nil || !errors.Is(states[8], context.Canceled) {
		t.Fatal("health observations lost", states)
	}
}

func TestInteractiveSupervisionCancellationBeforeStep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observed := false
	err := runInteractiveLoop(ctx, func(context.Context) error { t.Fatal("cancelled loop performed I/O"); return nil }, func(err error) { observed = errors.Is(err, context.Canceled) })
	if !observed || !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = waitRuntimeLoop(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal("backoff did not cancel", err)
	}
}
