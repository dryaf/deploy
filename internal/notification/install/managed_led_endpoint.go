package install

import (
	"context"

	"github.com/dryaf/deploy/internal/safefs"
)

func (i managedInstaller) probeLEDEndpoint(ctx context.Context, d *safefs.Directory, m ManagedManifest, c Runtime) error {
	if c.LEDEndpoint == nil {
		return nil
	}
	probe := i.ledEndpointProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-led-endpoint-probe")
		}
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	return probe(ctx, d, managedHelperName(m), m.HelperHash)
}
