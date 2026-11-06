package worker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestNextRecoveryPreservesInheritedLEDFaults(t *testing.T) {
	for _, clearedAfterSnapshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "later-clear"}[clearedAfterSnapshot], func(t *testing.T) {
			ctx := context.Background()
			f := newRuntimeLEDRecoveryFixture(t)
			artifact := filepath.Join(t.TempDir(), "snapshot")
			m, err := f.s.Snapshot(ctx, artifact)
			must(t, err)
			if clearedAfterSnapshot {
				p := f.proposal(t)
				_, err = f.s.ClearRuntimeLEDRecovery(ctx, f.id, p.Hash)
				must(t, err)
			}
			f.api.states["foreign"] = led.State{ID: "foreign", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}
			must(t, f.s.Revoke(ctx, producer, "aggregate-restored"))
			must(t, f.s.Close())
			must(t, prepareNextManagedRecovery(ctx, f.path, artifact, f.config, m, f.binding, f.revision, nil))
			r, err := openRecoveryReconciler(ctx, f.path, f.config, nil)
			must(t, err)
			_, err = r.preserveOriginal(ctx)
			must(t, err)
			operation := r.journal.Operation
			_, err = r.fenceRecoveryOwnership(ctx, producer)
			must(t, err)
			ch, err := r.Challenge(ctx, producer)
			must(t, err)
			must(t, r.Begin(ctx, ch, protocol.RecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: f.binding.After, Producer: producer}}))
			_, err = r.Finish(ctx, ch, snapshot(ch.IssuedAt, 3, 2))
			must(t, err)
			_, err = r.PrepareRecoveryOwnership(ctx, producer)
			must(t, err)
			_, err = r.PrepareRecoveryGeneration(ctx, producer)
			must(t, err)
			v, err := r.reviewLEDInventory(ctx, migrationSocket, f.api, false)
			must(t, err)
			p, err := r.preserveLED(ctx, migrationSocket, v.InventoryID, v.InventoryHash, f.api)
			must(t, err)
			a, err := r.adoptLED(ctx, migrationSocket, RecoveryLEDAdoptionRequest{v.InventoryID, v.InventoryHash, p.RecordHash}, f.api)
			must(t, err)
			_, err = r.handoffLED(ctx, migrationSocket, a.RecordHash)
			must(t, err)
			must(t, r.Close())
			review, err := ReviewRecovery(ctx, f.path, f.config, operation)
			must(t, err)
			cp, err := CheckpointRecovery(ctx, f.path, f.config, operation, review.ReviewHash)
			must(t, err)
			proof, err := VerifyRecoveryCheckpoint(ctx, f.path, f.config, operation, review.ReviewHash, cp.CheckpointHash)
			must(t, err)
			image, err := PrepareRecoveryPublication(ctx, f.path, f.config, operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
			must(t, err)
			publication, err := PublishRecovery(ctx, f.path, f.config, operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
			must(t, err)
			adopted, err := AdoptPublishedRecoveryVerification(ctx, f.path, f.config, publication.Commit.CommitHash)
			must(t, err)
			f.binding = ManagedDeliveryRecovery{Version: 2, Owner: f.binding.Owner, Before: adopted.Before, After: adopted.After, CommitHash: publication.Commit.CommitHash}
			s, err := OpenManagedRecoveryVerification(f.path, f.config, adopted.After, f.revision, publication.Commit.CommitHash)
			must(t, err)
			must(t, s.EnrollManagedDeliveryRecovery(f.binding))
			must(t, s.Close())
			f.open(t)
			must(t, f.s.Stage(producer, "second-led-runtime"))
			must(t, f.s.Activate(producer, "second-led-runtime"))
			must(t, f.s.Replace(ctx, snapshot(f.now, 4, 2)))
			proposal := f.proposal(t)
			result, err := f.s.ClearRuntimeLEDRecovery(ctx, f.id, proposal.Hash)
			must(t, err)
			if len(f.api.cleared) != 1 || clearedAfterSnapshot && result.Outcome != "observed_absent" {
				t.Fatal("later recovery repeated or lost cleanup", result, f.api.cleared)
			}
			if _, ok := f.api.states["foreign"]; !ok {
				t.Fatal("foreign fault was removed")
			}
			must(t, f.s.Close())
			f.open(t)
			_, err = f.s.ClearRuntimeLEDRecovery(ctx, f.id, proposal.Hash)
			must(t, err)
			if len(f.api.cleared) != 1 {
				t.Fatal("restart repeated cleanup")
			}
		})
	}
}
