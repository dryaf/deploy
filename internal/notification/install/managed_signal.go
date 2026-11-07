package install

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// This reference adopts verification configuration, never account ownership or
// provider admission. The separately running endpoint continues to own its locks.
type SignalVerificationReference struct {
	ServiceRoot  string `json:"service_root,omitempty"`
	ServiceHash  string `json:"service_hash,omitempty"`
	Root         string `json:"root"`
	ManifestHash string `json:"manifest_hash"`
}
type ManagedSignalBinding struct {
	SignalVerificationReference
	Operation string `json:"operation"`
}

func (r SignalVerificationReference) validate() error {
	if (r.ServiceRoot == "") != (r.ServiceHash == "") || r.ServiceRoot != "" && (!managedPath(r.ServiceRoot) || !safefs.DigestPattern.MatchString(r.ServiceHash) || managedWithin(r.Root, r.ServiceRoot) || managedWithin(r.ServiceRoot, r.Root)) {
		return fmt.Errorf("invalid Signal service reference")
	}

	if !managedPath(r.Root) || !safefs.DigestPattern.MatchString(r.ManifestHash) {
		return fmt.Errorf("invalid Signal verification reference")
	}
	return nil
}

// Inspection intentionally does not acquire the endpoint's exclusive operation
// lease: the independently running verifier must keep it throughout adoption.
// This checks immutable publication, not liveness; health is checked separately.
func inspectSignalVerificationPublication(ref SignalVerificationReference) (m SignalVerificationManifest, err error) {
	if err = ref.validate(); err != nil {
		return
	}
	d, err := privateManagedDir(ref.Root, true)
	if err != nil {
		return m, err
	}
	defer d.Close()
	m, err = readSignalVerificationManifest(d, signalVerificationComplete)
	if err != nil {
		return
	}
	if m.Spec.Root != ref.Root || m.ManifestHash != ref.ManifestHash {
		return m, fmt.Errorf("Signal verification publication reference changed")
	}
	check := func() error {
		if err := checkManagedDirectoryPath(d, ref.Root, true); err != nil {
			return err
		}
		dev, ino, err := verificationFileIdentity(d.File())
		if err != nil {
			return err
		}
		if dev != m.RootDevice || ino != m.RootInode {
			return fmt.Errorf("Signal verification root replaced")
		}
		f, err := d.Open(signalVerificationLock, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || uint64(st.Dev) != m.LeaseDevice || st.Ino != m.LeaseInode {
			return fmt.Errorf("Signal verification lease changed")
		}
		return nil
	}
	if err = check(); err != nil {
		return
	}
	intent, err := readSignalVerificationManifest(d, signalVerificationIntent)
	if err != nil {
		return m, err
	}
	if protocol.Hash(intent) != protocol.Hash(m) {
		return m, fmt.Errorf("Signal endpoint intent/completion diverged")
	}
	raw, err := readSignalVerificationFile(d, signalVerificationGateway)
	if err != nil {
		return m, err
	}
	if managedDigest(raw) != m.NextGatewaySHA256 {
		return m, fmt.Errorf("Signal staged configuration changed")
	}
	err = check()
	return
}

func validateManagedSignalPaths(m ManagedManifest, p SignalVerificationManifest) error {
	for _, path := range []string{p.Spec.Root, p.Spec.AccountState, p.Installation.Source, p.Installation.Destination} {
		for _, root := range []string{m.Root, m.UnitDirectory, m.Paths.State} {
			if managedWithin(path, root) || managedWithin(root, path) {
				return fmt.Errorf("Signal endpoint authority overlaps managed installation")
			}
		}
		// The gateway needs only the endpoint publication and socket, not account
		// files or inboxes. Existing account/source exclusions remain valid.
		if path == p.Spec.Root {
			for _, denied := range m.Paths.DeniedPaths {
				if managedWithin(path, denied) || managedWithin(denied, path) {
					return fmt.Errorf("Signal endpoint is excluded by service policy")
				}
			}
		}
	}
	return nil
}
func validateManagedSignalService(m ManagedManifest, ref SignalVerificationReference) error {
	if ref.ServiceRoot == "" {
		return nil
	}
	service, err := inspectSignalServiceRetained(ref, m.Mode == "uninstalled")
	if err != nil {
		return err
	}
	if service.Spec.UnitDirectory != m.UnitDirectory {
		return fmt.Errorf("Signal verifier must use the gateway user manager directory")
	}
	for _, path := range append([]string{m.Root, m.Paths.State, m.UnitDirectory}, m.Paths.DeniedPaths...) {
		if managedWithin(ref.ServiceRoot, path) || managedWithin(path, ref.ServiceRoot) {
			return fmt.Errorf("Signal service authority overlaps gateway paths")
		}
	}
	return nil
}
func validateManagedSignalTransition(operation string, before ManagedManifest, after Runtime, raw []byte, ref SignalVerificationReference) (SignalVerificationManifest, error) {
	if operation == "adopt-signal" {
		return validateManagedSignalChange(before, after, raw, ref)
	}
	if operation != "supervise-signal" || before.Mode != "verification" || before.SignalVerification == nil || ref.ServiceRoot == "" || before.SignalVerification.Root != ref.Root || before.SignalVerification.ManifestHash != ref.ManifestHash || before.RuntimeHash != managedDigest(raw) || before.ConfigHash != protocol.Hash(after) {
		return SignalVerificationManifest{}, fmt.Errorf("Signal supervision must retain the adopted endpoint and exact configuration")
	}
	if before.SignalVerification.ServiceRoot != "" {
		if err := validateSignalServiceSuccessor(before, ref); err != nil {
			return SignalVerificationManifest{}, err
		}
	} else if err := validateManagedSignalService(before, ref); err != nil {
		return SignalVerificationManifest{}, err
	}
	return inspectSignalVerificationPublication(ref)
}

func validateManagedSignalChange(before ManagedManifest, after Runtime, raw []byte, ref SignalVerificationReference) (p SignalVerificationManifest, err error) {
	if before.Mode != "verification" || before.RecoveryCommitHash == "" || before.SignalVerification != nil {
		return p, fmt.Errorf("Signal adoption requires a recovered verification installation without a prior endpoint")
	}
	if err = validateManagedSignalService(before, ref); err != nil {
		return
	}
	p, err = inspectSignalVerificationPublication(ref)
	if err != nil {
		return
	}
	if p.Spec.GatewayState != before.Paths.State || p.Spec.CommitHash != before.RecoveryCommitHash || p.Spec.GatewaySHA256 != before.RuntimeHash || managedDigest(raw) != p.NextGatewaySHA256 {
		return p, fmt.Errorf("Signal endpoint does not recover this exact gateway configuration")
	}
	if err = validateManagedSignalPaths(before, p); err != nil {
		return
	}
	original, err := decodeRuntime(p.OriginalConfig)
	if err != nil {
		return p, err
	}
	next, err := p.gateway()
	if err != nil {
		return p, err
	}
	expected, err := decodeRuntime(next)
	if err != nil {
		return p, err
	}
	if protocol.Hash(original) != before.ConfigHash || protocol.Hash(expected) != protocol.Hash(after) || protocol.Hash(original.Worker) != protocol.Hash(after.Worker) {
		return p, fmt.Errorf("Signal adoption changed configuration outside the frozen endpoint transition")
	}
	return
}

// The adoption's immutable intent and independent endpoint copy remain required
// by every later update, rollback, uninstall and runtime startup.
func readManagedSignalBinding(d *safefs.Directory, m ManagedManifest) (SignalVerificationManifest, error) {
	return readManagedSignalBindingPolicy(d, m, nil)
}
func readManagedSignalBindingPolicy(d *safefs.Directory, m ManagedManifest, successor *SignalVerificationReference) (SignalVerificationManifest, error) {
	return readManagedSignalBindingRetained(d, m, successor, m.Mode == "uninstalled")
}
func readManagedSignalBindingRetained(d *safefs.Directory, m ManagedManifest, successor *SignalVerificationReference, allowRemovedSuccessor bool) (p SignalVerificationManifest, err error) {
	if m.SignalVerification == nil {
		return p, fmt.Errorf("no managed Signal endpoint")
	}
	binding := *m.SignalVerification
	var intent managedIntent
	base := "operations/" + binding.Operation + "/"
	if err = d.ReadJSON(base+"intent.json", &intent); err != nil {
		return
	}
	if err = (managedJournal{Intent: intent, Phase: "complete"}).validate(); err != nil {
		return
	}
	a := intent.After
	if (intent.Operation != "adopt-signal" && intent.Operation != "supervise-signal" && intent.Operation != "adopt-recovery") || intent.ID != binding.Operation || a.SignalVerification == nil || *a.SignalVerification != binding || a.Installation != m.Installation || a.Root != m.Root || a.Paths.State != m.Paths.State || a.UnitDirectory != m.UnitDirectory || a.Identity != m.Identity || a.RecoveryCommitHash != m.RecoveryCommitHash || a.ConfigHash != m.ConfigHash || a.RuntimeHash != m.RuntimeHash || a.Schema != m.Schema {
		return p, fmt.Errorf("managed Signal adoption authority changed")
	}
	if err = d.ReadJSON(base+"signal-endpoint.json", &p); err != nil {
		return
	}
	if err = p.validate(); err != nil {
		return
	}
	actual, err := inspectSignalVerificationPublication(binding.SignalVerificationReference)
	if err != nil {
		return p, err
	}
	if protocol.Hash(actual) != protocol.Hash(p) {
		return p, fmt.Errorf("managed Signal endpoint copy differs from publication")
	}
	if successor != nil {
		err = validateSignalServiceDescendant(m, *successor, allowRemovedSuccessor)
	} else {
		err = validateManagedSignalService(m, binding.SignalVerificationReference)
	}
	if err != nil {
		return p, err
	}
	raw, err := p.gateway()
	if err != nil {
		return p, err
	}
	next, err := decodeRuntime(raw)
	if err != nil {
		return p, err
	}
	if intent.Operation == "adopt-recovery" {
		if intent.Before == nil || intent.Before.SignalVerification == nil || p.Spec.CommitHash != m.RecoveryCommitHash || managedDigest(raw) != m.RuntimeHash || protocol.Hash(next) != m.ConfigHash {
			return p, fmt.Errorf("next Signal adoption pins changed")
		}
		if _, e := readManagedSignalBindingRetained(d, *intent.Before, &binding.SignalVerificationReference, true); e != nil {
			return p, e
		}
		if err = validateNextSignalEndpoint(*intent.Before, p); err != nil {
			return p, err
		}
		original, e := decodeRuntime(p.OriginalConfig)
		if e != nil {
			return p, e
		}
		if protocol.Hash(original) != intent.Before.ConfigHash || protocol.Hash(original.Worker) != protocol.Hash(next.Worker) {
			return p, fmt.Errorf("next Signal adoption changed worker configuration")
		}
		return p, nil
	}
	if intent.Operation == "supervise-signal" {
		if intent.Before.SignalVerification == nil {
			return p, fmt.Errorf("invalid Signal supervision predecessor")
		}
		var nextService *SignalVerificationReference
		if intent.Before.SignalVerification.ServiceRoot != "" {
			nextService = &binding.SignalVerificationReference
			if successor != nil {
				nextService = successor
			}
		}
		previous, e := readManagedSignalBindingRetained(d, *intent.Before, nextService, allowRemovedSuccessor)
		if e != nil {
			return p, e
		}
		if protocol.Hash(previous) != protocol.Hash(p) {
			return p, fmt.Errorf("Signal supervision changed endpoint")
		}
		// Journal validation proves an unchanged configuration; the original
		// adoption above proves these exact staged bytes. Service publication
		// was checked separately, allowing retained removal only for an already
		// uninstalled gateway.
		return p, nil
	}
	ref := binding.SignalVerificationReference
	ref.ServiceRoot, ref.ServiceHash = "", ""
	_, err = validateManagedSignalChange(*intent.Before, next, raw, ref)
	return
}
func (i managedInstaller) probeSignalVerification(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	if m.FreshSignal != nil {
		probe := i.signalProbe
		if probe == nil {
			probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
				return probeManagedCapability(ctx, d, name, hash, "__managed-fresh-signal-probe")
			}
		}
		if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
			return err
		}
		return probe(ctx, d, managedHelperName(m), m.HelperHash)
	}

	if m.SignalVerification == nil {
		return nil
	}
	probe := i.signalProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			capability := "__managed-signal-verification-probe"
			if m.SignalVerification.ServiceRoot != "" {
				capability = "__managed-signal-supervision-probe"
				service, err := inspectSignalServiceRetained(m.SignalVerification.SignalVerificationReference, m.Mode == "uninstalled")
				if err != nil {
					return err
				}
				if service.Spec.Previous != nil {
					capability = "__managed-signal-replacement-probe"
				}
				if service.Spec.Runtime != nil {
					capability = "__managed-signal-production-probe"
				}
			}
			if m.DeliveryRecovery != nil && m.DeliveryRecovery.Version == 2 {
				capability = "__signal-next-recovery-probe"
			}
			return probeManagedCapability(ctx, d, name, hash, capability)
		}
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	return probe(ctx, d, managedHelperName(m), m.HelperHash)
}
func (i managedInstaller) checkSignalVerification(ctx context.Context, m ManagedManifest) error {
	if m.SignalVerification == nil && m.FreshSignal == nil {
		return nil
	}
	check := i.signalCheck
	if check == nil {
		check = checkManagedSignalHealth
	}
	return check(ctx, m)
}

