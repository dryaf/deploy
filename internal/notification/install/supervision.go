package install

import (
	"context"
	"fmt"
	"os"
	"time"
)

func waitRuntimeLoop(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runInteractiveLoop(ctx context.Context, step func(context.Context) error, observe func(error)) error {
	return superviseInteractiveLoop(ctx, step, observe, waitRuntimeLoop)
}

// Each step must preserve its durable cursor/attempt before returning. Retrying
// the step never resets those records or grants a new identity to old input.
func superviseInteractiveLoop(ctx context.Context, step func(context.Context) error, observe func(error), wait func(context.Context, time.Duration) error) error {
	delay := time.Second
	degraded := false
	for {
		if err := ctx.Err(); err != nil {
			observe(err)
			return err
		}
		err := step(ctx)
		if ctx.Err() != nil {
			observe(ctx.Err())
			return ctx.Err()
		}
		observe(err)
		if err != nil {
			if !degraded {
				fmt.Fprintln(os.Stderr, "Interactive notification loop degraded; retrying")
			}
			degraded = true
		} else {
			if degraded {
				fmt.Fprintln(os.Stderr, "Interactive notification loop recovered")
			}
			degraded = false
			delay = time.Second
		}
		if e := wait(ctx, delay); e != nil {
			observe(e)
			return e
		}
		if err != nil {
			delay = min(delay*2, 30*time.Second)
		}
	}
}
