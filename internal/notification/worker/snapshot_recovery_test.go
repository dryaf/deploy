package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestRecoveredSnapshotRetainsPublicationAndQuarantine(t *testing.T) {
	ctx := context.Background()
	path, c, revision, recovery := recoveredDeliveryFixture(t, false, true)
	s, err := OpenManagedRecoveryVerification(path, c, recovery.After, revision, recovery.CommitHash)
	must(t, err)
	defer s.Close()
	destination := filepath.Join(t.TempDir(), "snapshot")
	m, err := s.Snapshot(ctx, destination)
	must(t, err)
	if m.Version != 2 || m.RecoveryHash == "" || m.Identity != recovery.After {
		t.Fatal(m)
	}
	got, d, err := readSnapshot(ctx, destination, c)
	must(t, err)
	defer d.Close()
	if got != m {
		t.Fatal("recovered snapshot manifest changed")
	}
	evidence, err := readSnapshotRecovery(d, m, c)
	must(t, err)
	if evidence.Publication.CommitHash != recovery.CommitHash || evidence.Proof.After != recovery.After {
		t.Fatal("snapshot lost ancestor provenance")
	}
	db, err := snapshotDB(d, "state.db", true)
	must(t, err)
	defer db.Close()
	must(t, verifyRecoveryRuntimeSQL(ctx, db, evidence.Proof))
	if err = verifySnapshotDB(ctx, db, SnapshotManifest{Version: 1, Identity: m.Identity, Schema: m.Schema}, c); err == nil {
		t.Fatal("recovered snapshot became an ordinary snapshot")
	}
	// Ordinary startup and the old quarantine API remain fenced. The new artifact
	// alone cannot grant delivery or silently replace the ancestor-aware lifecycle.
	if opened, e := Open(destination, c, false); e == nil {
		opened.Close()
		t.Fatal("snapshot bypassed recovery")
	}
	if err = PrepareRecoveryQuarantine(ctx, path, destination, c); err == nil {
		t.Fatal("unmanaged recovery bypassed ancestor lifecycle")
	}
}

func TestRecoveredSnapshotRejectsLostOrChangedProvenance(t *testing.T) {
	ctx := context.Background()
	path, c, revision, recovery := recoveredDeliveryFixture(t, false, true)
	s, err := OpenManagedRecoveryVerification(path, c, recovery.After, revision, recovery.CommitHash)
	must(t, err)
	defer s.Close()
	for _, damage := range []string{"missing", "adoption", "proof", "publication", "downgrade", "ancestor", "ancestor-adoption", "runtime-owner"} {
		t.Run(damage, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "snapshot")
			m, err := s.Snapshot(ctx, dest)
			must(t, err)
			d, err := safefs.Open(dest)
			must(t, err)
			defer d.Close()
			var e snapshotRecoveryEvidence
			must(t, d.ReadJSON(snapshotRecoveryFile, &e))
			switch damage {
			case "missing":
				must(t, os.Remove(filepath.Join(dest, snapshotRecoveryFile)))
			case "adoption":
				e.Adoption.After.Recovery = "other"
				must(t, d.WriteJSON(snapshotRecoveryFile, e, true))
			case "proof":
				e.Proof.ProofHash = protocol.Hash("other")
				must(t, d.WriteJSON(snapshotRecoveryFile, e, true))
			case "publication":
				e.Publication.CommitHash = protocol.Hash("other")
				must(t, d.WriteJSON(snapshotRecoveryFile, e, true))
			case "ancestor":
				must(t, os.Remove(filepath.Join(dest, snapshotProofDirectory, publicationExchange)))
			case "runtime-owner":
				must(t, os.Remove(filepath.Join(dest, snapshotProofDirectory, managedDeliveryOwnerFile)))
			case "ancestor-adoption":
				must(t, os.Remove(filepath.Join(dest, snapshotProofDirectory, recoveryVerificationComplete)))
			case "downgrade":
				m.Version = 1
				m.RecoveryHash = ""
				must(t, d.WriteJSON("snapshot.json", m, true))
			}
			if _, err = VerifySnapshot(ctx, dest, c); err == nil {
				t.Fatal("changed provenance accepted")
			}
		})
	}
	must(t, os.Remove(filepath.Join(path, recoveryVerificationComplete)))
	dest := filepath.Join(t.TempDir(), "not-created")
	if _, err = s.Snapshot(ctx, dest); err == nil {
		t.Fatal("snapshot trusted a lost adoption anchor")
	}
	if _, err = os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("failed provenance check created an artifact", err)
	}
}
