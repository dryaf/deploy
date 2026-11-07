package install

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

// FreshSignalResumeRequired means retained installation authority is intact,
// but explicit resume must restart its stopped service or finish a restart.
type FreshSignalResumeRequired struct{ Reference FreshSignalReference }

func (FreshSignalResumeRequired) Error() string {
	return "retained Signal service needs explicit resume; account relinking is not required"
}

type freshSignalRestartIntent struct {
	Version   int                  `json:"version"`
	Operation string               `json:"operation"`
	Previous  string               `json:"previous,omitempty"`
	Reference FreshSignalReference `json:"reference"`
}
type freshSignalRestart struct {
	Intent   freshSignalRestartIntent `json:"intent"`
	Complete bool                     `json:"complete"`
}

func (r freshSignalRestart) digest() string { return protocol.Hash(r.Intent) }
func (r freshSignalRestart) validate(ref FreshSignalReference) error {
	if r.Intent.Version != 1 || !safefs.ValidID(r.Intent.Operation, "n") || r.Intent.Reference != ref || r.Intent.Previous != "" && !safefs.DigestPattern.MatchString(r.Intent.Previous) {
		return fmt.Errorf("fresh Signal restart intent changed")
	}
	return nil
}
func readFreshSignalRestart(d *safefs.Directory, m FreshSignalManifest, j signalServiceJournal, allowGap bool) (*freshSignalRestart, bool, error) {
	var r freshSignalRestart
	err := d.ReadJSON("restart.json", &r)
	if errors.Is(err, os.ErrNotExist) && j.Restart == "" {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("fresh Signal restart authority missing: %w", err)
	}
	if err = r.validate(m.reference()); err != nil {
		return nil, false, err
	}
	gap := j.Restart != r.digest()
	if gap && (r.Complete || j.Restart != r.Intent.Previous) {
		return nil, false, fmt.Errorf("fresh Signal restart differs from lifecycle anchor")
	}
	prior := r
	for count := 1; ; count++ {
		if count > 32 {
			return nil, false, fmt.Errorf("fresh Signal restart history exceeds bound")
		}
		if prior.Intent.Previous == "" {
			break
		}
		var next freshSignalRestart
		if err = d.ReadJSON("restart-history/"+prior.Intent.Previous+".json", &next); err != nil {
			return nil, false, err
		}
		if next.validate(m.reference()) != nil || !next.Complete || next.digest() != prior.Intent.Previous {
			return nil, false, fmt.Errorf("fresh Signal restart history changed")
		}
		prior = next
	}
	if gap && !allowGap {
		return &r, true, FreshSignalResumeRequired{Reference: m.reference()}
	}
	return &r, gap, nil
}

