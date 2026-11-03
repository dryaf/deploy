package worker

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func checkpointRetainedProjection(t *testing.T, r *recoveryReconciler) RecoveryStatus {
	t.Helper()
	ctx := context.Background()
	_, err := r.preserveOriginal(ctx)
	must(t, err)
	before, err := r.reviewRecovery(ctx)
	must(t, err)
	copy, err := r.checkpointRecovery(ctx, before.ReviewHash)
	must(t, err)
	after, err := r.reviewRecovery(ctx)
	must(t, err)
	if protocol.Hash(before) != protocol.Hash(after) || copy.ProjectionHash != before.ProjectionHash {
		t.Fatal("checkpoint changed retained evidence", before, copy, after)
	}
	proof, err := verifyRetainedCheckpoint(ctx, r.dir, r.lease, r.config, r.journal.Operation, before.ReviewHash, copy.CheckpointHash, nil)
	must(t, err)
	if proof.CheckpointHash != copy.CheckpointHash || proof.PublicationAllowed || proof.DeliveryEnabled || proof.ExecutionEnabled {
		t.Fatal("retained proof changed checkpoint or enabled execution", proof)
	}
	image, err := r.preparePublication(ctx, before.ReviewHash, copy.CheckpointHash, proof.ProofHash)
	must(t, err)
	if image.ProofHash != proof.ProofHash || image.PublicationAllowed || image.DeliveryEnabled || image.ExecutionEnabled {
		t.Fatal("publication preparation changed proof or enabled execution", image)
	}
	verified, err := verifyImageFixture(ctx, r, image, nil)
	must(t, err)
	if verified != image {
		t.Fatal("historical image verification changed recovered projection")
	}
	after, err = r.reviewRecovery(ctx)
	must(t, err)
	if after.ReviewHash != before.ReviewHash {
		t.Fatal("publication preparation changed reviewed evidence")
	}
	return after
}

func TestRecoveryCheckpointRetainsReconciledProjections(t *testing.T) {
	ctx := context.Background()
	t.Run("delivery", func(t *testing.T) {
		f, d, _ := generationFixture(t)
		_, err := f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
		must(t, err)
		view := checkpointRetainedProjection(t, f.r)
		statusCheck(t, view, "producer/"+producer.ID, "message_dispositions", "complete")
	})
	t.Run("commands", func(t *testing.T) {
		f, _ := commandInstallFixture(t, false)
		_, err := interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
		must(t, err)
		checkpointRetainedProjection(t, f.r)
	})
	t.Run("signal", func(t *testing.T) {
		f, proof := signalCursorFixture(t)
		must(t, f.r.InstallSignalCursor(ctx, proof))
		view := checkpointRetainedProjection(t, f.r)
		statusCheck(t, view, "intake/signal/"+proof.Handoff.Target.Account, "activation_recheck", "pending")
	})
	t.Run("telegram", func(t *testing.T) {
		f, r, _ := telegramCursorFixture(t)
		_, err := r.CommitTelegramRecoveryCursor(ctx, f.Account)
		must(t, err)
		view := checkpointRetainedProjection(t, r)
		statusCheck(t, view, "intake/telegram/"+f.Account, "activation_recheck", "pending")
	})
	t.Run("inherited-led", func(t *testing.T) {
		r, _, _, api, adoption := epochFixture(t, true)
		before := protocol.Hash(api.states)
		_, err := r.handoffLED(ctx, migrationSocket, adoption)
		must(t, err)
		view := checkpointRetainedProjection(t, r)
		statusCheck(t, view, "led", "inherited_faults", "retained_fenced")
		if before != protocol.Hash(api.states) {
			t.Fatal("checkpoint contacted LED daemon")
		}
	})
}
