package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// UpdateBootstrapVerification applies a compatible executable update to an
// unchanged unbound gateway. Configuration, enrollment and helper compatibility
// remain governed by the managed update transaction; no state is initialized.
func UpdateBootstrapVerification(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	return newManagedInstaller().updateBootstrapVerification(ctx, spec, expected)
}
func (i managedInstaller) updateBootstrapVerification(ctx context.Context, spec ManagedSpec, expected string) (ManagedManifest, error) {
	var zero ManagedManifest
	if !safefs.DigestPattern.MatchString(expected) {
		return zero, fmt.Errorf("exact installation receipt required for artifact update")
	}
	if err := spec.validate(); err != nil {
		return zero, err
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lease, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return zero, err
	}
	j, err := readManagedJournal(d)
	if err != nil {
		lease.Close()
		return zero, err
	}
	resuming := j.Intent.Operation == "update" && j.Intent.Before != nil && protocol.Hash(*j.Intent.Before) == expected
	b := j.Intent.After
	if resuming {
		b = *j.Intent.Before
	}
	if b.Mode != "verification" || b.Root != spec.Root || protocol.Hash(b) != expected || b.DeliveryOwnerHash != "" || b.RecoveryCommitHash != "" || b.Commands != nil || (!resuming && j.Phase != "complete") {
		lease.Close()
		return zero, fmt.Errorf("artifact update requires exact unbound verification authority")
	}
	c, err := verifyManagedArtifacts(d, b)
	if err == nil && !c.Bootstrap {
		err = fmt.Errorf("enrolled gateway artifact update requires coordinated migration")
	}
	lease.Close()
	if err != nil {
		return zero, err
	}
	if resuming || b.BinaryHash == spec.BinarySHA256 {
		return i.ensureVerification(ctx, spec)
	}
	return i.prepareExpected(ctx, "update", spec, expected)
}
