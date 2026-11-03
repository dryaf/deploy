package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
)

type runtimeService func(context.Context) error

func runRuntimeGroup(ctx context.Context, services []runtimeService) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(services) == 0 {
		return fmt.Errorf("notification runtime has no services")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(services))
	for _, service := range services {
		go func(run runtimeService) {
			err := run(ctx)
			if err == nil && ctx.Err() == nil {
				err = fmt.Errorf("notification runtime service stopped unexpectedly")
			}
			results <- err
		}(service)
	}
	err := <-results
	cancel()
	for i := 1; i < len(services); i++ {
		<-results
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// serveRuntime registers health observations before exposing the API and joins
// every service before the caller can close the store.
func serveRuntime(ctx context.Context, s *worker.Store, c Runtime, socket string) error {
	monitors := map[string]func(error){}
	var names []string
	if c.Telegram != nil && !s.VerificationOnly() {
		names = append(names, "telegram-intake")
	}
	if c.Signal != nil && !s.VerificationOnly() {
		names = append(names, "signal-intake")
	}
	if (c.Telegram != nil || c.Signal != nil) && !s.VerificationOnly() {
		names = append(names, "command-conversion", "command-replies")
	}
	for _, name := range names {
		observe, e := s.MonitorRuntimeLoop(name)
		if e != nil {
			return e
		}
		monitors[name] = observe
	}
	services := []runtimeService{
		func(ctx context.Context) error { return s.Serve(ctx, socket) },
	}
	if !s.VerificationOnly() {
		services = append(services, func(ctx context.Context) error { return s.RunDispatcher(ctx, provider.Adapter{Profiles: c.Providers}) })
	}
	if c.Telegram != nil && !s.VerificationOnly() {
		services = append(services, func(ctx context.Context) error {
			poller := provider.TelegramPoller{Profile: *c.Telegram, Destinations: c.Worker.Interaction.Destinations}
			return runInteractiveLoop(ctx, func(ctx context.Context) error { return s.PullTelegram(ctx, poller) }, monitors["telegram-intake"])
		})
	}
	if c.Signal != nil && !s.VerificationOnly() {
		services = append(services, func(ctx context.Context) error {
			client := provider.SignalInboxClient{Profile: *c.Signal}
			return runInteractiveLoop(ctx, func(ctx context.Context) error { return s.PullSignal(ctx, client) }, monitors["signal-intake"])
		})
	}
	if (c.Telegram != nil || c.Signal != nil) && !s.VerificationOnly() {
		services = append(services, func(ctx context.Context) error {
			return runInteractiveLoop(ctx, s.ConvertIntake, monitors["command-conversion"])
		})
		services = append(services, func(ctx context.Context) error {
			adapter := provider.ConversationAdapter{Signal: c.Signal}
			if c.Telegram != nil {
				adapter.TelegramAccount = c.Telegram.Account
				adapter.TelegramTokenFile = c.Telegram.TokenFile
			}
			return runInteractiveLoop(ctx, func(ctx context.Context) error { return s.DispatchCommandReplies(ctx, adapter) }, monitors["command-replies"])
		})
	}
	if c.LEDSocket != "" {
		services = append(services, func(ctx context.Context) error {
			r := worker.LEDReconciler{Store: s, API: led.Client{Socket: c.LEDSocket, Budget: 2 * time.Second}, Budget: 2 * time.Second}
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
		})
	}
	return runRuntimeGroup(ctx, services)
}
