package install

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// ValidateManagedEnrolledLimits preserves the verification installation and its
// enrolled producers, unchanged LED bindings and completed output disposition.
// Activated or recovered gateways require separate ownership handoffs.
func ValidateManagedEnrolledLimits(m ManagedManifest, before, after Runtime) error {
	return validateManagedEnrolledPolicy(m, before, after, "")
}
func validateManagedEnrolledPolicy(m ManagedManifest, before, after Runtime, kind string) error {
	if m.Mode != "verification" || m.RecoveryCommitHash != "" || m.DeliveryOwnerHash != "" || m.Commands != nil || m.CommandHistoryHash != "" || before.Bootstrap || after.Bootstrap || m.SignalVerification != nil || m.SignalProfiles != nil {
		return fmt.Errorf("shared policy changes require unrecovered verification without admitted command or legacy Signal authority")
	}
	validate := worker.ValidateEnrolledLimitsChange
	if kind == "producer" {
		validate = worker.ValidateEnrolledProducerPolicyChange
	}
	if kind == "routes" {
		validate = worker.ValidateEnrolledRoutesChange
	}
	if kind == "freshness" {
		validate = worker.ValidateEnrolledFreshnessChange
	}
	if err := validate(before.Worker, after.Worker); err != nil {
		return err
	}
	want := before
	want.Worker = after.Worker
	if protocol.Hash(want) != protocol.Hash(after) {
		return fmt.Errorf("change shared policy separately from providers and runtime policy")
	}
	return nil
}
func MigrateEnrolledLimits(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "enrolled-limits")
}
func (i managedInstaller) requireEnrolledLimitsCapability(ctx context.Context, d *safefs.Directory, m ManagedManifest, required bool) error {
	if m.DeliveryOwnerConfigHash != "" {
		kind := "limits"
		if m.CommandHistoryHash != "" {
			kind = "command-limits"
		}
		if err := i.probePausedRuntimeChange(ctx, d, m, kind); err != nil {
			return err
		}
	}
	present, err := worker.EnrolledLimitsRequired(m.Paths.State)
	if err != nil {
		return err
	}
	if !required && !present {
		return nil
	}
	probe := i.enrolledLimitsProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-enrolled-limits-probe")
		}
	}
	for _, b := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err = probe(ctx, d, b.name, b.hash); err != nil {
			return err
		}
	}
	return nil
}

func ValidateManagedEnrolledFreshness(m ManagedManifest, before, after Runtime) error {
	return validateManagedEnrolledPolicy(m, before, after, "freshness")
}
func MigrateEnrolledFreshness(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "enrolled-freshness")
}

func ValidateManagedEnrolledRoutes(m ManagedManifest, before, after Runtime) error {
	return validateManagedEnrolledPolicy(m, before, after, "routes")
}
func MigrateEnrolledRoutes(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "enrolled-routes")
}

func ValidateManagedEnrolledProducerPolicy(m ManagedManifest, before, after Runtime) error {
	return validateManagedEnrolledPolicy(m, before, after, "producer")
}
func MigrateEnrolledProducerPolicy(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().migrateBootstrapPolicy(ctx, spec, expected, "enrolled-producer")
}