// ResumeFreshSignal starts only an intact, enabled service under its original
// account/inbox ownership. Ordinary setup and read-only status never invoke it.
func ResumeFreshSignal(ctx context.Context, ref FreshSignalReference) (FreshSignalManifest, error) {
	return (freshSignalInstaller{manager: userFreshSignalManager{}}).restart(ctx, ref)
}
func (i freshSignalInstaller) restart(ctx context.Context, ref FreshSignalReference) (m FreshSignalManifest, err error) {
	if !managedPath(ref.Root) || !safefs.DigestPattern.MatchString(ref.ManifestHash) {
		return m, fmt.Errorf("exact retained Signal reference required")
	}
	d, lock, err := openSignalServiceRoot(ref.Root, false)
	if err != nil {
		return m, err
	}
	defer d.Close()
	defer lock.Close()
	if err = freshSignalNotRemoving(d); err != nil {
		return m, err
	}
	m, err = readFreshSignal(d, "complete.json")
	if err != nil {
		return m, err
	}
	if m.reference() != ref {
		return m, fmt.Errorf("fresh Signal restart reference changed")
	}
	if err = checkFreshSignalFiles(d, m, true); err != nil {
		return m, err
	}
	if err = verifyFreshSignalComplete(d, m); err != nil {
		return m, err
	}
	var j signalServiceJournal
	if err = d.ReadJSON("lifecycle.json", &j); err != nil {
		return m, err
	}
	if j.ManifestHash != m.ManifestHash || j.Phase != "complete" {
		return m, fmt.Errorf("finish Signal setup or removal before restarting")
	}
	r, gap, err := readFreshSignalRestart(d, m, j, true)
	if err != nil {
		return m, err
	}
	units, err := freshSignalUnits(m.Spec.UnitDirectory)
	if err != nil {
		return m, err
	}
	defer units.Close()
	raw, present, err := freshSignalUnitBytes(units)
	if err != nil {
		return m, err
	}
	if !present || managedDigest(raw) != m.UnitHash {
		return m, fmt.Errorf("fresh Signal restart unit changed")
	}
	if err = signalaccount.InspectNativeService(m.Spec.AccountState, m.binding()); err != nil {
		return m, err
	}
	state, err := i.manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if !state.Loaded || !state.Enabled {
		return m, fmt.Errorf("fresh Signal restart requires its enabled, loaded owned unit")
	}
	if state.Active {
		if gap {
			return m, fmt.Errorf("Signal service started before restart authority was anchored")
		}
		if err = i.manager.Verify(ctx, m); err != nil {
			return m, err
		}
		if r == nil || r.Complete {
			return m, nil
		}
		r.Complete = true
		if err = d.WriteJSON("restart.json", r, true); err != nil {
			return m, err
		}
		return m, i.boundary(ctx, "restart-complete")
	}
	probe := i.restartProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, m FreshSignalManifest) error {
			return probeManagedCapability(ctx, d, "deploy", m.Spec.BinarySHA256, "__fresh-signal-restart-probe")
		}
	}
	if err = probe(ctx, d, m); err != nil {
		return m, err
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return m, err
	}
	defer lease.Close()
	// A stopped systemd unit does not prove its native CLI children released
	// the account. Acquire its real runtime lease before retaining restart intent.
	account, err := signalaccount.OpenNativeRuntime(m.Spec.AccountState, m.binding(), func() error { return checkFreshSignalFiles(d, m, true) })
	if err != nil {
		return m, err
	}
	defer account.Close()
	if r == nil || r.Complete {
		next := freshSignalRestart{Intent: freshSignalRestartIntent{Version: 1, Reference: ref}}
		next.Intent.Operation, err = safefs.NewID("n")
		if err != nil {
			return m, err
		}
		if r != nil {
			// Count validated history before reserving its next bounded entry.
			prior := *r
			for count := 1; ; count++ {
				if count >= 32 {
					return m, fmt.Errorf("Signal restart history full; reviewed archive required")
				}
				if prior.Intent.Previous == "" {
					break
				}
				var nextPrior freshSignalRestart
				if err = d.ReadJSON("restart-history/"+prior.Intent.Previous+".json", &nextPrior); err != nil {
					return m, err
				}
				prior = nextPrior
			}
			next.Intent.Previous = r.digest()
			if err = d.Mkdir("restart-history"); err != nil {
				return m, err
			}
			if err = runtimeJSONOnceForService(d, "restart-history/"+r.digest()+".json", r); err != nil {
				return m, err
			}
		}
		if err = d.WriteJSON("restart.json", next, r != nil); err != nil {
			return m, err
		}
		r = &next
		gap = true
		if err = i.boundary(ctx, "restart-intent"); err != nil {
			return m, err
		}
	}
	if gap {
		j.Restart = r.digest()
		if err = d.WriteJSON("lifecycle.json", j, true); err != nil {
			return m, err
		}
		if err = i.boundary(ctx, "restart-anchored"); err != nil {
			return m, err
		}
	}
	account.Close()
	lease.Close()
	if err = i.manager.Start(ctx); err != nil {
		return m, err
	}
	if err = i.boundary(ctx, "restart-start-returned"); err != nil {
		return m, err
	}
	if err = i.manager.Verify(ctx, m); err != nil {
		return m, err
	}
	if err = i.boundary(ctx, "restart-verified"); err != nil {
		return m, err
	}
	r.Complete = true
	if err = d.WriteJSON("restart.json", r, true); err != nil {
		return m, err
	}
	return m, i.boundary(ctx, "restart-complete")
}
