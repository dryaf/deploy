package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func recoveredTelegramCommandsFixture(t *testing.T) (ManagedSpec, managedInstaller, Runtime, ManagedManifest, QualificationRecord) {
	t.Helper()
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	spec, i, _, _ := managedTelegramFixtureConfigured(t, func(_ *ManagedSpec, i *managedInstaller, f *fakeManagedServices) {
		i.services = &fakeDeliveryServices{f}
		i.commandProbe, i.deliveryProbe, i.deliveryRecoveryProbe, i.telegramHandoffProbe, i.commandRecoveryProbe = nop, nop, nop, nop, nop
		i.deliveryInspect = func(context.Context, ManagedManifest, bool) error { return nil }
	}, func(c *Runtime) {
		for name, p := range c.Providers {
			base := t.TempDir()
			p.TokenFile, p.ChatFile = filepath.Join(base, "token"), filepath.Join(base, "chat")
			if err := os.WriteFile(p.TokenFile, []byte("123:fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ChatFile, []byte("456"), 0600); err != nil {
				t.Fatal(err)
			}
			p.Destination = provider.TelegramIdentity("123:fixture", "456")
			c.Providers[name] = p
		}
	})
	ctx := context.Background()
	m, err := i.prepareTelegramRenewal(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	receipt := managedTelegramReceiptForTest(t, m)
	// Native monotonic handoff settling is part of the production contract.
	if delay := time.Until(receipt.Renewal.Cursor.NotBefore.Add(time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}
	record := freshDeliveryQualification(t, spec.Root)
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	return spec, i, c, active, record
}

func TestManagedRecoveredTelegramCommandsLifecycle(t *testing.T) {
	spec, i, c, active, record := recoveredTelegramCommandsFixture(t)
	ctx := context.Background()
	s, err := openManagedStore(active, c)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(ctx)
	s.Close()
	if err != nil || h.CommandMode != "" || h.TelegramRenewalID != active.TelegramRenewalID {
		t.Fatal(h, err)
	}
	enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err = openManagedStore(enabled, c)
	if err != nil {
		t.Fatal(err)
	}
	d, err := safefs.Open(spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = requireManagedTelegramCommandHandoff(ctx, d, s, enabled, c); err != nil {
		t.Fatal(err)
	}
	d.Close()
	s.Close()
	drained, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	inactive, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(drained), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = i.prepareTelegramRenewal(ctx, spec.Root); err == nil {
		t.Fatal("consumed cutoff replaced")
	}
	record = freshDeliveryQualification(t, spec.Root)
	active, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(inactive), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "all", false); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRecoveredTelegramCommandCancellation(t *testing.T) {
	for _, at := range []string{"prepared", "telegram-handoff-enrolled", "command-admission-committed"} {
		t.Run(at, func(t *testing.T) {
			spec, i, _, active, record := recoveredTelegramCommandsFixture(t)
			ctx := context.Background()
			stop := errors.New("interrupted command enable")
			i.fault = func(p string) error {
				if p == at {
					return stop
				}
				return nil
			}
			if _, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			d, err := safefs.Open(spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			j, err := readManagedJournal(d)
			d.Close()
			if err != nil {
				t.Fatal(err)
			}
			m, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(j.Intent.After), "", false)
			if err != nil || m.Mode != "verification" || m.Commands != nil {
				t.Fatal(m, err)
			}
		})
	}
}
