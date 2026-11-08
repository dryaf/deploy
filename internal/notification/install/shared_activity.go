package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// ManagedActivityRestoration pins a unique observation cycle across later
// installation operations, including restoration without a delivery owner.
type ManagedActivityRestoration struct {
	Operation string `json:"operation"`
	Before    string `json:"before"`
}

// activityPrevious binds suspension to its exact managed predecessor. An explicit
// uninstall never supplies restoration authority.
type activityPrevious struct {
	Operation string
	Hash      string
}

func sharedActivityOperation(op string) bool {
	return op == "suspend-shared-activity" || op == "restore-shared-activity"
}

func validateSharedActivityJournal(j managedJournal) error {
	i := j.Intent
	if !sharedActivityOperation(i.Operation) || i.Version != managedVersion || !safefs.ValidID(i.ID, "n") || i.Before == nil || i.Before.validate() != nil || i.After.validate() != nil || managedPhase(j.Phase) == 0 || i.Supersedes != nil || i.DeliveryProvider != nil || i.ActivityPrevious == nil || !safefs.ValidID(i.ActivityPrevious.Operation, "n") || !safefs.DigestPattern.MatchString(i.ActivityPrevious.Hash) {
		return fmt.Errorf("invalid shared activity service intent")
	}
	b := *i.Before
	if b.RecoveryCommitHash != "" || b.SignalVerification != nil || b.SignalProfiles != nil || b.Commands != nil {
		return fmt.Errorf("shared activity requires unrecovered verification with disabled command intake")
	}
	want := b
	if i.Operation == "suspend-shared-activity" {
		if b.Mode != "verification" || i.AfterUnit != "" {
			return fmt.Errorf("shared activity suspension requires verification")
		}
		unit, e := renderManagedUnit(b)
		if e != nil || unit != i.BeforeUnit {
			return fmt.Errorf("shared activity prior unit changed")
		}
		want.Mode = "uninstalled"
	} else {
		if b.Mode != "verification" && b.Mode != "uninstalled" {
			return fmt.Errorf("shared activity restoration requires its suspension")
		}
		if b.Mode == "uninstalled" && i.BeforeUnit != "" {
			return fmt.Errorf("suspended shared gateway has prior unit bytes")
		}
		if b.Mode == "verification" {
			unit, e := renderManagedUnit(b)
			if e != nil || unit != i.BeforeUnit {
				return fmt.Errorf("shared verification prior unit changed")
			}
		}
		if i.After.ActivityRestoration == nil || i.After.ActivityRestoration.Operation != i.ID || i.After.ActivityRestoration.Before != protocol.Hash(b) {
			return fmt.Errorf("shared restoration lost its unique observation cycle")
		}
		want.ActivityRestoration = i.After.ActivityRestoration
		want.Mode = "verification"
		want.UnitHash = i.After.UnitHash
		unit, e := renderManagedUnit(want)
		if e != nil || unit != i.AfterUnit || managedDigest([]byte(unit)) != want.UnitHash {
			return fmt.Errorf("shared activity restored unit changed")
		}
	}
	if protocol.Hash(want) != protocol.Hash(i.After) {
		return fmt.Errorf("shared activity changed retained authority")
	}
	return nil
}

func sharedActivityPredecessor(d *safefs.Directory, j managedJournal) (managedJournal, error) {
	if err := validateSharedActivityJournal(j); err != nil {
		return j, err
	}
	ref := j.Intent.ActivityPrevious
	var prior managedIntent
	if err := d.ReadJSON("operations/"+ref.Operation+"/intent.json", &prior); err != nil {
		return j, err
	}
	p := managedJournal{Intent: prior, Phase: "complete"}
	if prior.ID != ref.Operation || protocol.Hash(prior) != ref.Hash || p.validate() != nil || protocol.Hash(prior.After) != protocol.Hash(*j.Intent.Before) {
		return j, fmt.Errorf("shared activity service history changed")
	}
	if j.Intent.Operation == "restore-shared-activity" && j.Intent.Before.Mode == "uninstalled" && prior.Operation != "suspend-shared-activity" {
		return j, fmt.Errorf("explicit removal cannot be restored by shared activity")
	}
	return p, nil
}
func sharedActivityPause(d *safefs.Directory, j managedJournal) (managedJournal, error) {
	for depth := 0; depth < 128; depth++ {
		if j.Intent.Operation == "pause-delivery" {
			if j.validate() != nil || !preservesManagedDeliveryOwner(j) {
				return j, fmt.Errorf("shared activity lost its delivery pause")
			}
			return j, nil
		}
		var err error
		j, err = sharedActivityPredecessor(d, j)
		if err != nil {
			return j, err
		}
	}
	return j, fmt.Errorf("shared activity service history exceeds review bound")
}

