package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func (i managedInstaller) commandTelegramHandoff(ctx context.Context, d *safefs.Directory, m ManagedManifest, c Runtime, cancel bool) error {
	if m.DeliveryRecovery == nil || c.Telegram == nil || m.TelegramRenewalID == "" {
		return fmt.Errorf("recovered commands require exact delivery lineage and Telegram renewal")
	}
	receipt, err := readManagedTelegramReceipt(d, m)
	if err != nil {
		return err
	}
	s, err := worker.OpenManagedRecoveryVerification(m.Paths.State, c.Worker, m.Identity, m.Revision, m.RecoveryCommitHash)
	if err != nil {
		return err
	}
	defer s.Close()
	if cancel {
		return s.CancelManagedTelegramHandoff(ctx, *m.DeliveryRecovery, c.Telegram.Account, m.TelegramRenewalID, receipt.Renewal.RenewalHash)
	}
	_, err = s.EnrollManagedTelegramHandoff(ctx, *m.DeliveryRecovery, c.Telegram.Account, m.TelegramRenewalID, receipt.Renewal.RenewalHash)
	if err != nil {
		return err
	}
	return i.boundary("telegram-handoff-enrolled")
}

func requireManagedTelegramCommandHandoff(ctx context.Context, d *safefs.Directory, s *worker.Store, m ManagedManifest, c Runtime) error {
	if m.RecoveryCommitHash == "" {
		return nil
	}
	if m.DeliveryRecovery == nil {
		return fmt.Errorf("managed command handoff unavailable")
	}
	if c.Signal != nil {
		if err := s.RequireManagedSignalHandoff(ctx, *c.Signal); err != nil {
			return err
		}
	}
	if c.Telegram == nil {
		return nil
	}
	receipt, err := readManagedTelegramReceipt(d, m)
	if err != nil {
		return err
	}
	want := worker.ManagedTelegramHandoff{Version: 1, CommitHash: m.RecoveryCommitHash, OwnerHash: m.DeliveryOwnerHash, Renewal: receipt.Renewal}
	return s.RequireManagedTelegramHandoff(ctx, want, protocol.Hash(want))
}
