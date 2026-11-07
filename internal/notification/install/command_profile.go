package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// InspectManagedCommandPolicy returns the effective specialist policy, including
// retained permission reductions, without
// granting intake, opening the live database or resuming a pending operation.
// The caller retains its exact manifest pin separately from this policy.
func InspectManagedCommandPolicy(ctx context.Context, root, manifestHash string, producer protocol.Producer) (interaction.Config, error) {
	var zero interaction.Config
	if !producer.Valid() {
		return zero, fmt.Errorf("command policy requires an exact producer")
	}
	err := withManagedCommandPolicy(ctx, root, manifestHash, func(_ *safefs.Directory, m ManagedManifest, c Runtime) error {
		policy := c.Worker.Interaction
		if m.CommandHistoryHash != "" {
			var err error
			policy, err = worker.InspectManagedCommandPolicyFile(m.Paths.State, policy, m.CommandHistoryHash)
			if err != nil {
				return err
			}
		}
		for _, w := range policy.Workers {
			if w.Producer == producer {
				zero = *policy
				return nil
			}
		}
		return fmt.Errorf("command policy does not enroll the requested producer")
	})
	return zero, err
}

// ReviewCommandNarrowing validates a permission reduction while intake is
// disabled. Its exact request must be retained before any coordinator effects.
func ReviewCommandNarrowing(ctx context.Context, root, manifestHash, policy string) (CommandPolicyRequest, error) {
	return reviewCommandPolicy(ctx, root, manifestHash, "narrow-commands", policy)
}

// ReviewCommandEnrollment checks a complete narrowed enrollment while intake is
// disabled. It cannot add rights, redirect audiences or replace worker contracts.
func ReviewCommandEnrollment(ctx context.Context, root, manifestHash, policy string) (CommandPolicyRequest, error) {
	return reviewCommandPolicy(ctx, root, manifestHash, "migrate-commands", policy)
}

func reviewCommandPolicy(ctx context.Context, root, manifestHash, operation, policy string) (CommandPolicyRequest, error) {
	r := CommandPolicyRequest{Root: root, ManifestHash: manifestHash, Operation: operation, Policy: policy}
	if err := r.validate(); err != nil {
		return CommandPolicyRequest{}, err
	}
	err := withManagedCommandPolicy(ctx, root, manifestHash, func(_ *safefs.Directory, m ManagedManifest, c Runtime) error {
		if !disabledManagedCommandPolicy(m) {
			return fmt.Errorf("disable commands before changing permissions")
		}
		if operation == "restore-command-permissions" {
			return worker.ValidateManagedCommandGrantFile(m.Paths.State, c.Worker.Interaction, m.CommandHistoryHash, policy)
		}
		return worker.ValidateManagedCommandPolicyFile(m.Paths.State, c.Worker.Interaction, m.CommandHistoryHash, policy)
	})
	if err != nil {
		return CommandPolicyRequest{}, err
	}
	return r, nil
}

func withManagedCommandPolicy(ctx context.Context, root, manifestHash string, review func(*safefs.Directory, ManagedManifest, Runtime) error) error {
	if !managedPath(root) || !safefs.DigestPattern.MatchString(manifestHash) {
		return fmt.Errorf("command policy inspection requires exact gateway pins")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	op, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return err
	}
	defer op.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return err
	}
	m := j.Intent.After
	if j.Phase != "complete" || m.Root != root || protocol.Hash(m) != manifestHash || m.Mode != "delivery" || m.DeliveryActivation == nil {
		return fmt.Errorf("command policy requires unchanged completed delivery")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return err
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return err
	}
	if _, _, err = readDeliveryAdmission(d, m, c); err != nil {
		return err
	}
	if m.Commands != nil {
		if _, err = readManagedCommandAdmission(d, m, c); err != nil {
			return err
		}
	}
	if c.Worker.Interaction == nil || c.Worker.Interaction.Validate() != nil || c.Telegram == nil && c.Signal == nil {
		return fmt.Errorf("command policy and provider intake are not enrolled")
	}
	return review(d, m, c)
}

// ReviewCommandPermissionRestore checks an explicit restoration within the
// originally admitted receiver authority. Intake remains disabled.
func ReviewCommandPermissionRestore(ctx context.Context, root, manifestHash, policy string) (CommandPolicyRequest, error) {
	return reviewCommandPolicy(ctx, root, manifestHash, "restore-command-permissions", policy)
}
