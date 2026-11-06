package install

import (
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Only an explicit supervision operation may use a removed service as its
// source. Normal runtime/artifact inspection still requires the live binding.
func validateSignalServiceSuccessor(before ManagedManifest, next SignalVerificationReference) error {
	if before.Mode != "verification" || before.SignalVerification == nil || before.SignalVerification.ServiceRoot == "" {
		return fmt.Errorf("Signal replacement requires supervised verification")
	}
	if err := validateManagedSignalService(before, next); err != nil {
		return err
	}
	m, err := inspectSignalService(next)
	if err != nil {
		return err
	}
	if m.Spec.Previous == nil || *m.Spec.Previous != before.SignalVerification.SignalVerificationReference {
		return fmt.Errorf("Signal replacement does not name this exact predecessor")
	}
	d, err := privateManagedDir(m.Spec.Root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	return checkSignalServiceReplacement(d, m)
}

func validateSignalServiceDescendant(before ManagedManifest, next SignalVerificationReference, allowRemoval bool) error {
	if before.Mode != "verification" || before.SignalVerification == nil || before.SignalVerification.ServiceRoot == "" {
		return fmt.Errorf("Signal predecessor must be supervised verification")
	}
	policy := before
	if allowRemoval {
		policy.Mode = "uninstalled"
	}
	if err := validateManagedSignalService(policy, next); err != nil {
		return err
	}
	if next.Root != before.SignalVerification.Root || next.ManifestHash != before.SignalVerification.ManifestHash {
		p, err := inspectSignalVerificationPublication(next)
		if err != nil {
			return err
		}
		return validateNextSignalEndpoint(before, p)
	}
	m, err := inspectSignalServiceRetained(next, allowRemoval)
	if err != nil {
		return err
	}
	for depth := 0; depth < 32 && m.Spec.Previous != nil; depth++ {
		p := *m.Spec.Previous
		m, err = signalServicePredecessor(m.Spec)
		if err != nil {
			return err
		}
		if p == before.SignalVerification.SignalVerificationReference {
			return nil
		}
	}
	return fmt.Errorf("Signal service is outside the retained replacement lineage")
}

// Profile migration pins the endpoint and canonical bytes. An explicitly
// journaled verifier replacement preserves those pins and the effective routes.
func managedSignalBindingDescends(d *safefs.Directory, m ManagedManifest, ancestor *ManagedSignalBinding) bool {
	for depth := 0; depth < 32; depth++ {
		if protocol.Hash(m.SignalVerification) == protocol.Hash(ancestor) {
			return true
		}
		if m.SignalVerification == nil {
			return false
		}
		var intent managedIntent
		if d.ReadJSON("operations/"+m.SignalVerification.Operation+"/intent.json", &intent) != nil || (managedJournal{Intent: intent, Phase: "complete"}).validate() != nil || intent.Operation != "supervise-signal" || intent.Before == nil || protocol.Hash(intent.After.SignalVerification) != protocol.Hash(m.SignalVerification) || intent.After.ConfigHash != m.ConfigHash || intent.After.RuntimeHash != m.RuntimeHash || intent.After.Installation != m.Installation {
			return false
		}
		m = *intent.Before
	}
	return false
}
