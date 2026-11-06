package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedRepeatedCommandRecovery(t *testing.T) {
	spec, i, c, _, _ := managedCommandRecoveryFixture(t, true)
	ctx := context.Background()
	i.nextRecoveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	m, err := i.prepare(ctx, "adopt-recovery", spec)
	if err != nil {
		t.Fatal(err)
	}
	for epoch := 1; epoch <= 3; epoch++ {
		m, err = i.prepareTelegramRenewal(ctx, spec.Root)
		if err != nil {
			t.Fatal("renew", epoch, err)
		}
		receipt := managedTelegramReceiptForTest(t, m)
		if delay := time.Until(receipt.Renewal.Cursor.NotBefore.Add(time.Millisecond)); delay > 0 {
			time.Sleep(delay)
		}
		record := freshDeliveryQualification(t, spec.Root)
		m, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(record), false)
		if err != nil {
			t.Fatal("activate", epoch, err)
		}
		m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(record), "read", false)
		if err != nil {
			t.Fatal("enable", epoch, err)
		}
		s, err := openManagedStore(m, c)
		if err != nil {
			t.Fatal("open", epoch, err)
		}
		if err = s.BindManagedCommandAuthority(func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		d := c.Worker.Interaction.Destinations[0]
		in := interaction.Inbound{Provider: "telegram", Account: c.Telegram.Account, NativeID: "recovered-revoked", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"}
		if _, err = s.AdmitCommand(ctx, in); err == nil {
			t.Fatal("later recovery restored revoked actor")
		}
		artifact := filepath.Join(t.TempDir(), "snapshot")
		snapshot, err := s.Snapshot(ctx, artifact)
		if err != nil {
			t.Fatal("snapshot", epoch, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		if epoch == 3 {
			break
		}
		// The snapshot predates this disable. Recovery must retain the later head,
		// its policy and the consumed provider handoff without reusing its cutoff.
		m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		m, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(m), "", false)
		if err != nil {
			t.Fatal(err)
		}
		q, err := i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false)
		if err != nil {
			t.Fatal("quarantine", epoch, err)
		}
		intake := func(operation string, extra ...string) {
			t.Helper()
			args := append([]string{operation, "--local", "--config", spec.RuntimeFile, "--state-dir", spec.State, "--operation", q.Operation}, extra...)
			var out bytes.Buffer
			if err := runRecoveryIntakeCLIWith(args, &out, telegramCheckTransport{}); err != nil {
				t.Fatal(operation, epoch, err)
			}
		}
		intake("recovery-telegram-history", "--from", artifact, "--snapshot-id", snapshot.ID, "--database-hash", snapshot.DatabaseHash)
		intake("recovery-telegram-drain", "--receivers-stopped")
		for _, p := range c.Worker.Producers {
			if _, err = worker.FenceRecoveryOwnership(ctx, spec.State, c.Worker, p.Producer); err != nil {
				t.Fatal(err)
			}
		}
		intake("recovery-requests-seal", "--reconciliation-complete")
		intake("recovery-telegram-cursor")
		publication := publishPreparedManagedRecovery(t, spec.State, c.Worker, q.Operation)
		spec.RecoveryCommitHash = publication.Commit.CommitHash
		m, err = i.prepare(ctx, "adopt-recovery", spec)
		if err != nil {
			t.Fatal("adopt", epoch, err)
		}
		if m.TelegramRenewalID != "" || m.Commands != nil || m.DeliveryRecovery.Version != 2 {
			t.Fatal("adoption retained old intake authority")
		}
	}
}

func TestManagedRepeatedRecoveryLifecycle(t *testing.T) {
	spec, i, c, original, _ := managedDeliveryRecoveryFixture(t)
	ctx := context.Background()
	m, err := i.prepare(ctx, "adopt-recovery", spec)
	if err != nil {
		t.Fatal(err)
	}
	for epoch := 2; epoch <= 3; epoch++ {
		qualification := freshDeliveryQualification(t, spec.Root)
		active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(qualification), false)
		if err != nil {
			t.Fatal(err)
		}
		m, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(active), "", false)
		if err != nil {
			t.Fatal(err)
		}
		if err = i.services.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		artifact := filepath.Join(t.TempDir(), "snapshot")
		snapshot, err := i.managedSnapshot(ctx, spec.Root, protocol.Hash(m), artifact, false)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Version != 2 {
			t.Fatal("managed snapshot lost lineage")
		}
		if err = i.services.EnableStart(ctx); err != nil {
			t.Fatal(err)
		}
		i.nextRecoveryProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old binary") }
		if _, err = i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false); err == nil {
			t.Fatal("old helper admitted next recovery")
		}
		if _, err = os.Stat(filepath.Join(spec.Root, managedRecoveryPreparationName(m))); !os.IsNotExist(err) {
			t.Fatal("failed capability check wrote intent", err)
		}
		i.nextRecoveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
		interrupted := errors.New("interrupted")
		i.fault = func(phase string) error {
			if phase == "recovery-preparation-intent" {
				return interrupted
			}
			return nil
		}
		if _, err = i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false); !errors.Is(err, interrupted) {
			t.Fatal(err)
		}
		i.fault = nil
		spec.RecoveryCommitHash = ""
		if _, err = i.prepare(ctx, "update", spec); err == nil {
			t.Fatal("update bypassed next recovery intent")
		}
		if _, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(qualification), false); err == nil {
			t.Fatal("activation bypassed next recovery intent")
		}
		q, err := i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false)
		if err != nil {
			t.Fatal(err)
		}
		again, err := i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(m), artifact, snapshot.ID, snapshot.DatabaseHash, false)
		if err != nil || again != q {
			t.Fatal("resume changed quarantine", err)
		}
		for _, p := range c.Worker.Producers {
			if _, err = worker.FenceRecoveryOwnership(ctx, spec.State, c.Worker, p.Producer); err != nil {
				t.Fatal(err)
			}
		}
		publication := publishPreparedManagedRecovery(t, spec.State, c.Worker, q.Operation)
		spec.RecoveryCommitHash = publication.Commit.CommitHash
		m, err = i.prepare(ctx, "adopt-recovery", spec)
		if err != nil {
			t.Fatal(err)
		}
		if m.DeliveryRecovery.Version != 2 || m.DeliveryOwnerHash != original.DeliveryOwnerHash || m.DeliveryRecovery.Owner.Identity != original.Identity || m.Identity != q.After {
			t.Fatal("later adoption changed original owner")
		}
		if _, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(qualification), false); err == nil {
			t.Fatal("old epoch qualification admitted sends")
		}
		s, err := openManagedStore(m, c)
		if err != nil {
			t.Fatal(err)
		}
		if !s.VerificationOnly() {
			t.Fatal("adoption granted sends")
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func publishPreparedManagedRecovery(t *testing.T, path string, c worker.Config, operation string) worker.RecoveryPublicationResult {
	t.Helper()
	ctx := context.Background()
	review, err := worker.ReviewRecovery(ctx, path, c, operation)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := worker.CheckpointRecovery(ctx, path, c, operation, review.ReviewHash)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := worker.VerifyRecoveryCheckpoint(ctx, path, c, operation, review.ReviewHash, cp.CheckpointHash)
	if err != nil {
		t.Fatal(err)
	}
	image, err := worker.PrepareRecoveryPublication(ctx, path, c, operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := worker.PublishRecovery(ctx, path, c, operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
	if err != nil {
		t.Fatal(err)
	}
	return publication
}
