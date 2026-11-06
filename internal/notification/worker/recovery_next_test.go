package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestNextRecoveryResumesEveryDurableBoundary(t *testing.T) {
	for _, boundary := range []string{"next_intent", "next_identity", "next_retire_" + recoveryVerificationComplete, "next_retire_" + recoveryVerificationIntent, "next_retire_" + publicationCommitComplete, "next_retire_" + publicationCommitSeal, "next_retire_" + publicationExchange, "next_retire_" + publicationCommitIntent, "next_journal", "identity", "epoch_published", "binding", "classified", "committed", "quarantined"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, c, revision, parent := recoveredDeliveryFixture(t, false, true)
			s, err := OpenManagedRecoveryVerification(path, c, parent.After, revision, parent.CommitHash)
			must(t, err)
			must(t, s.EnrollManagedDeliveryRecovery(parent))
			artifact := filepath.Join(t.TempDir(), "snapshot")
			m, err := s.Snapshot(ctx, artifact)
			must(t, err)
			must(t, s.Close())
			interrupted := errors.New("interrupted")
			err = prepareNextManagedRecovery(ctx, path, artifact, c, m, parent, revision, func(phase string) error {
				if phase == boundary {
					return interrupted
				}
				return nil
			})
			if !errors.Is(err, interrupted) {
				t.Fatalf("boundary %s: %v", boundary, err)
			}
			if opened, err := OpenManagedRecoveryVerification(path, c, parent.After, revision, parent.CommitHash); err == nil {
				opened.Close()
				t.Fatal("interrupted transition admitted old runtime")
			}
			must(t, prepareNextManagedRecovery(ctx, path, artifact, c, m, parent, revision, nil))
			must(t, prepareNextManagedRecovery(ctx, path, artifact, c, m, parent, revision, nil))
			r, err := openRecoveryReconciler(ctx, path, c, nil)
			must(t, err)
			defer r.Close()
			_, err = r.preserveOriginal(ctx)
			must(t, err)
		})
	}
}

func TestNextRecoveryRefusesMissingAncestorOnResume(t *testing.T) {
	ctx := context.Background()
	path, c, revision, parent := recoveredDeliveryFixture(t, false, true)
	s, err := OpenManagedRecoveryVerification(path, c, parent.After, revision, parent.CommitHash)
	must(t, err)
	must(t, s.EnrollManagedDeliveryRecovery(parent))
	artifact := filepath.Join(t.TempDir(), "snapshot")
	m, err := s.Snapshot(ctx, artifact)
	must(t, err)
	must(t, s.Close())
	interrupted := errors.New("interrupted")
	err = prepareNextManagedRecovery(ctx, path, artifact, c, m, parent, revision, func(phase string) error {
		if phase == "next_intent" {
			return interrupted
		}
		return nil
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	d, err := privateDirectory(path)
	must(t, err)
	defer d.Close()
	var intent nextRecoveryIntent
	must(t, d.ReadJSON(nextRecoveryName(parent.CommitHash), &intent))
	must(t, os.Remove(filepath.Join(path, "recovery", intent.Journal.Operation, snapshotProofDirectory, publicationExchange)))
	if err = prepareNextManagedRecovery(ctx, path, artifact, c, m, parent, revision, nil); err == nil {
		t.Fatal("lost ancestor regenerated on resume")
	}
	var identity protocol.Identity
	must(t, d.ReadJSON("identity.json", &identity))
	if identity != parent.After {
		t.Fatal("failed ancestor verification rotated identity")
	}
}

func TestNextRecoveryRetainsAncestorPublication(t *testing.T) {
	ctx := context.Background()
	path, c, revision, parent := recoveredDeliveryFixture(t, false, true)
	for epoch := 2; epoch <= 3; epoch++ {
		s, err := OpenManagedRecoveryVerification(path, c, parent.After, revision, parent.CommitHash)
		must(t, err)
		must(t, s.EnrollManagedDeliveryRecovery(parent))
		artifact := filepath.Join(t.TempDir(), "snapshot")
		m, err := s.Snapshot(ctx, artifact)
		must(t, err)
		must(t, s.Close())
		must(t, prepareNextManagedRecovery(ctx, path, artifact, c, m, parent, revision, nil))
		r, err := openRecoveryReconciler(ctx, path, c, nil)
		must(t, err)
		_, err = r.preserveOriginal(ctx)
		must(t, err)
		operation := r.journal.Operation
		if r.journal.Before != parent.After || r.journal.After == parent.After {
			t.Fatal("next epoch was not rotated")
		}
		var ancestor string
		must(t, r.db.QueryRow("SELECT value FROM metadata WHERE key=?", "recovery-ancestor:"+operation).Scan(&ancestor))
		if ancestor != protocol.Hash(m) {
			t.Fatal("ancestor snapshot is not bound")
		}
		must(t, r.Close())
		_, err = FenceRecoveryOwnership(ctx, path, c, producer)
		must(t, err)

		r, err = openRecoveryReconciler(ctx, path, c, nil)
		must(t, err)
		ch, err := r.Challenge(ctx, producer)
		must(t, err)
		must(t, r.Begin(ctx, ch, protocol.RecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: parent.After, Producer: producer}}))
		_, err = r.Finish(ctx, ch, snapshot(ch.IssuedAt, 1, 1))
		must(t, err)
		_, err = r.PrepareRecoveryOwnership(ctx, producer)
		must(t, err)
		must(t, r.Close())
		generation, err := OpenRecoveryGenerationSession(ctx, path, c)
		must(t, err)
		_, err = generation.PrepareRecoveryGeneration(ctx, producer)
		must(t, err)
		must(t, generation.Close())
		review, err := ReviewRecovery(ctx, path, c, operation)
		must(t, err)
		cp, err := CheckpointRecovery(ctx, path, c, operation, review.ReviewHash)
		must(t, err)
		proof, err := VerifyRecoveryCheckpoint(ctx, path, c, operation, review.ReviewHash, cp.CheckpointHash)
		must(t, err)
		image, err := PrepareRecoveryPublication(ctx, path, c, operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
		must(t, err)
		publication, err := PublishRecovery(ctx, path, c, operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
		must(t, err)
		adopted, err := AdoptPublishedRecoveryVerification(ctx, path, c, publication.Commit.CommitHash)
		must(t, err)
		s, err = OpenManagedRecoveryVerification(path, c, adopted.After, revision, publication.Commit.CommitHash)
		must(t, err)
		parent = ManagedDeliveryRecovery{Version: 2, Owner: parent.Owner, Before: adopted.Before, After: adopted.After, CommitHash: publication.Commit.CommitHash}
		must(t, s.EnrollManagedDeliveryRecovery(parent))
		must(t, s.RequireManagedDeliveryRecovery(parent, protocol.Hash(parent.Owner)))
		_, err = s.Snapshot(ctx, filepath.Join(t.TempDir(), "next-snapshot"))
		must(t, err)
		must(t, s.Close())
	}
}
