package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// DeliveryPauseRequest pins an existing delivery installation. Pausing keeps
// producer, delivery-owner and queued-message authority for requalification.
type DeliveryPauseRequest struct {
	Root         string `json:"root"`
	ManifestHash string `json:"manifest_hash"`
}

func (r DeliveryPauseRequest) validate() error {
	if !managedPath(r.Root) || !safefs.DigestPattern.MatchString(r.ManifestHash) {
		return fmt.Errorf("delivery pause requires exact installation pins")
	}
	return nil
}

func EnsureDeliveryPause(ctx context.Context, r DeliveryPauseRequest) (ManagedManifest, error) {
	if err := r.validate(); err != nil {
		return ManagedManifest{}, err
	}
	return newManagedInstaller().prepareDelivery(ctx, "pause-delivery", r.Root, r.ManifestHash, "", false)
}

func InspectDeliveryPause(ctx context.Context, r DeliveryPauseRequest) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectDeliveryPause(ctx, r)
}

func (i managedInstaller) inspectDeliveryPause(ctx context.Context, r DeliveryPauseRequest) (out ManagedManifest, complete bool, err error) {
	if err = r.validate(); err != nil {
		return out, false, err
	}
	d, err := privateManagedDir(r.Root, true)
	if err != nil {
		return out, false, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return out, false, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return out, false, err
	}
	if j.Intent.After.Root != r.Root {
		return out, false, fmt.Errorf("delivery pause root changed")
	}
	if j.Phase == "complete" && j.Intent.After.Mode == "delivery" && protocol.Hash(j.Intent.After) == r.ManifestHash {
		if err = validateManagedPointer(d, j, true); err != nil {
			return out, false, err
		}
		return j.Intent.After, false, nil
	}
	if j.Intent.Operation != "pause-delivery" || j.Intent.Before == nil || protocol.Hash(*j.Intent.Before) != r.ManifestHash || j.Intent.Supersedes != nil {
		return out, false, fmt.Errorf("delivery pause belongs to another transition")
	}
	if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return out, false, err
	}
	if _, err = verifyManagedArtifacts(d, j.Intent.After); err != nil {
		return out, false, err
	}
	if j.Phase != "complete" {
		return j.Intent.After, false, nil
	}
	out, err = i.resumeLocked(ctx, d, j, lock)
	return out, err == nil, err
}

// Only an unchanged, already enrolled owner may retain active producer
// generations across a pause/renewal. Setup, uninstall and policy changes still
// require inactive producers.
func preservesManagedDeliveryOwner(j managedJournal) bool {
	b, a := j.Intent.Before, j.Intent.After
	if sharedActivityOperation(j.Intent.Operation) {
		return validateSharedActivityJournal(j) == nil && b.DeliveryOwnerHash != ""
	}
	if isPausedRuntimeChange(j.Intent.Operation) {
		return validateDeliveryProviderJournal(j) == nil
	}
	return (j.Intent.Operation == "pause-delivery" || j.Intent.Operation == "activate-delivery") && b != nil && j.Intent.Supersedes == nil && b.DeliveryOwnerHash != "" && b.DeliveryOwnerHash == a.DeliveryOwnerHash && b.Identity == a.Identity && b.ConfigHash == a.ConfigHash && b.RecoveryCommitHash == "" && b.CommandHistoryHash == a.CommandHistoryHash && a.Commands == nil && (b.Commands == nil || j.Intent.Operation == "pause-delivery" && b.Commands.Mode == "drain")
}

func (i managedInstaller) probeDeliveryPause(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	probe := i.deliveryPauseProbe
	capability := "__managed-delivery-pause-probe"
	if m.CommandHistoryHash != "" {
		probe = i.commandPauseProbe
		capability = "__managed-command-pause-probe"
	}
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, capability)
		}
	}
	for _, binary := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, binary.name, binary.hash); err != nil {
			return err
		}
	}
	return nil
}

// A paused runtime can retain active generations only under the exact completed
// pause intent. The verification service still has no provider dispatch loop.
func requireManagedVerificationProducers(d *safefs.Directory, s *worker.Store, m ManagedManifest, c Runtime, j managedJournal) error {
	if (j.Intent.Operation == "pause-delivery" || isPausedRuntimeChange(j.Intent.Operation) || j.Intent.Operation == "restore-shared-activity") && preservesManagedDeliveryOwner(j) && protocol.Hash(j.Intent.After) == protocol.Hash(m) && managedPhase(j.Phase) >= managedPhase("published") && m.Mode == "verification" {
		if j.Intent.Operation == "restore-shared-activity" {
			if _, err := sharedActivityPause(d, j); err != nil {
				return err
			}
		}
		if isPausedRuntimeChange(j.Intent.Operation) {
			if _, err := deliveryProviderPause(d, j, c); err != nil {
				return err
			}
		}
		return requireDeliveryOwner(s, m, c)
	}
	return s.RequireInactiveProducers()
}

