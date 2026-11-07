package install

import (
	"context"
	"fmt"
	"reflect"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// InspectCommandDeliveryContinuation proves that an exact earlier delivery
// manifest is an ancestor of the current installation through command-only
// transitions, retained LED output toggles and explicitly pinned pause/renewal pairs. It grants neither producer ownership nor command admission and
// never resumes an operation. A specialist may retain its original delivery
// receipt instead of repeating a completed reporting cutover after admission.
func InspectCommandDeliveryContinuation(ctx context.Context, root, currentHash, earlierHash string) (ManagedManifest, error) {
	return inspectDeliveryContinuation(ctx, root, currentHash, "", earlierHash, nil)
}

// InspectCapturedDeliveryContinuation also proves that a retained reporting
// snapshot lies between the current installation and an earlier scope receipt.
// Both being ancestors independently is not enough: their order must match.
func InspectCapturedDeliveryContinuation(ctx context.Context, root, currentHash, capturedHash, earlierHash string) (ManagedManifest, error) {
	if !safefs.DigestPattern.MatchString(capturedHash) {
		return ManagedManifest{}, fmt.Errorf("captured delivery continuation requires an exact receipt")
	}
	return inspectDeliveryContinuation(ctx, root, currentHash, capturedHash, earlierHash, nil)
}

// InspectCapturedReportProfile proves the exact historical credential exclusions
// as well as the captured manifest. A later credential-only renewal may expand
// the current exclusions, but cannot rewrite an earlier reporting snapshot.
func InspectCapturedReportProfile(ctx context.Context, root, currentHash string, captured ManagedReportProfile) (ManagedManifest, error) {
	return inspectDeliveryContinuation(ctx, root, currentHash, captured.ManifestHash, captured.ManifestHash, &captured)
}

func inspectDeliveryContinuation(ctx context.Context, root, currentHash, capturedHash, earlierHash string, profile *ManagedReportProfile) (ManagedManifest, error) {
	var zero ManagedManifest
	if !managedPath(root) || !safefs.DigestPattern.MatchString(currentHash) || !safefs.DigestPattern.MatchString(earlierHash) {
		return zero, fmt.Errorf("command delivery continuation requires exact installation receipts")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	op, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return zero, err
	}
	defer op.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		return zero, err
	}
	defer lease.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return zero, err
	}
	current := j.Intent.After
	if j.Phase != "complete" || current.Root != root || current.Mode != "delivery" || current.DeliveryActivation == nil || protocol.Hash(current) != currentHash {
		return zero, fmt.Errorf("command delivery continuation requires unchanged completed delivery")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return zero, err
	}
	c, err := verifyManagedArtifacts(d, current)
	if err != nil {
		return zero, err
	}
	if _, _, err = readDeliveryAdmission(d, current, c); err != nil {
		return zero, err
	}
	units, err := managedUnitDirectory(current.UnitDirectory)
	if err != nil {
		return zero, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return zero, err
	}
	if !present || managedDigest(unit) != current.UnitHash {
		return zero, fmt.Errorf("command delivery continuation unit changed")
	}
	ancestor := current
	captured := capturedHash == ""
	visit := func(m ManagedManifest, intent *managedIntent) (bool, error) {
		hash := protocol.Hash(m)
		if hash == capturedHash {
			if profile != nil {
				runtime, err := managedRuntime(d, m)
				if err != nil {
					return false, err
				}
				journal := managedJournal{Intent: managedIntent{After: m}, Phase: "complete"}
				if intent != nil {
					journal.Intent = *intent
				}
				credentials, err := deliveryCredentialAncestry(d, journal, runtime)
				if err != nil {
					return false, err
				}
				want := ManagedReportProfile{ManifestHash: hash, Identity: m.Identity, UnitDirectory: m.UnitDirectory, Credentials: credentials}
				if !reflect.DeepEqual(want, *profile) {
					return false, fmt.Errorf("captured reporting credential exclusions changed")
				}
			}
			captured = true
		}
		if hash == earlierHash {
			if !captured {
				return false, fmt.Errorf("captured delivery predates the required scope receipt")
			}
			return true, nil
		}
		return false, nil
	}
	for depth := 0; depth < 256; depth++ {
		if err = ctx.Err(); err != nil {
			return zero, err
		}
		c, err = managedRuntime(d, ancestor)
		if err != nil {
			return zero, err
		}
		if ancestor.Commands != nil {
			if _, err = readManagedCommandAdmission(d, ancestor, c); err != nil {
				return zero, err
			}
		}
		if found, err := visit(ancestor, nil); err != nil {
			return zero, err
		} else if found {
			return current, nil
		}
		next, e := deliveryPolicyPredecessor(d, ancestor)
		if e != nil {
			next, e = renewedDeliveryPredecessor(d, ancestor, c)
			if e == nil {
				proof, verification, err := readDeliveryAdmission(d, ancestor, c)
				if err != nil {
					return zero, err
				}
				if proof.Version == 3 {
					var intent managedIntent
					if err = d.ReadJSON("operations/"+proof.ProviderOperation+"/intent.json", &intent); err != nil {
						return zero, err
					}
					if found, err := visit(verification, &intent); err != nil {
						return zero, err
					} else if found {
						return current, nil
					}
				}
			}
		}
		if e != nil {
			return zero, e
		}
		ancestor = next
	}
	return zero, fmt.Errorf("command delivery history exceeds the review bound")
}
