package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func validateManagedBootstrapLimits(m ManagedManifest, before, after Runtime) error {
	return validateManagedBootstrapPolicy(m, before, after, "bootstrap-limits")
}
func isManagedPolicyOperation(operation string) bool {
	return operation == "enrolled-command-activity" || operation == "enrolled-command-policy" || operation == "enrolled-commands" || operation == "enrolled-providers" || operation == "enrolled-producer" || operation == "enrolled-routes" || operation == "enrolled-freshness" || operation == "enrolled-limits" || operation == "bootstrap-limits" || operation == "bootstrap-routes" || operation == "bootstrap-providers"
}
func validateManagedBootstrapPolicy(m ManagedManifest, before, after Runtime, operation string) error {
	if operation == "enrolled-command-activity" {
		return ValidateManagedEnrolledCommandActivity(m, before, after)
	}
	if operation == "enrolled-command-policy" {
		return ValidateManagedEnrolledCommandPolicy(m, before, after)
	}
	if operation == "enrolled-commands" {
		return ValidateManagedEnrolledCommands(m, before, after)
	}
	if operation == "enrolled-providers" {
		return ValidateManagedEnrolledProviders(m, before, after)
	}
	if operation == "enrolled-producer" {
		return ValidateManagedEnrolledProducerPolicy(m, before, after)
	}
	if operation == "enrolled-routes" {
		return ValidateManagedEnrolledRoutes(m, before, after)
	}
	if operation == "enrolled-freshness" {
		return ValidateManagedEnrolledFreshness(m, before, after)
	}
	if operation == "enrolled-limits" {
		return ValidateManagedEnrolledLimits(m, before, after)
	}
	if m.LEDOutput != nil && !safefs.ValidID(m.LEDOutput.Operation, "n") {
		return fmt.Errorf("bootstrap policy require an exact completed LED output disposition")
	}
	if m.Mode != "verification" || m.DeliveryOwnerHash != "" || m.RecoveryCommitHash != "" || m.Commands != nil || !before.Bootstrap || !after.Bootstrap {
		return fmt.Errorf("bootstrap policy migration requires an unbound verification gateway")
	}
	if operation == "bootstrap-providers" {
		if m.SignalVerification != nil || m.SignalProfiles != nil || m.CommandHistoryHash != "" {
			return fmt.Errorf("provider migration excludes retained account or command ownership")
		}
		for _, runtime := range []Runtime{before, after} {
			for _, profile := range runtime.Providers {
				if profile.Kind == "signal" {
					if m.FreshSignal == nil {
						return fmt.Errorf("retained Signal profile requires its owned native service")
					}
					continue
				}
				if err := provider.CheckTelegramProfile(profile); err != nil {
					return err
				}
			}
		}
		return ValidateBootstrapProvidersChange(before, after)
	}
	validate := worker.ValidateBootstrapLimitsChange
	if operation == "bootstrap-routes" {
		validate = worker.ValidateBootstrapRoutesChange
	}
	if err := validate(before.Worker, after.Worker); err != nil {
		return err
	}
	want := before
	want.Worker = after.Worker
	if protocol.Hash(want) != protocol.Hash(after) {
		return fmt.Errorf("apply provider, LED and runtime changes separately from bootstrap policy")
	}
	return nil
}

func (i managedInstaller) requireBootstrapLimitsCapability(ctx context.Context, d *safefs.Directory, m ManagedManifest, required bool) error {
	present, err := worker.BootstrapLimitsRequired(m.Paths.State)
	if err != nil {
		return err
	}
	if required || present {
		return i.probeBootstrapLimits(ctx, d, m)
	}
	return nil
}
func managedBootstrapLimitsPair(d *safefs.Directory, j managedJournal, after Runtime) (Runtime, error) {
	if j.Intent.Before == nil {
		return Runtime{}, fmt.Errorf("bootstrap policy migration lacks prior authority")
	}
	before, err := managedRuntime(d, *j.Intent.Before)
	if err != nil {
		return before, err
	}
	return before, validateManagedBootstrapPolicy(*j.Intent.Before, before, after, j.Intent.Operation)
}

func (i managedInstaller) probeBootstrapLimits(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	// All future executables opening a migrated store need this capability,
	// including retained recovery helpers and later compatible artifact updates.
	probe := i.bootstrapLimitsProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-bootstrap-limits-probe")
		}
	}
	for _, binary := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, binary.name, binary.hash); err != nil {
			return err
		}
	}
	return nil
}

// MigrateBootstrapLimits preserves the original reservation and account owners;
// it stops verification, migrates the durable policy, and restarts verification.
func MigrateBootstrapLimits(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapLimits(ctx, spec, expected)
}
func (i managedInstaller) migrateBootstrapLimits(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return i.migrateBootstrapPolicy(ctx, spec, expected, "bootstrap-limits")
}
func MigrateBootstrapRoutes(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "bootstrap-routes")
}
func (i managedInstaller) migrateBootstrapPolicy(ctx context.Context, spec ManagedSpec, expected, operation string) (ManagedManifest, error) {
	if !isManagedPolicyOperation(operation) {
		return ManagedManifest{}, fmt.Errorf("unsupported bootstrap policy operation")
	}

	if !safefs.DigestPattern.MatchString(expected) {
		return ManagedManifest{}, fmt.Errorf("exact prior installation receipt required")
	}
	if err := spec.validate(); err != nil {
		return ManagedManifest{}, err
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		return ManagedManifest{}, err
	}
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		d.Close()
		return ManagedManifest{}, err
	}
	j, err := readManagedJournal(d)
	lock.Close()
	d.Close()
	if err != nil {
		return ManagedManifest{}, err
	}
	if j.Intent.Operation == operation && j.Intent.Before != nil && protocol.Hash(*j.Intent.Before) == expected {
		return i.ensureVerification(ctx, spec)
	}
	return i.prepareExpected(ctx, operation, spec, expected)
}