// Version 2 admissions bind one exact pause intent. This lets a reporting owner
// prove renewal without replacing its original handoff or generation.
func deliveryPausePredecessor(d *safefs.Directory, proof deliveryAdmission, verification ManagedManifest) (*ManagedManifest, error) {
	if proof.Version == 1 {
		if proof.PauseOperation != "" || proof.PauseHash != "" || proof.ProviderOperation != "" || proof.ProviderHash != "" {
			return nil, fmt.Errorf("legacy delivery proof carries pause authority")
		}
		return nil, nil
	}
	if (proof.Version != 2 && proof.Version != 3 && proof.Version != 4) || !safefs.ValidID(proof.PauseOperation, "n") || !safefs.DigestPattern.MatchString(proof.PauseHash) {
		return nil, fmt.Errorf("renewed delivery lacks a pinned pause")
	}
	paused := verification
	if proof.Version == 3 || proof.Version == 4 {
		if !safefs.ValidID(proof.ProviderOperation, "n") || !safefs.DigestPattern.MatchString(proof.ProviderHash) {
			return nil, fmt.Errorf("provider renewal lacks its pinned rotation")
		}
		var transition managedIntent
		if err := d.ReadJSON("operations/"+proof.ProviderOperation+"/intent.json", &transition); err != nil {
			return nil, err
		}
		if transition.ID != proof.ProviderOperation || protocol.Hash(transition) != proof.ProviderHash || protocol.Hash(transition.After) != protocol.Hash(verification) {
			return nil, fmt.Errorf("provider renewal history changed")
		}
		c, err := managedRuntime(d, verification)
		if err != nil {
			return nil, err
		}
		var pause managedJournal
		if proof.Version == 4 {
			pause, err = sharedActivityPause(d, managedJournal{Intent: transition, Phase: "complete"})
		} else {
			pause, err = deliveryProviderPause(d, managedJournal{Intent: transition, Phase: "complete"}, c)
		}
		if err != nil {
			return nil, err
		}
		if pause.Intent.ID != proof.PauseOperation || protocol.Hash(pause.Intent) != proof.PauseHash {
			return nil, fmt.Errorf("provider renewal changed its pause")
		}
		paused = pause.Intent.After
	} else if proof.ProviderOperation != "" || proof.ProviderHash != "" {
		return nil, fmt.Errorf("ordinary renewal carries provider migration authority")
	}
	var intent managedIntent
	if err := d.ReadJSON("operations/"+proof.PauseOperation+"/intent.json", &intent); err != nil {
		return nil, err
	}
	j := managedJournal{Intent: intent, Phase: "complete"}
	if intent.Operation != "pause-delivery" || intent.ID != proof.PauseOperation || protocol.Hash(intent) != proof.PauseHash || j.validate() != nil || !preservesManagedDeliveryOwner(j) || protocol.Hash(intent.After) != protocol.Hash(paused) || verification.DeliveryOwnerHash != protocol.Hash(proof.Owner) {
		return nil, fmt.Errorf("delivery renewal pause history changed")
	}
	return intent.Before, nil
}

func renewedDeliveryPredecessor(d *safefs.Directory, m ManagedManifest, c Runtime) (ManagedManifest, error) {
	if m.DeliveryActivation == nil {
		return ManagedManifest{}, fmt.Errorf("no renewed delivery activation")
	}
	var intent managedIntent
	if err := d.ReadJSON("operations/"+m.DeliveryActivation.Operation+"/intent.json", &intent); err != nil {
		return ManagedManifest{}, err
	}
	if protocol.Hash(intent.After) != protocol.Hash(m) {
		return ManagedManifest{}, fmt.Errorf("renewal does not bypass later command or output changes")
	}
	proof, verification, err := readDeliveryAdmission(d, m, c)
	if err != nil {
		return ManagedManifest{}, err
	}
	prior, err := deliveryPausePredecessor(d, proof, verification)
	if err != nil {
		return ManagedManifest{}, err
	}
	if prior == nil {
		return ManagedManifest{}, fmt.Errorf("delivery has no reviewed pause predecessor")
	}
	return *prior, nil
}
