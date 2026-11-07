package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/safefs"
)

// PolicyReview is a local inspection result, never adoption or activation
// authority. Callers must translate references into public policy before sending
// a response; runtime fingerprints and provider identities remain private.
type PolicyReview struct {
	Manifest ManagedManifest
	Input    RuntimeInput
	Runtime  []byte // Exact unresolved bytes, including original route labels.
	Signal   *FreshSignalSpec
	Native   *FreshSignalManifest // Canonically inspected owned native service.
}

func InspectPolicyReview(ctx context.Context, root string) (PolicyReview, error) {
	return newManagedInstaller().inspectPolicyReview(ctx, root)
}

func (i managedInstaller) inspectPolicyReview(ctx context.Context, root string) (PolicyReview, error) {
	var result PolicyReview
	d, err := privateManagedDir(root, true)
	if err != nil {
		return result, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return result, err
	}
	m := j.Intent.After
	if j.Phase != "complete" || m.Root != root || (m.Mode != "verification" && m.Mode != "delivery") {
		return result, fmt.Errorf("policy review requires a completed retained managed installation")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return result, err
	}
	if _, err = verifyManagedArtifacts(d, m); err != nil {
		return result, err
	}
	// The completed branch verifies service, database and lifecycle receipts. It
	// does not resume an unfinished operation or create missing authority.
	if _, err = i.resumeLocked(ctx, d, j, lock); err != nil {
		return result, err
	}
	raw, err := readManagedInput(m.Paths.Config, safefs.MaxRecordBytes)
	if err != nil {
		return result, err
	}
	if managedDigest(raw) != m.RuntimeHash {
		return result, fmt.Errorf("policy review runtime changed")
	}
	if err = safefs.DecodeJSON(raw, &result.Input, safefs.MaxRecordBytes); err != nil {
		return result, err
	}
	if m.FreshSignal != nil {
		service, e := InspectFreshSignal(ctx, *m.FreshSignal, false)
		if e != nil {
			return PolicyReview{}, e
		}
		result.Signal = &service.Spec
		result.Native = &service
	}
	result.Manifest = m
	result.Runtime = raw
	return result, nil
}
