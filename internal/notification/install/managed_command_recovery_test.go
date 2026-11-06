package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func managedCommandRecoveryFixture(t *testing.T, narrow ...bool) (ManagedSpec, managedInstaller, Runtime, ManagedManifest, string) {
	t.Helper()
	spec, i, c, active, record := managedCommandsFixture(t)
	ctx := context.Background()
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.recoveryProbe, i.deliveryRecoveryProbe, i.telegramHandoffProbe, i.telegramProbe, i.commandRecoveryProbe = nop, nop, nop, nop, nop
	i.telegramTransport = telegramCheckTransport{}
	enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openManagedStore(enabled, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindManagedCommandAuthority(func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err = s.PersistTelegram(ctx, provider.IntakePage{Account: c.Telegram.Account}); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "old-command-snapshot")
	manifest, err := s.Snapshot(ctx, snapshot)
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	drained, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow) == 1 && narrow[0] {
		i.commandPolicyProbe = nop
		drained, err = i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(drained), "", "", false, "[]")
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(drained), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = i.services.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	publication := publishManagedRecoveryFrom(t, spec, snapshot, &manifest)
	spec.RecoveryCommitHash = publication.Commit.CommitHash
	return spec, i, c, before, enabled.CommandHistoryHash
}

func TestManagedCommandRecoveryPreservesLaterDisable(t *testing.T) {
	testManagedCommandRecoveryPreservesPolicy(t, false)
}
func TestManagedCommandRecoveryPreservesLaterPolicy(t *testing.T) {
	testManagedCommandRecoveryPreservesPolicy(t, true)
}
func testManagedCommandRecoveryPreservesPolicy(t *testing.T, narrow bool) {
	spec, i, c, before, enabledHash := managedCommandRecoveryFixture(t, narrow)
	ctx := context.Background()
	after, err := i.prepare(ctx, "adopt-recovery", spec)
	if err != nil {
		t.Fatal("adopt command history", err)
	}
	if after.Commands != nil || after.Mode != "verification" || after.CommandHistoryHash == before.CommandHistoryHash {
		t.Fatal("recovery did not fence command admission", after)
	}
	s, err := openManagedStore(after, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.RequireManagedCommandHistory(enabledHash) == nil {
		t.Fatal("snapshot enable replaced later disable")
	}
	s.Close()
	after, err = i.prepareTelegramRenewal(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	receipt := managedTelegramReceiptForTest(t, after)
	if delay := time.Until(receipt.Renewal.Cursor.NotBefore.Add(time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}
	record := freshDeliveryQualification(t, spec.Root)
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(after), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	if err != nil {
		t.Fatal("recovered command admission", err)
	}
	if narrow {
		s, err = openManagedStore(enabled, c)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.BindManagedCommandAuthority(func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		d := c.Worker.Interaction.Destinations[0]
		in := interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "recovered-revoked", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"}
		if _, err = s.AdmitCommand(ctx, in); err == nil {
			t.Fatal("recovery restored revoked actor")
		}
		s.Close()
	}
	if err = os.Remove(filepath.Join(spec.State, "managed-commands", before.CommandHistoryHash+".json")); err != nil {
		t.Fatal(err)
	}
	if s, err = openManagedStore(enabled, c); err == nil {
		s.Close()
		t.Fatal("lost pre-recovery disable accepted")
	}
}

func TestManagedCommandRecoveryAdoptionInterrupted(t *testing.T) {
	for _, point := range []string{"command-recovery-enrolled", "stopped", "manifest-published"} {
		t.Run(point, func(t *testing.T) {
			spec, i, c, _, _ := managedCommandRecoveryFixture(t)
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
			if !s.VerificationOnly() || m.Commands != nil {
				t.Fatal("adoption admitted commands")
			}
			s.Close()
			if err := os.Remove(filepath.Join(spec.State, "managed-commands", m.CommandHistoryHash+"-complete.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := i.resume(context.Background(), spec.Root); err == nil {
				t.Fatal("completed adoption recreated lost history")
			}
		})
	}
}

func TestManagedCommandRecoveryPreparationCapabilityPreflight(t *testing.T) {
	spec, i, _, active, record := managedCommandsFixture(t)
	ctx := context.Background()
	enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(enabled), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = i.services.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	snapshot, err := i.managedSnapshot(ctx, spec.Root, protocol.Hash(before), artifact, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = i.services.EnableStart(ctx); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("old helper lacks command recovery")
	i.commandRecoveryProbe = func(context.Context, *safefs.Directory, string, string) error { return stop }
	for _, dry := range []bool{true, false} {
		if _, err = i.prepareManagedRecovery(ctx, spec.Root, protocol.Hash(before), artifact, snapshot.ID, snapshot.DatabaseHash, dry); !errors.Is(err, stop) {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(spec.Root, managedRecoveryPreparationFile)); !os.IsNotExist(err) {
			t.Fatal("unsupported helper wrote preparation", err)
		}
		state, err := inspectManagedService(ctx, i.services, before, false)
		if err != nil || state.Active != "active" {
			t.Fatal("unsupported helper stopped service", state, err)
		}
	}
}
