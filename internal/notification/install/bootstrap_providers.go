package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// ValidateBootstrapProvidersChange changes direct Telegram before producer or
// delivery admission. An unchanged direct Signal profile permits adding/removing unused Telegram;
// its native service reference is verified by the managed installation lifecycle.
// Account and command audience changes require their separate lifecycles.
func ValidateBootstrapProvidersChange(before, after Runtime) error {
	for _, c := range []Runtime{before, after} {
		if !c.Bootstrap || c.Telegram != nil || c.Signal != nil || c.Worker.Interaction != nil || (len(c.Providers) != 1 && len(c.Providers) != 2) {
			return fmt.Errorf("provider migration requires an unbound direct-alert gateway")
		}
		telegrams, signals := 0, 0
		for name, p := range c.Providers {
			if p.Conversation != nil {
				return fmt.Errorf("provider migration excludes command profiles")
			}
			switch p.Kind {
			case "telegram":
				telegrams++
				if prior, ok := before.Providers[name]; ok && prior.Kind != "telegram" {
					return fmt.Errorf("provider migration cannot replace a profile kind")
				}
				if next, ok := after.Providers[name]; ok && next.Kind != "telegram" {
					return fmt.Errorf("provider migration cannot replace a profile kind")
				}
			case "signal":
				signals++
				if protocol.Hash(before.Providers[name]) != protocol.Hash(after.Providers[name]) {
					return fmt.Errorf("provider migration must retain the exact Signal profile")
				}
			default:
				return fmt.Errorf("unsupported provider migration profile")
			}
		}
		if telegrams > 1 || signals > 1 || telegrams == 0 && signals != 1 {
			return fmt.Errorf("provider migration requires direct Telegram or retained Signal")
		}
	}
	for name, p := range before.Providers {
		if p.Kind != "telegram" {
			continue
		}
		for nextName, next := range after.Providers {
			if next.Kind == "telegram" && name != nextName {
				return fmt.Errorf("provider migration cannot rename a Telegram profile")
			}
		}
	}
	if err := worker.ValidateBootstrapRoutesChange(before.Worker, after.Worker); err != nil {
		return err
	}
	if len(before.Worker.Routes) != len(after.Worker.Routes) {
		return fmt.Errorf("apply route policy separately from provider changes")
	}
	for name, route := range before.Worker.Routes {
		next, ok := after.Worker.Routes[name]
		if !ok || route.Primary != next.Primary || route.Fallback != next.Fallback {
			return fmt.Errorf("provider migration changed route selection")
		}
	}
	want := before
	want.Providers, want.Worker.Routes = after.Providers, after.Worker.Routes
	if protocol.Hash(want) != protocol.Hash(after) || protocol.Hash(before) == protocol.Hash(after) {
		return fmt.Errorf("provider migration requires only a changed Telegram profile")
	}
	return nil
}

func MigrateBootstrapProviders(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "bootstrap-providers")
}

func (i managedInstaller) requireBootstrapProvidersCapability(ctx context.Context, d *safefs.Directory, m ManagedManifest, operation string) error {
	if operation != "bootstrap-providers" {
		return nil
	}
	probe := i.bootstrapProvidersProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-bootstrap-providers-probe")
		}
	}
	for _, binary := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, binary.name, binary.hash); err != nil {
			return err
		}
	}
	return nil
}
