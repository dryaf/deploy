package install

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// Validate the credential bytes actually placed in the provider URL against the
// qualified input. A changed file cannot authorize a request built with another
// token, even if the on-disk credential is restored before this check.
type admittedTelegramTransport struct {
	check                func(context.Context) error
	fail                 func(error)
	tokenFile, tokenHash string
	next                 http.RoundTripper
}

func (t admittedTelegramTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	refuse := func(err error) (*http.Response, error) { t.fail(err); return nil, err }
	if err := t.check(r.Context()); err != nil {
		return refuse(err)
	}
	b, err := readManagedInput(t.tokenFile, 8192)
	if err != nil || managedDigest(b) != t.tokenHash {
		return refuse(fmt.Errorf("qualified Telegram credential changed"))
	}
	token := strings.TrimSpace(string(b))
	if token == "" || r.URL.Scheme != "https" || r.URL.Host != "api.telegram.org" || !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
		return refuse(fmt.Errorf("Telegram request credential does not match qualification"))
	}
	next := t.next
	if next == nil {
		return refuse(fmt.Errorf("managed Telegram transport unavailable"))
	}
	return next.RoundTrip(r)
}

func managedCommandServices(d *safefs.Directory, s *worker.Store, c Runtime, m ManagedManifest, check func(context.Context) error, fail func(error), transport http.RoundTripper) ([]runtimeService, error) {
	if m.Commands == nil {
		return nil, nil
	}
	if _, err := readManagedCommandAdmission(d, m, c); err != nil {
		return nil, err
	}
	proof, _, err := readDeliveryAdmission(d, m, c)
	if err != nil {
		return nil, err
	}
	var services []runtimeService
	if transport == nil && c.Telegram != nil {
		t := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10}
		transport = t
		services = append(services, func(ctx context.Context) error { <-ctx.Done(); t.CloseIdleConnections(); return ctx.Err() })
	}
	var guarded http.RoundTripper
	if c.Telegram != nil {
		guarded = admittedTelegramTransport{check: check, fail: fail, tokenFile: c.Telegram.TokenFile, tokenHash: proof.Qualification.Report.Binding.Inputs["interactive/telegram/token"], next: transport}
	}
	var signalTransport http.RoundTripper
	if c.Signal != nil {
		var closeTransport func()
		signalTransport, closeTransport, err = managedSignalTransport(m, check, fail)
		if err != nil {
			return nil, err
		}
		services = append(services, func(ctx context.Context) error { <-ctx.Done(); closeTransport(); return ctx.Err() })
	}
	add := func(name string, step func(context.Context) error) error {
		observe, err := s.MonitorRuntimeLoop(name)
		if err != nil {
			return err
		}
		services = append(services, func(ctx context.Context) error {
			return runInteractiveLoop(ctx, func(ctx context.Context) error {
				if err := check(ctx); err != nil {
					fail(err)
					return err
				}
				return step(ctx)
			}, observe)
		})
		return nil
	}
	if m.Commands.Mode != "drain" {
		if c.Telegram != nil {
			poller := provider.TelegramPoller{Profile: *c.Telegram, Destinations: s.CommandDestinations(), Transport: guarded}
			if err = add("telegram-intake", func(ctx context.Context) error { return s.PullTelegram(ctx, poller) }); err != nil {
				return nil, err
			}
		}
		if c.Signal != nil {
			client := provider.SignalInboxClient{Profile: *c.Signal, Transport: signalTransport}
			if err = add("signal-intake", func(ctx context.Context) error { return s.PullSignal(ctx, client) }); err != nil {
				return nil, err
			}
		}
		if err = add("command-conversion", s.ConvertIntake); err != nil {
			return nil, err
		}
	}
	adapter := provider.ConversationAdapter{Signal: c.Signal, SignalTransport: signalTransport, Transport: guarded}
	if c.Telegram != nil {
		adapter.TelegramAccount, adapter.TelegramTokenFile = c.Telegram.Account, c.Telegram.TokenFile
	}
	if err = add("command-replies", func(ctx context.Context) error { return s.DispatchCommandReplies(ctx, adapter) }); err != nil {
		return nil, err
	}
	return services, nil
}
