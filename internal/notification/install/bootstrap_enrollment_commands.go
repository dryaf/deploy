package install

import (
	"context"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func (i managedInstaller) requireBootstrapEnrollmentCommandsCapability(ctx context.Context, d *safefs.Directory, m ManagedManifest, required bool) error {
	present, err := worker.BootstrapEnrollmentCommandsRequired(m.Paths.State)
	if err != nil {
		return err
	}
	if !required && !present {
		return nil
	}
	probe := i.bootstrapEnrollmentCommandsProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-bootstrap-enrollment-commands-probe")
		}
	}
	for _, binary := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err = probe(ctx, d, binary.name, binary.hash); err != nil {
			return err
		}
	}
	return nil
}
