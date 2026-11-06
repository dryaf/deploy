package install

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

const managedRecoveryPreparationFile = "managed-recovery-preparation.json"

type managedRecoveryPreparation struct {
	Version      int                     `json:"version"`
	ManifestHash string                  `json:"manifest_hash"`
	SnapshotPath string                  `json:"snapshot_path"`
	Snapshot     worker.SnapshotManifest `json:"snapshot"`
}

func managedRecoveryPreparationName(m ManagedManifest) string {
	if m.RecoveryCommitHash == "" {
		return managedRecoveryPreparationFile
	}
	return "managed-recovery-preparation-" + m.RecoveryCommitHash + ".json"
}

func refuseManagedRecoveryPreparation(d *safefs.Directory, m ManagedManifest) error {
	f, err := d.Open(managedRecoveryPreparationName(m), os.O_RDONLY, 0)
	if err == nil {
		f.Close()
		return fmt.Errorf("recovery preparation is retained; finish publication and adopt-recovery before other lifecycle changes")
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// prepareManagedRecovery retains the operation and installation leases through
// quarantine. Its intent is immutable; restart resumes only the same snapshot.
func (i managedInstaller) prepareManagedRecovery(ctx context.Context, root, manifestHash, from, snapshotID, databaseHash string, dry bool) (out worker.RecoveryQuarantineView, err error) {
	if !managedPath(root) || !managedPath(from) || !safefs.DigestPattern.MatchString(manifestHash) {
		return out, fmt.Errorf("managed recovery requires exact root, manifest hash and snapshot path")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	operation, err := d.Lock("operation.lock", !dry, false)
	if err != nil {
		return out, err
	}
	defer operation.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return out, err
	}
	if j.Phase != "complete" {
		return out, fmt.Errorf("finish the current lifecycle operation before recovery")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return out, err
	}
	m := j.Intent.After
	if protocol.Hash(m) != manifestHash || m.Mode != "verification" || m.DeliveryOwnerHash == "" {
		return out, fmt.Errorf("managed recovery requires the exact deactivated delivery installation")
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return out, err
	}
	snapshot, err := worker.VerifySnapshot(ctx, from, c.Worker)
	if err != nil {
		return out, err
	}
	if snapshot.ID != snapshotID || snapshot.DatabaseHash != databaseHash || snapshot.Identity != m.Identity {
		return out, fmt.Errorf("managed recovery snapshot pin changed")
	}
	want := managedRecoveryPreparation{1, manifestHash, from, snapshot}
	var got managedRecoveryPreparation
	err = d.ReadJSON(managedRecoveryPreparationName(m), &got)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return out, err
	}
	if !missing && got != want {
		return out, fmt.Errorf("another managed recovery preparation is retained")
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return out, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return out, err
	}
	if !present || managedDigest(unit) != m.UnitHash {
		return out, fmt.Errorf("managed recovery unit changed")
	}
	if err = i.inspectDelivery(ctx, m, false); err != nil {
		return out, err
	}
	if err = i.probeCommandRecovery(ctx, d, m); err != nil {
		return out, err
	}
	if m.RecoveryCommitHash != "" {
		if m.DeliveryRecovery == nil {
			return out, fmt.Errorf("next recovery requires retained delivery ownership")
		}
		if err = i.probeNextRecovery(ctx, d, m); err != nil {
			return out, err
		}
		if m.SignalVerification != nil {
			service, e := inspectSignalService(m.SignalVerification.SignalVerificationReference)
			if e != nil {
				return out, e
			}
			root, e := privateManagedDir(service.Spec.Root, true)
			if e != nil {
				return out, e
			}
			if i.signalProbe != nil {
				e = i.signalProbe(ctx, root, "deploy", service.Spec.BinarySHA256)
			} else {
				e = probeManagedCapability(ctx, root, "deploy", service.Spec.BinarySHA256, "__signal-next-recovery-probe")
			}
			root.Close()
			if e != nil {
				return out, e
			}
		}
	}
	if dry {
		return out, nil
	}
	if missing {
		if err = d.WriteJSON(managedRecoveryPreparationName(m), want, false); err != nil {
			return out, err
		}
	}
	if err = i.boundary("recovery-preparation-intent"); err != nil {
		return out, err
	}
	if err = i.services.Stop(ctx); err != nil {
		return out, err
	}
	state, err := inspectManagedService(ctx, i.services, m, false)
	if err != nil {
		return out, err
	}
	if state.Active != "inactive" && state.Active != "failed" {
		return out, fmt.Errorf("managed recovery service did not stop")
	}
	if err = i.inspectDelivery(ctx, m, false); err != nil {
		return out, err
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	if err = managedLeaseMatches(d, "operation.lock", operation); err != nil {
		return out, err
	}
	if err = managedLeaseMatches(d, "installation.lock", lease); err != nil {
		return out, err
	}
	if err = checkManagedDirectoryPath(d, root, true); err != nil {
		return out, err
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return out, err
	}
	if err = i.boundary("recovery-preparation-stopped"); err != nil {
		return out, err
	}
	if m.DeliveryRecovery != nil {
		return worker.QuarantineNextManagedDeliveryRecovery(ctx, m.Paths.State, from, c.Worker, snapshotID, databaseHash, m.Revision, *m.DeliveryRecovery)
	}
	return worker.QuarantineManagedDeliveryRecovery(ctx, m.Paths.State, from, c.Worker, snapshotID, databaseHash, m.Revision, deliveryOwner(m, c))
}
