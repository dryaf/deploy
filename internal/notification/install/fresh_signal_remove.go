package install

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type freshSignalRemovalManager interface {
	freshSignalManager
	Stop(context.Context) error
	Disable(context.Context) error
}

// A retained removal intent fences every runtime and setup retry. Account,
// inbox, credentials and ownership history are intentionally not deleted.
func freshSignalNotRemoving(d *safefs.Directory) error {
	_, err := readSignalVerificationFile(d, "remove.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("fresh Signal removal is retained; resume uninstall")
}

// UninstallFreshSignal removes only the exact service named by its receipt.
// Callers must first retire dependent gateways; this never unlinks the account.
func UninstallFreshSignal(ctx context.Context, ref FreshSignalReference) (FreshSignalManifest, error) {
	return (freshSignalInstaller{manager: userFreshSignalManager{}}).uninstall(ctx, ref)
}

func (i freshSignalInstaller) uninstall(ctx context.Context, ref FreshSignalReference) (FreshSignalManifest, error) {
	var m FreshSignalManifest
	manager, ok := i.manager.(freshSignalRemovalManager)
	if !ok {
		return m, fmt.Errorf("fresh Signal removal manager unavailable")
	}
	d, lock, err := openSignalServiceRoot(ref.Root, false)
	if err != nil {
		return m, err
	}
	defer d.Close()
	defer lock.Close()
	m, j, err := retainedFreshSignal(d, ref)
	if err != nil {
		return m, err
	}
	units, err := freshSignalUnits(m.Spec.UnitDirectory)
	if err != nil {
		return m, err
	}
	defer units.Close()
	raw, exists, err := freshSignalUnitBytes(units)
	if err != nil {
		return m, err
	}
	if exists && managedDigest(raw) != m.UnitHash {
		return m, fmt.Errorf("foreign fresh Signal unit during removal")
	}
	if j.Phase == "removed" {
		if exists {
			return m, fmt.Errorf("removed fresh Signal unit reappeared")
		}
		return m, inspectRemovedFreshSignal(ctx, manager, m)
	}
	// A missing unit before removal intent is lost ownership, not a crash gap.
	if j.Phase == "complete" && !exists {
		return m, fmt.Errorf("fresh Signal unit missing before removal")
	}
	if _, err = manager.Inspect(ctx, m); err != nil {
		return m, err
	}
	if err = runtimeJSONOnceForService(d, "remove.json", m.binding()); err != nil {
		return m, err
	}
	if err = i.boundary(ctx, "remove-intent"); err != nil {
		return m, err
	}
	if err = i.advance(ctx, d, m, &j, "removing"); err != nil {
		return m, err
	}
	if !exists {
		if err = manager.Reload(ctx); err != nil {
			return m, err
		}
	}
	state, err := manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if state.Loaded {
		if !exists {
			return m, fmt.Errorf("fresh Signal loaded without owned unit")
		}
		if err = manager.Stop(ctx); err != nil {
			return m, err
		}
	}
	if err = i.boundary(ctx, "stop-returned"); err != nil {
		return m, err
	}
	state, err = manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if state.Active || state.PID != 0 {
		return m, fmt.Errorf("fresh Signal service did not stop")
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return m, err
	}
	defer lease.Close()
	if exists {
		if err = manager.Disable(ctx); err != nil {
			return m, err
		}
		if err = i.boundary(ctx, "disable-returned"); err != nil {
			return m, err
		}
		state, err = manager.Inspect(ctx, m)
		if err != nil {
			return m, err
		}
		if state.Active || state.Enabled || state.PID != 0 {
			return m, fmt.Errorf("fresh Signal remains enabled or running")
		}
		current, present, err := freshSignalUnitBytes(units)
		if err != nil {
			return m, err
		}
		if !present || string(current) != string(raw) {
			return m, fmt.Errorf("fresh Signal unit changed during removal")
		}
		if err = checkManagedDirectoryPath(units, m.Spec.UnitDirectory, false); err != nil {
			return m, err
		}
		if err = unix.Unlinkat(int(units.File().Fd()), freshSignalUnit, 0); err != nil {
			return m, err
		}
		if err = units.File().Sync(); err != nil {
			return m, err
		}
	}
	if err = i.boundary(ctx, "unit-removed"); err != nil {
		return m, err
	}
	if err = manager.Reload(ctx); err != nil {
		return m, err
	}
	if err = inspectRemovedFreshSignal(ctx, manager, m); err != nil {
		return m, err
	}
	return m, i.advance(ctx, d, m, &j, "removed")
}

func inspectRemovedFreshSignal(ctx context.Context, manager freshSignalManager, m FreshSignalManifest) error {
	state, err := manager.Inspect(ctx, m)
	if err != nil {
		return err
	}
	if state.Loaded || state.Active || state.Enabled || state.PID != 0 {
		return fmt.Errorf("removed fresh Signal service remains loaded")
	}
	return nil
}

// Retained inspection is deliberately separate from live service admission.
// Only an uninstalled gateway may use this to verify its historical dependency.
func retainedFreshSignal(d *safefs.Directory, ref FreshSignalReference) (FreshSignalManifest, signalServiceJournal, error) {
	var j signalServiceJournal
	m, err := readFreshSignal(d, "complete.json")
	if err != nil {
		return m, j, err
	}
	if m.reference() != ref {
		return m, j, fmt.Errorf("fresh Signal removal reference changed")
	}
	if err = checkFreshSignalFiles(d, m, false); err != nil {
		return m, j, err
	}
	if err = verifyFreshSignalComplete(d, m); err != nil {
		return m, j, err
	}
	j, err = readFreshSignalJournal(d, m)
	if err != nil {
		return m, j, err
	}
	if freshSignalPhase(j.Phase) < freshSignalPhase("complete") {
		return m, j, fmt.Errorf("finish fresh Signal installation before removal")
	}
	if j.Phase != "complete" {
		var removal signalaccount.NativeServiceBinding
		if err = d.ReadJSON("remove.json", &removal); err != nil {
			return m, j, err
		}
		if removal != m.binding() {
			return m, j, fmt.Errorf("fresh Signal removal authority changed")
		}
	}
	err = signalaccount.InspectNativeService(m.Spec.AccountState, m.binding())
	return m, j, err
}

func inspectRetainedFreshSignal(ref FreshSignalReference) (FreshSignalManifest, signalServiceJournal, error) {
	var m FreshSignalManifest
	var j signalServiceJournal
	d, err := privateManagedDir(ref.Root, true)
	if err != nil {
		return m, j, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return m, j, err
	}
	defer lock.Close()
	return retainedFreshSignal(d, ref)
}

// InspectFreshSignalRemoval reports completion only after both retained intent
// and the exact manager/unit state prove removal. Pending removal is resumable.
func InspectFreshSignalRemoval(ctx context.Context, ref FreshSignalReference) (FreshSignalManifest, bool, error) {
	m, j, err := inspectRetainedFreshSignal(ref)
	if err != nil || j.Phase != "removed" {
		return m, false, err
	}
	units, err := freshSignalUnits(m.Spec.UnitDirectory)
	if err != nil {
		return m, false, err
	}
	defer units.Close()
	_, exists, err := freshSignalUnitBytes(units)
	if err != nil {
		return m, false, err
	}
	if exists {
		return m, false, fmt.Errorf("removed fresh Signal unit reappeared")
	}
	err = inspectRemovedFreshSignal(ctx, userFreshSignalManager{}, m)
	return m, err == nil, err
}