func managedSignalConfigBridge(d *safefs.Directory, old, current ManagedManifest) bool {
	return managedSignalConfigBridgePolicy(d, old, current, nil)
}
func managedSignalConfigBridgePolicy(d *safefs.Directory, old, current ManagedManifest, successor *SignalVerificationReference) bool {
	if current.SignalVerification == nil || old.SignalVerification != nil {
		return false
	}
	var intent managedIntent
	if d.ReadJSON("operations/"+current.SignalVerification.Operation+"/intent.json", &intent) != nil || (managedJournal{Intent: intent, Phase: "complete"}).validate() != nil || intent.Before == nil {
		return false
	}
	for depth := 0; intent.Operation == "supervise-signal"; depth++ {
		if depth >= 32 {
			return false
		}
		predecessor := intent.Before.SignalVerification
		intent = managedIntent{}
		if predecessor == nil || d.ReadJSON("operations/"+predecessor.Operation+"/intent.json", &intent) != nil || (managedJournal{Intent: intent, Phase: "complete"}).validate() != nil || intent.Before == nil {
			return false
		}
	}
	// Only an unchanged Telegram proof may cross this exact Signal-only change.
	b := intent.Before
	if intent.Operation != "adopt-signal" || b.ConfigHash != old.ConfigHash || b.TelegramRenewalID != current.TelegramRenewalID || old.TelegramRenewalID != current.TelegramRenewalID {
		return false
	}
	_, err := readManagedSignalBindingPolicy(d, current, successor)
	return err == nil
}
