package install

import (
	"context"

	"github.com/dryaf/deploy/internal/notification/worker"
)

func ValidateManagedEnrolledCommandActivity(m ManagedManifest, before, after Runtime) error {
	if err := worker.ValidateEnrolledCommandActivityChange(before.Worker, after.Worker); err != nil {
		return err
	}
	// Both directions retain the same noninteractive alert provider. Existing
	// receivers, delivery ownership, recovery and legacy Signal are excluded by
	// the initial-enrollment validation; the worker proves retained alias policy.
	if after.Worker.Interaction == nil {
		return ValidateManagedEnrolledCommands(m, after, before)
	}
	return ValidateManagedEnrolledCommands(m, before, after)
}

func MigrateEnrolledCommandActivity(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "enrolled-command-activity")
}
