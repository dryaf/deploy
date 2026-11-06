package install

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func (i managedInstaller) probeTelegramRenewal(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	if m.TelegramRenewalID == "" {
		return nil
	}
	return i.requireTelegramRenewalCapability(ctx, d, m)
}

func (i managedInstaller) requireTelegramRenewalCapability(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.telegramProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-telegram-renewal-probe")
		}
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	return probe(ctx, d, managedHelperName(m), m.HelperHash)
}

func (i managedInstaller) prepareTelegramRenewal(ctx context.Context, root string) (ManagedManifest, error) {
	var zero ManagedManifest
	d, err := privateManagedDir(root, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return zero, err
	}
	if j.Phase != "complete" {
		return zero, fmt.Errorf("unfinished managed operation; use resume")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return zero, err
	}
	before := j.Intent.After
	if before.Root != root || before.Mode != "verification" || before.RecoveryCommitHash == "" {
		return zero, fmt.Errorf("Telegram renewal requires an adopted recovery verification service")
	}
	c, err := verifyManagedArtifacts(d, before)
	if err != nil {
		return zero, err
	}
	if c.Telegram == nil {
		return zero, fmt.Errorf("managed gateway has no interactive Telegram account")
	}
	state, err := privateManagedDir(before.Paths.State, true)
	if err != nil {
		return zero, err
	}
	var consumed worker.ManagedTelegramHandoff
	handoffName := "managed-telegram-handoff-" + protocol.Hash(c.Telegram.Account)
	if before.DeliveryRecovery != nil && before.DeliveryRecovery.Version == 2 {
		handoffName += "-" + protocol.Hash(before.Identity)
	}
	err = state.ReadJSON(handoffName+".json", &consumed)
	state.Close()
	if err == nil {
		return zero, fmt.Errorf("Telegram cutoff already consumed; retain it for this recovery epoch")
	}
	if !os.IsNotExist(err) {
		return zero, err
	}
	units, err := managedUnitDirectory(before.UnitDirectory)
	if err != nil {
		return zero, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return zero, err
	}
	if !present || managedDigest(unit) != before.UnitHash {
		return zero, fmt.Errorf("managed unit changed")
	}
	if _, err = inspectManagedService(ctx, i.services, before, false); err != nil {
		return zero, err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	after := before
	after.TelegramRenewalID = id
	if err = i.probeTelegramRenewal(ctx, d, after); err != nil {
		return zero, err
	}
	if err = limitManagedHistory(d); err != nil {
		return zero, err
	}
	j = managedJournal{Intent: managedIntent{Version: managedVersion, ID: id, Operation: "renew-telegram", Before: &before, After: after, BeforeUnit: string(unit), AfterUnit: string(unit)}, Phase: "prepared"}
	if err = writeManagedIntent(d, j); err != nil {
		return zero, err
	}
	if err = i.boundary("prepared"); err != nil {
		return zero, err
	}
	return i.resumeLocked(ctx, d, j, lock)
}

func managedLeaseMatches(d *safefs.Directory, name string, lease *safefs.Lock) error {
	if lease == nil || lease.File() == nil {
		return fmt.Errorf("managed renewal lease unavailable")
	}
	held, err := lease.File().Stat()
	if err != nil {
		return err
	}
	f, err := d.Open(name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	current, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return fmt.Errorf("managed renewal lease changed: %s", name)
	}
	return nil
}

func (i managedInstaller) renewTelegram(ctx context.Context, d *safefs.Directory, j managedJournal, c Runtime, installation, operation *safefs.Lock) error {
	m := j.Intent.After
	if j.Intent.Operation != "renew-telegram" {
		return verifyManagedTelegramAudit(ctx, d, m, c)
	}
	if c.Telegram == nil {
		return fmt.Errorf("managed Telegram account missing")
	}
	if err := i.probeTelegramRenewal(ctx, d, m); err != nil {
		return err
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return err
	}
	defer units.Close()
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkManagedDirectoryPath(d, m.Root, true); err != nil {
			return err
		}
		if err := managedLeaseMatches(d, "operation.lock", operation); err != nil {
			return err
		}
		if err := managedLeaseMatches(d, "installation.lock", installation); err != nil {
			return err
		}
		actual, err := readManagedJournal(d)
		if err != nil {
			return err
		}
		if protocol.Hash(actual) != protocol.Hash(j) {
			return fmt.Errorf("managed renewal journal changed")
		}
		if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
			return err
		}
		if err = checkManagedDirectoryPath(units, m.UnitDirectory, false); err != nil {
			return err
		}
		unit, present, err := managedUnitBytes(units)
		if err != nil {
			return err
		}
		if !present || managedDigest(unit) != m.UnitHash {
			return fmt.Errorf("managed renewal unit changed")
		}
		if _, err = managedRuntime(d, m); err != nil {
			return err
		}
		state, err := inspectManagedService(ctx, i.services, m, false)
		if err != nil {
			return err
		}
		if state.Active != "inactive" && state.Active != "failed" {
			return fmt.Errorf("managed receiver restarted during Telegram renewal")
		}
		return nil
	}
	if err = check(ctx); err != nil {
		return err
	}
	// Once published, loss of this installation's independent receipt is not a
	// partially acknowledged provider operation. Never silently rebuild it.
	if managedPhase(j.Phase) >= managedPhase("published") {
		if err = verifyManagedTelegramAudit(ctx, d, m, c); err != nil {
			return err
		}
		return check(ctx)
	}
	request, err := prepareManagedTelegramRequest(ctx, d, j, c)
	if err != nil {
		return err
	}
	if err = i.boundary("telegram-request"); err != nil {
		return err
	}
	p := provider.TelegramPoller{Profile: *c.Telegram, Transport: i.telegramTransport}
	renewal, err := worker.RenewManagedTelegramHandoff(ctx, m.Paths.State, c.Worker, m.RecoveryCommitHash, request.Request, p, check)
	if err != nil {
		return err
	}
	if err = i.boundary("telegram-renewed"); err != nil {
		return err
	}
	if err = check(ctx); err != nil {
		return err
	}
	if renewal.Binding.PreviousID != j.Intent.Before.TelegramRenewalID {
		return fmt.Errorf("managed Telegram predecessor changed")
	}
	receipt := managedTelegramReceipt{Operation: j.Intent.ID, ManifestHash: protocol.Hash(m), Renewal: renewal}
	name := "operations/" + j.Intent.ID + "/telegram-receipt.json"
	var saved managedTelegramReceipt
	err = d.ReadJSON(name, &saved)
	if os.IsNotExist(err) {
		err = d.WriteJSON(name, receipt, false)
	} else if err == nil && saved != receipt {
		err = fmt.Errorf("managed Telegram receipt changed")
	}
	if err != nil {
		return err
	}
	if err = i.boundary("telegram-receipt"); err != nil {
		return err
	}
	if err = verifyManagedTelegramAudit(ctx, d, m, c); err != nil {
		return err
	}
	return check(ctx)
}
