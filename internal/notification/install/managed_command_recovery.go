package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func commandRecoveryBinding(before, after ManagedManifest) (worker.ManagedCommandRecovery, error) {
	r := worker.ManagedCommandRecovery{Version: 1, Before: before.Identity, After: after.Identity, CommitHash: after.RecoveryCommitHash, PreviousHash: before.CommandHistoryHash, OwnerHash: before.DeliveryOwnerHash}
	if err := r.Validate(); err != nil {
		return r, err
	}
	if before.Mode != "verification" || after.Mode != "verification" || before.Commands != nil || after.Commands != nil || after.DeliveryRecovery == nil || after.DeliveryOwnerHash != r.OwnerHash {
		return r, fmt.Errorf("command recovery requires deactivated retained delivery ownership")
	}
	return r, nil
}

func (i managedInstaller) probeCommandRecovery(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	if m.CommandHistoryHash == "" {
		return nil
	}
	probe := i.commandRecoveryProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-command-recovery-probe")
		}
	}
	for _, p := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, p.name, p.hash); err != nil {
			return err
		}
	}
	return nil
}

func (i managedInstaller) adoptCommandRecovery(ctx context.Context, d *safefs.Directory, j managedJournal, s *worker.Store) error {
	if j.Intent.Before.CommandHistoryHash == "" {
		return nil
	}
	r, err := commandRecoveryBinding(*j.Intent.Before, j.Intent.After)
	if err != nil {
		return err
	}
	if protocol.Hash(r) != j.Intent.After.CommandHistoryHash {
		return fmt.Errorf("command recovery installation pin changed")
	}
	if err = i.probeCommandRecovery(ctx, d, j.Intent.After); err != nil {
		return err
	}
	if managedPhase(j.Phase) >= managedPhase("stopped") {
		return s.RequireManagedCommandHistory(protocol.Hash(r))
	}
	if err = s.EnrollManagedCommandRecovery(r, *j.Intent.After.DeliveryRecovery); err != nil {
		return err
	}
	return i.boundary("command-recovery-enrolled")
}
