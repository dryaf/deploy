package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func managedDeliveryRecoveryFixture(t *testing.T) (ManagedSpec, managedInstaller, Runtime, ManagedManifest, QualificationRecord) {
	t.Helper()
	spec, i, f, c, before, record := deliveryFixture(t)
	ctx := context.Background()
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	inactive, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(active), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	result := publishManagedRecovery(t, spec)
	spec.RecoveryCommitHash = result.Commit.CommitHash
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.recoveryProbe, i.deliveryRecoveryProbe = nop, nop
	return spec, i, c, inactive, record
}

func freshDeliveryQualification(t *testing.T, root string) QualificationRecord {
	t.Helper()
	var out bytes.Buffer
	if err := runQualificationCLI([]string{"qualification-template", "--local", "--root", root}, &out); err != nil {
		t.Fatal(err)
	}
	var report QualificationReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	qualificationPass(&report)
	status, err := qualifyFixture(t, root, report)
	if err != nil {
		t.Fatal(err)
	}
	return *status.Record
}

func TestManagedRecoveredDeliveryLifecycle(t *testing.T) {
	spec, i, c, before, oldQualification := managedDeliveryRecoveryFixture(t)
	ctx := context.Background()
	m, err := i.prepare(ctx, "adopt-recovery", spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.DeliveryRecovery == nil || m.DeliveryOwnerHash != before.DeliveryOwnerHash || m.DeliveryRecovery.Owner.Identity != before.Identity || m.Mode != "verification" || m.Identity == before.Identity {
		t.Fatal("recovery lost original owner or activated", m)
	}
	if _, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(oldQualification), false); err == nil {
		t.Fatal("old qualification admitted recovered runtime")
	}
	record := freshDeliveryQualification(t, spec.Root)
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openManagedStore(active, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.VerificationOnly() {
		t.Fatal("activation stayed in verification")
	}
	if err = s.RequireInactiveProducers(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := i.resume(ctx, spec.Root)
	if err != nil || protocol.Hash(again) != protocol.Hash(active) {
		t.Fatal(err, again)
	}
	inactive, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(active), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(inactive.DeliveryRecovery) != protocol.Hash(m.DeliveryRecovery) {
		t.Fatal("deactivation lost lineage")
	}
	spec.RecoveryCommitHash = ""
	updated, err := i.prepare(ctx, "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(updated.DeliveryRecovery) != protocol.Hash(m.DeliveryRecovery) {
		t.Fatal("update lost lineage")
	}
	if _, _, _, err = reviewManagedRollback(spec.Root, before.Revision); err == nil {
		t.Fatal("rollback crossed recovery")
	}
	if _, err = os.Stat(filepath.Join(spec.State, "recovery.json")); err != nil {
		t.Fatal("quarantine erased", err)
	}
}

func TestManagedRecoveredDeliveryAdoptionInterrupted(t *testing.T) {
	for _, point := range []string{"delivery-recovery-enrolled", "manifest-published"} {
		t.Run(point, func(t *testing.T) {
			spec, i, c, _, _ := managedDeliveryRecoveryFixture(t)
			stop := errors.New("interrupted")
			i.fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if _, err := i.prepare(context.Background(), "adopt-recovery", spec); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			s, err := openManagedStore(m, c)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if !s.VerificationOnly() {
				t.Fatal("resumed adoption activated delivery")
			}
		})
	}
}

func TestManagedRecoveredDeliveryRequiresCapableBinaries(t *testing.T) {
	spec, i, _, _, _ := managedDeliveryRecoveryFixture(t)
	i.deliveryRecoveryProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old executable") }
	if _, err := i.prepare(context.Background(), "adopt-recovery", spec); err == nil {
		t.Fatal("old helper accepted recovery owner")
	}
	var m ManagedManifest
	b, err := os.ReadFile(filepath.Join(spec.Root, "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.DeliveryRecovery != nil {
		t.Fatal("failed probe published new authority")
	}
}

func TestManagedRecoveryPreparationAndSnapshot(t *testing.T) {
	for _, point := range []string{"recovery-preparation-intent", "recovery-preparation-stopped"} {
		t.Run(point, func(t *testing.T) {
			spec, i, f, _, before, record := deliveryFixture(t)
			ctx := context.Background()
			active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
			if err != nil {
				t.Fatal(err)
			}
			m, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(active), "", false)
			if err != nil {
				t.Fatal(err)
			}
			artifact := filepath.Join(t.TempDir(), "snapshot")
			if _, err = i.managedSnapshot(ctx, spec.Root, protocol.Hash(m), artifact, false); err == nil {
				t.Fatal("live service snapshotted")
			}
			if err = f.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			snapshot, err := i.managedSnapshot(ctx, spec.Root, protocol.Hash(m), artifact, false)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.EnableStart(ctx); err != nil {
				t.Fatal(err)
			}
			stops := f.stops
			if _, err = i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, true); err != nil {
				t.Fatal(err)
			}
			if stops != f.stops {
				t.Fatal("dry-run stopped service")
			}
			if _, err = os.Stat(filepath.Join(spec.Root, managedRecoveryPreparationFile)); !os.IsNotExist(err) {
				t.Fatal("dry-run wrote intent", err)
			}
			stop := errors.New("interrupted")
			i.fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if _, err = i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			if _, err = i.prepare(ctx, "update", spec); err == nil {
				t.Fatal("update bypassed pending recovery")
			}
			fresh := freshDeliveryQualification(t, spec.Root)
			if _, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(fresh), false); err == nil {
				t.Fatal("activation bypassed pending recovery")
			}
			q, err := i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false)
			if err != nil {
				t.Fatal(err)
			}
			again, err := i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false)
			if err != nil || again != q {
				t.Fatal("quarantine retry changed", err)
			}
			if q.Before != m.Identity || q.After == m.Identity || q.DeliveryEnabled {
				t.Fatal(q)
			}
			owner, err := worker.InspectManagedDeliveryOwner(spec.State)
			if err != nil || protocol.Hash(owner) != m.DeliveryOwnerHash {
				t.Fatal("quarantine lost permanent owner", err)
			}
		})
	}
}

func TestManagedRecoveredDeliveryInitiallyUnenrolled(t *testing.T) {
	spec, i, f, _, _, _ := deliveryFixture(t)
	ctx := context.Background()
	if err := f.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	result := publishManagedRecovery(t, spec)
	spec.RecoveryCommitHash = result.Commit.CommitHash
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.recoveryProbe, i.deliveryRecoveryProbe = nop, nop
	m, err := i.prepare(ctx, "adopt-recovery", spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.DeliveryOwnerHash != "" || m.DeliveryRecovery != nil {
		t.Fatal("adoption enrolled delivery implicitly")
	}
	record := freshDeliveryQualification(t, spec.Root)
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	if active.DeliveryRecovery == nil || active.DeliveryRecovery.Owner.Identity != m.Identity {
		t.Fatal("new enrollment claimed historical ownership")
	}
	if _, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(active), "", false); err != nil {
		t.Fatal(err)
	}
}
