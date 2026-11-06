package install

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/worker"
)

// Check installation admission before each daemon request, including the
// observations used to authorize a clear. Daemon failures affect LED health;
// loss of installation authority stops the managed runtime.
type admittedLEDClient struct {
	api   worker.LEDAPI
	check func(context.Context) error
	fail  func(error)
}

func (a admittedLEDClient) admitted(ctx context.Context) error {
	if err := a.check(ctx); err != nil {
		if ctx.Err() == nil {
			a.fail(err)
		}
		return err
	}
	return ctx.Err()
}
func (a admittedLEDClient) Set(ctx context.Context, state led.State) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.admitted(ctx); err != nil {
		return err
	}
	return a.api.Set(ctx, state)
}
func (a admittedLEDClient) Clear(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.admitted(ctx); err != nil {
		return err
	}
	return a.api.Clear(ctx, id)
}
func (a admittedLEDClient) Status(ctx context.Context) (led.DaemonStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.admitted(ctx); err != nil {
		return led.DaemonStatus{}, err
	}
	return a.api.Status(ctx)
}
func (a admittedLEDClient) List(ctx context.Context) ([]led.Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.admitted(ctx); err != nil {
		return nil, err
	}
	return a.api.List(ctx)
}

func managedLEDService(s *worker.Store, socket string, check func(context.Context) error, fail func(error), api worker.LEDAPI) runtimeService {
	if api == nil {
		api = led.Client{Socket: socket, Budget: 2 * time.Second}
	}
	return func(ctx context.Context) error {
		r := worker.LEDReconciler{Store: s, API: admittedLEDClient{api, check, fail}, Budget: 2 * time.Second}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := r.Step(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "LED reconciliation degraded")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}
