package install

import (
	"context"

	"github.com/dryaf/deploy/internal/safefs"
)

// A helper upgrade is explicit: ordinary executable updates and rollbacks keep
// the retained helper. The new helper must understand its own durable intent.
func (i managedInstaller) probeHelperUpgrade(ctx context.Context, d *safefs.Directory, m ManagedManifest, c Runtime) error {
	probe := i.helperProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-helper-upgrade-probe")
		}
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	if err := probe(ctx, d, managedHelperName(m), m.HelperHash); err != nil {
		return err
	}
	if c.Telegram != nil {
		// Probe future renewal support before adopting either executable, even
		// when the first renewal has not yet assigned its audit pin.
		return i.requireTelegramRenewalCapability(ctx, d, m)
	}
	return nil
}
