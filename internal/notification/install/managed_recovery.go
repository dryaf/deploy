package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func (i managedInstaller) probeRecovery(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	if m.RecoveryCommitHash == "" {
		return nil
	}
	if m.DeliveryRecovery != nil && m.DeliveryRecovery.Version == 2 {
		if err := i.probeNextRecovery(ctx, d, m); err != nil {
			return err
		}
	}
	probe := i.recoveryProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-recovery-probe")
		}
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	return probe(ctx, d, managedHelperName(m), m.HelperHash)
}

func openManagedStore(m ManagedManifest, c Runtime) (*worker.Store, error) {
	s, err := openCanonicalManagedStore(m, c)
	if err != nil {
		return nil, err
	}
	if m.DeliveryOwnerHash != "" {
		if err = requireDeliveryOwner(s, m, c); err != nil {
			s.Close()
			return nil, err
		}
	}
	if err = s.RequireManagedCommandHistory(m.CommandHistoryHash); err != nil {
		s.Close()
		return nil, err
	}
	if m.Commands != nil {
		d, e := privateManagedDir(m.Root, true)
		if e != nil {
			s.Close()
			return nil, e
		}
		_, e = readManagedCommandAdmission(d, m, c)
		d.Close()
		if e == nil {
			e = s.ConfigureManagedCommands(m.Commands.Mode, m.Commands.Hash)
		}
		if e != nil {
			s.Close()
			return nil, e
		}
	}
	if m.SignalProfiles == nil {
		return s, nil
	}
	d, err := privateManagedDir(m.Root, true)
	if err != nil {
		s.Close()
		return nil, err
	}
	effective, err := managedEffectiveRuntime(d, m, c)
	d.Close()
	if err == nil {
		err = s.BindSignalProfileRoutes(effective.Worker, m.SignalProfiles.Hash)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func openCanonicalManagedStore(m ManagedManifest, c Runtime) (*worker.Store, error) {
	if m.Mode == "delivery" {
		if m.DeliveryRecovery != nil {
			s, err := worker.OpenManagedRecoveredDelivery(m.Paths.State, c.Worker, m.Revision, *m.DeliveryRecovery, m.DeliveryOwnerHash, m.DeliveryActivation.Hash)
			if err != nil {
				return nil, err
			}
			if m.TelegramRenewalID != "" {
				d, e := privateManagedDir(m.Root, true)
				if e != nil {
					s.Close()
					return nil, e
				}
				receipt, e := readManagedTelegramReceipt(d, m)
				d.Close()
				if e == nil && c.Telegram == nil {
					e = fmt.Errorf("managed Telegram account missing")
				}
				if e == nil {
					e = s.BindManagedTelegramRenewal(context.Background(), c.Telegram.Account, m.TelegramRenewalID, receipt.Renewal.RenewalHash)
				}
				if e != nil {
					s.Close()
					return nil, e
				}
			}
			return s, nil
		}
		return worker.OpenManagedDelivery(m.Paths.State, c.Worker, m.Identity, m.Revision, deliveryOwner(m, c), m.DeliveryOwnerHash, m.DeliveryActivation.Hash)
	}
	if m.TelegramRenewalID != "" {
		d, err := privateManagedDir(m.Root, true)
		if err != nil {
			return nil, err
		}
		receipt, err := readManagedTelegramReceipt(d, m)
		d.Close()
		if err != nil {
			return nil, err
		}
		if c.Telegram == nil {
			return nil, fmt.Errorf("managed Telegram account missing")
		}
		return worker.OpenManagedTelegramVerification(m.Paths.State, c.Worker, m.Identity, m.Revision, m.RecoveryCommitHash, c.Telegram.Account, m.TelegramRenewalID, receipt.Renewal.RenewalHash)
	}
	if m.RecoveryCommitHash != "" {
		return worker.OpenManagedRecoveryVerification(m.Paths.State, c.Worker, m.Identity, m.Revision, m.RecoveryCommitHash)
	}
	return worker.OpenManagedRuntime(m.Paths.State, c.Worker, m.Identity, m.Revision)
}

func (i managedInstaller) adoptRecovery(ctx context.Context, d *safefs.Directory, j managedJournal, c Runtime) error {
	m := j.Intent.After
	if err := i.probeRecovery(ctx, d, m); err != nil {
		return err
	}
	if j.Intent.Operation != "adopt-recovery" {
		return nil
	}
	review, err := worker.ReviewPublishedRecovery(ctx, m.Paths.State, c.Worker, m.RecoveryCommitHash)
	if err != nil {
		return err
	}
	if j.Intent.Before == nil || review.Before != j.Intent.Before.Identity || review.After != m.Identity {
		return fmt.Errorf("recovery adoption changed managed identity transition")
	}
	adopted, err := worker.AdoptPublishedRecoveryVerification(ctx, m.Paths.State, c.Worker, m.RecoveryCommitHash)
	if err != nil {
		return err
	}
	if adopted != review {
		return fmt.Errorf("recovery changed during adoption")
	}
	if m.DeliveryRecovery != nil {
		if err = i.probeDelivery(ctx, d, m); err != nil {
			return err
		}
		s, e := worker.OpenManagedRecoveryVerification(m.Paths.State, c.Worker, m.Identity, m.Revision, m.RecoveryCommitHash)
		if e != nil {
			return e
		}
		err = s.EnrollManagedDeliveryRecovery(*m.DeliveryRecovery)
		if err == nil {
			err = i.adoptCommandRecovery(ctx, d, j, s)
		}
		closeErr := s.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = i.boundary("delivery-recovery-enrolled"); err != nil {
			return err
		}
	}
	return i.boundary("recovery-adopted")
}

func (i managedInstaller) probeNextRecovery(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.nextRecoveryProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-next-recovery-probe")
		}
	}
	for _, file := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, file.name, file.hash); err != nil {
			return err
		}
	}
	return nil
}