// SetSharedActivityService changes service presence only. Producers must already
// be fenced by the shared coordinator; identity, native Signal and queues stay owned.
func SetSharedActivityService(ctx context.Context, root, expected string, disabled bool) (ManagedManifest, error) {
	return newManagedInstaller().setSharedActivityService(ctx, root, expected, disabled)
}
func (i managedInstaller) setSharedActivityService(ctx context.Context, root, expected string, disabled bool) (ManagedManifest, error) {
	var zero ManagedManifest
	if !managedPath(root) || !safefs.DigestPattern.MatchString(expected) {
		return zero, fmt.Errorf("shared activity requires exact service pins")
	}
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
	op := "restore-shared-activity"
	if disabled {
		op = "suspend-shared-activity"
	}
	if j.Intent.Operation == op && j.Intent.Before != nil && protocol.Hash(*j.Intent.Before) == expected {
		return i.resumeLocked(ctx, d, j, lock)
	}
	if j.Phase != "complete" || protocol.Hash(j.Intent.After) != expected {
		return zero, fmt.Errorf("finish the exact retained service operation before shared activity")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return zero, err
	}
	b := j.Intent.After
	if _, err = verifyManagedArtifacts(d, b); err != nil {
		return zero, err
	}
	if !disabled && b.Mode == "uninstalled" && j.Intent.Operation != "suspend-shared-activity" {
		return zero, fmt.Errorf("shared activity restoration has no suspension authority")
	}
	if b.DeliveryOwnerHash != "" {
		if disabled {
			if j.Intent.Operation != "pause-delivery" {
				if j.Intent.Operation != "restore-shared-activity" {
					return zero, fmt.Errorf("pause delivery before shared activity suspension")
				}
				if _, err = sharedActivityPause(d, j); err != nil {
					return zero, err
				}
			}
		} else {
			if _, err = sharedActivityPause(d, j); err != nil {
				return zero, err
			}
		}
	}
	a := b
	beforeUnit, afterUnit := "", ""
	if disabled {
		a.Mode = "uninstalled"
		beforeUnit, err = renderManagedUnit(b)
	} else {
		a.Mode = "verification"
		if b.Mode == "verification" {
			beforeUnit, err = renderManagedUnit(b)
			if err != nil {
				return zero, err
			}
		}
		afterUnit, err = renderManagedUnit(a)
		a.UnitHash = managedDigest([]byte(afterUnit))
	}
	if err != nil {
		return zero, err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	if !disabled {
		a.ActivityRestoration = &ManagedActivityRestoration{Operation: id, Before: protocol.Hash(b)}
	}
	next := managedJournal{Intent: managedIntent{Version: managedVersion, ID: id, Operation: op, Before: &b, After: a, BeforeUnit: beforeUnit, AfterUnit: afterUnit, ActivityPrevious: &activityPrevious{j.Intent.ID, protocol.Hash(j.Intent)}}, Phase: "prepared"}
	if err = i.probeSharedActivity(ctx, d, b); err != nil {
		return zero, err
	}
	if err = next.validate(); err != nil {
		return zero, err
	}
	if err = limitManagedHistoryFor(d, true); err != nil {
		return zero, err
	}
	if err = writeManagedIntent(d, next); err != nil {
		return zero, err
	}
	if err = i.boundary("prepared"); err != nil {
		return zero, err
	}
	return i.resumeLocked(ctx, d, next, lock)
}

func (i managedInstaller) probeSharedActivity(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.sharedActivityProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-shared-activity-probe")
		}
	}
	for _, binary := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, binary.name, binary.hash); err != nil {
			return err
		}
	}
	return nil
}

// ReviewSharedActivityCapabilities is read-only and must precede dependency effects.
func ReviewSharedActivityCapabilities(ctx context.Context, root, expected string) error {
	d, err := privateManagedDir(root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return err
	}
	if j.Phase != "complete" || protocol.Hash(j.Intent.After) != expected {
		return fmt.Errorf("shared activity capability review requires exact completed installation")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return err
	}
	return newManagedInstaller().probeSharedActivity(ctx, d, j.Intent.After)
}

// The permanent restoration anchor requires new observations even for a staged
// gateway without a delivery owner after a verification-only activity cycle.
func verifySharedActivityRestoration(d *safefs.Directory, m ManagedManifest) error {
	ref := m.ActivityRestoration
	if ref == nil {
		return nil
	}
	var intent managedIntent
	if err := d.ReadJSON("operations/"+ref.Operation+"/intent.json", &intent); err != nil {
		return err
	}
	j := managedJournal{Intent: intent, Phase: "complete"}
	if intent.ID != ref.Operation || intent.Operation != "restore-shared-activity" || j.validate() != nil || intent.Before == nil || protocol.Hash(*intent.Before) != ref.Before || intent.After.Root != m.Root || intent.After.Installation != m.Installation || protocol.Hash(intent.After.ActivityRestoration) != protocol.Hash(ref) {
		return fmt.Errorf("shared activity restoration anchor changed")
	}
	_, err := sharedActivityPredecessor(d, j)
	return err
}
