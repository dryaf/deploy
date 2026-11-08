package install

import (
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"slices"
)

// The manifest anchors every retained rotation. Deactivation, artifact updates
// and specialist recovery preserve this pin, so neither losing an old record
// nor starting a new admission lineage can silently release its exclusions.
func retainedDeliveryCredentials(d *safefs.Directory, j managedJournal, c Runtime) ([]string, error) {
	return deliveryCredentialAncestry(d, j, c)
}

// Historical snapshots start from their own manifest anchor, never from a
// directory listing that might include later rotations.
func deliveryCredentialAncestry(d *safefs.Directory, j managedJournal, c Runtime) ([]string, error) {
	m := j.Intent.After
	paths, err := runtimeCredentialPaths(m, c)
	if err != nil {
		return nil, err
	}
	ref := m.ReportCredentialHistory
	for depth := 0; ref != nil; depth++ {
		if depth >= 128 || !safefs.ValidID(ref.Operation, "n") || !safefs.DigestPattern.MatchString(ref.Hash) {
			return nil, fmt.Errorf("invalid or excessive report credential history")
		}
		var intent managedIntent
		if err := d.ReadJSON("operations/"+ref.Operation+"/intent.json", &intent); err != nil {
			return nil, err
		}
		if intent.ID != ref.Operation || intent.After.Root != m.Root || intent.After.Installation != m.Installation || intent.After.ReportCredentialHistory == nil || *intent.After.ReportCredentialHistory != *ref || intent.Before == nil {
			return nil, fmt.Errorf("report credential history anchor changed")
		}
		after, err := managedRuntime(d, intent.After)
		if err != nil {
			return nil, err
		}
		if intent.DeliveryProvider != nil {
			if protocol.Hash(intent.DeliveryProvider) != ref.Hash {
				return nil, fmt.Errorf("provider credential anchor changed")
			}
			if _, err = deliveryProviderPause(d, managedJournal{Intent: intent, Phase: "complete"}, after); err != nil {
				return nil, err
			}
		} else {
			if !validStagedProviderCredentialHistory(intent) {
				return nil, fmt.Errorf("staged provider credential anchor changed")
			}
			if err = (managedJournal{Intent: intent, Phase: "complete"}).validate(); err != nil {
				return nil, err
			}
			if _, err = managedBootstrapLimitsPair(d, managedJournal{Intent: intent, Phase: "complete"}, after); err != nil {
				return nil, err
			}
		}
		before, err := managedRuntime(d, *intent.Before)
		if err != nil {
			return nil, err
		}
		oldPaths, err := runtimeCredentialPaths(*intent.Before, before)
		if err != nil {
			return nil, err
		}
		newPaths, err := runtimeCredentialPaths(intent.After, after)
		if err != nil {
			return nil, err
		}
		paths = append(paths, oldPaths...)
		paths = append(paths, newPaths...)
		ref = intent.Before.ReportCredentialHistory
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func runtimeCredentialPaths(m ManagedManifest, c Runtime) ([]string, error) {
	return runtimeCredentialPathsPolicy(m, c, true)
}
func runtimeCredentialPathsPolicy(m ManagedManifest, c Runtime, committed bool) ([]string, error) {
	out := directRuntimeCredentialPaths(c)
	if m.FreshSignal != nil {
		service, _, err := inspectRetainedFreshSignal(*m.FreshSignal)
		if err != nil {
			return nil, err
		}
		for _, p := range c.Providers {
			if p.SignalDelivery != nil {
				if err = checkFreshSignalDelivery(service, p, committed); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, service.Spec.AccountFile, service.Spec.RecipientFile)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func stagedProviderCredentialHistory(i managedIntent) *ManagedReportCredentialHistory {
	if i.Before == nil || i.After.FreshSignal == nil || i.Operation != "bootstrap-providers" && i.Operation != "enrolled-providers" {
		return nil
	}
	normalized := i.After
	normalized.ReportCredentialHistory = i.Before.ReportCredentialHistory
	return &ManagedReportCredentialHistory{Operation: i.ID, Hash: protocol.Hash(struct{ ID, Kind, Before, After string }{i.ID, i.Operation, protocol.Hash(*i.Before), protocol.Hash(normalized)})}
}
func validStagedProviderCredentialHistory(i managedIntent) bool {
	want := stagedProviderCredentialHistory(i)
	return want != nil && i.After.ReportCredentialHistory != nil && *want == *i.After.ReportCredentialHistory
}

// ValidateStagedProviderCredentialHistory lets the shared coordinator verify the
// exact domain anchor without granting any other installation field change.
func ValidateStagedProviderCredentialHistory(before, after ManagedManifest, operation string) error {
	if after.ReportCredentialHistory == nil || !safefs.ValidID(after.ReportCredentialHistory.Operation, "n") || !validStagedProviderCredentialHistory(managedIntent{ID: after.ReportCredentialHistory.Operation, Operation: operation, Before: &before, After: after}) {
		return fmt.Errorf("staged provider credential history differs from the exact domain transition")
	}
	return nil
}
