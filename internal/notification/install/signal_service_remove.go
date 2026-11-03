package install

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func (i signalServiceInstaller) uninstall(ctx context.Context, root string) (SignalServiceManifest, error) {
	d, lock, err := openSignalServiceRoot(root, false)
	if err != nil {
		return SignalServiceManifest{}, err
	}
	defer d.Close()
	defer lock.Close()
	m, err := readSignalService(d, "intent.json")
	if err != nil {
		return m, err
	}
	if m.Spec.Root != root {
		return m, fmt.Errorf("Signal service root changed")
	}
	j, err := readSignalServiceJournal(d, m)
	if err != nil {
		return m, err
	}
	if signalServicePhase(j.Phase) < signalServicePhase("complete") {
		return m, fmt.Errorf("resume Signal service setup before uninstall")
	}
	if err = checkSignalServiceAuthority(d, m); err != nil {
		return m, err
	}
	if err = verifySignalServiceComplete(d, m); err != nil {
		return m, err
	}
	if err = runtimeJSONOnceForService(d, "remove.json", m.binding()); err != nil {
		return m, err
	}
	if err = i.boundary("remove-intent"); err != nil {
		return m, err
	}
	return i.removeLocked(ctx, d, m, j)
}
func (i signalServiceInstaller) removeLocked(ctx context.Context, d *safefs.Directory, m SignalServiceManifest, j signalServiceJournal) (SignalServiceManifest, error) {
	if err := checkSignalServiceFiles(d, m, false); err != nil {
		return m, err
	}
	if err := verifySignalServiceComplete(d, m); err != nil {
		return m, err
	}
	units, err := signalServiceUnits(m.Spec.UnitDirectory)
	if err != nil {
		return m, err
	}
	defer units.Close()
	raw, exists, err := signalServiceUnitBytes(units)
	if err != nil {
		return m, err
	}
	if exists && managedDigest(raw) != m.UnitHash {
		return m, fmt.Errorf("foreign Signal unit during uninstall")
	}
	if j.Phase == "removed" {
		if exists {
			return m, fmt.Errorf("removed Signal unit reappeared")
		}
		state, err := i.manager.Inspect(ctx, m)
		if err != nil {
			return m, err
		}
		if state.Loaded || state.Active || state.Enabled {
			return m, fmt.Errorf("removed Signal service remains loaded")
		}
		return m, nil
	}
	if err = i.advance(d, m, &j, "removing"); err != nil {
		return m, err
	}
	if !exists {
		if err = i.manager.Reload(ctx); err != nil {
			return m, err
		}
	}
	state, err := i.manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if state.Loaded {
		if !exists {
			return m, fmt.Errorf("Signal service still loaded without owned unit")
		}
		if err = i.manager.Stop(ctx); err != nil {
			return m, err
		}
	}
	if err = i.boundary("stop-returned"); err != nil {
		return m, err
	}
	state, err = i.manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if state.Active || state.PID != 0 {
		return m, fmt.Errorf("Signal verifier did not stop")
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return m, err
	}
	defer lease.Close()
	v, err := openSignalEndpointVerifierService(ctx, m.Spec.Endpoint.Root, m.Spec.Endpoint.ManifestHash, m.binding())
	if err != nil {
		return m, err
	}
	defer v.close()
	if exists {
		if err = i.manager.Disable(ctx); err != nil {
			return m, err
		}
		if err = i.boundary("disable-returned"); err != nil {
			return m, err
		}
		state, err = i.manager.Inspect(ctx, m)
		if err != nil {
			return m, err
		}
		if state.Active || state.Enabled {
			return m, fmt.Errorf("Signal verifier remains enabled")
		}
		current, present, err := signalServiceUnitBytes(units)
		if err != nil {
			return m, err
		}
		if !present || string(current) != string(raw) {
			return m, fmt.Errorf("Signal unit changed during removal")
		}
		if err = checkManagedDirectoryPath(units, m.Spec.UnitDirectory, false); err != nil {
			return m, err
		}
		if err = unix.Unlinkat(int(units.File().Fd()), signalServiceUnit, 0); err != nil {
			return m, err
		}
		if err = units.File().Sync(); err != nil {
			return m, err
		}
	}
	if err = i.boundary("unit-removed"); err != nil {
		return m, err
	}
	if err = i.manager.Reload(ctx); err != nil {
		return m, err
	}
	state, err = i.manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if state.Loaded || state.Active || state.Enabled {
		return m, fmt.Errorf("Signal service remains loaded after removal")
	}
	if _, err = os.Stat(m.Spec.Root); err != nil {
		return m, err
	}
	return m, i.advance(d, m, &j, "removed")
}
