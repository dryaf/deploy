package install

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
)

// ManagedSignalMigrationSession proves that legacy notifications use the same
// adopted account and route as the admitted, single-owner Signal service.
type ManagedSignalMigrationSession interface {
	SignalMigrationProfile(context.Context, string, string, string, string, string) (provider.Profile, error)
}

func (s *managedProducerSession) SignalMigrationProfile(ctx context.Context, accountState, bootstrap, selectorHash, primary, fallback string) (out provider.Profile, err error) {
	if err = s.check(); err != nil {
		return
	}
	service, err := signalDeliveryService(s.manifest, s.runtime)
	if err != nil {
		return out, err
	}
	if service.Endpoint.Spec.AccountState != accountState {
		return out, fmt.Errorf("Signal migration account storage changed")
	}
	selector, err := provider.ReadSecret(filepath.Join(service.Spec.Root, "account"))
	if err != nil {
		return out, err
	}
	if protocol.Hash(selector) != selectorHash {
		return out, fmt.Errorf("Signal migration changed CLI account selector")
	}
	if err = relay.RequireLegacyRuntimeAncestor(ctx, service.Endpoint.Installation, bootstrap); err != nil {
		return out, err
	}
	d, err := privateManagedDir(s.manifest.Root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	c, err := managedEffectiveRuntime(d, s.manifest, s.runtime)
	if err != nil {
		return out, err
	}
	route, ok := c.Worker.Routes[s.request.Route]
	if !ok || c.Providers[route.Primary].Kind != primary || (route.Fallback == "") != (fallback == "") || fallback != "" && c.Providers[route.Fallback].Kind != fallback {
		return out, fmt.Errorf("Signal migration changed provider order")
	}
	for _, name := range []string{route.Primary, route.Fallback} {
		if p := c.Providers[name]; p.Kind == "signal" {
			if p.Conversation == nil || p.Conversation.Account != c.Signal.Account || p.Socket != c.Signal.Socket || p.RelayIdentity != c.Signal.InboxIdentity {
				return out, fmt.Errorf("Signal migration requires an admitted conversation profile")
			}
			return p, nil
		}
	}
	return out, fmt.Errorf("Signal migration route has no Signal profile")
}
