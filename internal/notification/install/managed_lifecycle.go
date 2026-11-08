package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// managedInstaller coordinates files and a user manager. Its crash hook is only
// supplied by tests; every hook follows a durable boundary or external effect.
type managedInstaller struct {
	signalDeliveryProbe              func(context.Context, *safefs.Directory, string, string) error
	commandProvidersProbe            func(context.Context, *safefs.Directory, string, string) error
	commandRoutesProbe               func(context.Context, *safefs.Directory, string, string) error
	commandProducerProbe             func(context.Context, *safefs.Directory, string, string) error
	commandFreshnessProbe            func(context.Context, *safefs.Directory, string, string) error
	commandLimitsProbe               func(context.Context, *safefs.Directory, string, string) error
	commandPauseProbe                func(context.Context, *safefs.Directory, string, string) error
	deliveryLimitsProbe              func(context.Context, *safefs.Directory, string, string) error
	deliveryProvidersProbe           func(context.Context, *safefs.Directory, string, string) error
	sharedActivityProbe              func(context.Context, *safefs.Directory, string, string) error
	deliveryPauseProbe               func(context.Context, *safefs.Directory, string, string) error
	bootstrapProvidersProbe          func(context.Context, *safefs.Directory, string, string) error
	enrolledLimitsProbe              func(context.Context, *safefs.Directory, string, string) error
	bootstrapLimitsProbe             func(context.Context, *safefs.Directory, string, string) error
	bootstrapEnrollmentLEDProbe      func(context.Context, *safefs.Directory, string, string) error
	bootstrapEnrollmentCommandsProbe func(context.Context, *safefs.Directory, string, string) error
	bootstrapProbe                   func(context.Context, *safefs.Directory, string, string) error
	commandRecoveryProbe             func(context.Context, *safefs.Directory, string, string) error
	telegramHandoffProbe             func(context.Context, *safefs.Directory, string, string) error
	commandPolicyProbe               func(context.Context, *safefs.Directory, string, string) error
	commandProbe                     func(context.Context, *safefs.Directory, string, string) error
	deliveryRecoveryProbe            func(context.Context, *safefs.Directory, string, string) error
	deliveryProbe                    func(context.Context, *safefs.Directory, string, string) error
	deliveryInspect                  func(context.Context, ManagedManifest, bool) error
	services                         managedServices
	signalProbe                      func(context.Context, *safefs.Directory, string, string) error
	signalProfilesProbe              func(context.Context, *safefs.Directory, string, string) error
	signalCheck                      func(context.Context, ManagedManifest) error
	probe                            func(context.Context, *safefs.Directory, string, string, bool) error
	nextRecoveryProbe                func(context.Context, *safefs.Directory, string, string) error
	recoveryProbe                    func(context.Context, *safefs.Directory, string, string) error
	helperProbe                      func(context.Context, *safefs.Directory, string, string) error
	telegramProbe                    func(context.Context, *safefs.Directory, string, string) error
	telegramTransport                http.RoundTripper
	ledEndpointProbe                 func(context.Context, *safefs.Directory, string, string) error
	ledOutputProbe                   func(context.Context, *safefs.Directory, string, string) error
	ledProbe                         func(context.Context, *safefs.Directory, string, string) error
	ledDeliveryProbe                 func(context.Context, *safefs.Directory, string, string) error
	ledAPI                           func(string) worker.LEDAPI
	fault                            func(string) error
}

func newManagedInstaller() managedInstaller {
	return managedInstaller{services: userManagedServices{}, probe: probeManagedBinary}
}
func (i managedInstaller) boundary(name string) error {
	if i.fault != nil {
		return i.fault(name)
	}
	return nil
}

func managedUnitDirectory(path string) (*safefs.Directory, error) {
	d, err := privateManagedDir(path, false)
	if err != nil {
		return nil, err
	}
	sub, e := d.Sub(managedUnit + ".d")
	if e == nil {
		sub.Close()
		d.Close()
		return nil, fmt.Errorf("remove unowned service drop-ins before installation")
	}
	if !os.IsNotExist(e) {
		d.Close()
		return nil, e
	}
	return d, nil
}

func managedUnitBytes(d *safefs.Directory) ([]byte, bool, error) {
	b, err := ownedManagedFile(d, managedUnit, 1<<20)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (i managedInstaller) prepare(ctx context.Context, operation string, spec ManagedSpec) (ManagedManifest, error) {
	return i.prepareExpected(ctx, operation, spec, "")
}

// expected pins the complete manifest reviewed by a retained-release rollback.
// The operation lease protects the check through durable intent publication.
func (i managedInstaller) prepareExpected(ctx context.Context, operation string, spec ManagedSpec, expected string) (ManagedManifest, error) {
	var zero ManagedManifest
	if !isManagedPolicyOperation(operation) && operation != "enroll-bootstrap" && operation != "setup" && operation != "update" && operation != "migrate-schema" && operation != "migrate-led" && operation != "adopt-recovery" && operation != "upgrade-helper" && operation != "adopt-signal" && operation != "supervise-signal" && operation != "migrate-signal-profiles" {
		return zero, fmt.Errorf("invalid preparation operation")
	}
	nextSignal := operation == "adopt-recovery" && spec.SignalVerification != nil
	if !nextSignal && (operation == "adopt-signal" || operation == "supervise-signal") != (spec.SignalVerification != nil) {
		return zero, fmt.Errorf("adopt-signal and supervise-signal require signal_verification")
	}
	if len(spec.SignalConversations) != 0 && operation != "migrate-signal-profiles" {
		return zero, fmt.Errorf("Signal conversation enrollment requires migrate-signal-profiles")
	}
	if err := spec.validate(); err != nil {
		return zero, err
	}
	if (operation == "adopt-recovery") != (spec.RecoveryCommitHash != "") {
		return zero, fmt.Errorf("only adopt-recovery requires recovery_commit_hash in its spec")
	}
	units, err := managedUnitDirectory(spec.UnitDirectory)
	if err != nil {
		return zero, err
	}
	defer units.Close()
	raw, err := readManagedInput(spec.RuntimeFile, safefs.MaxRecordBytes)
	if err != nil {
		return zero, err
	}
	c, err := decodeRuntime(raw)
	if err != nil {
		return zero, err
	}
	if spec.FreshSignal != nil {
		var native FreshSignalManifest
		native, err = InspectFreshSignal(ctx, *spec.FreshSignal, false)
		if err == nil {
			err = validateFreshSignalGatewayManifestPolicy(native, spec.Root, spec.State, spec.UnitDirectory, c, operation != "bootstrap-providers" && operation != "enrolled-providers")
		}
		if err != nil {
			return zero, err
		}
	}
	if operation == "setup" {
		if err = verifyReplacementEnrollment(spec, c); err != nil {
			return zero, err
		}
	}
	binary, err := readManagedInput(spec.BinaryFile, maxManagedBinary)
	if err != nil {
		return zero, err
	}
	if managedDigest(binary) != spec.BinarySHA256 {
		return zero, fmt.Errorf("input executable does not match requested SHA256")
	}
	if err = checkManagedELF(binary); err != nil {
		return zero, err
	}
	var d *safefs.Directory
	var before *ManagedManifest
	var lock *safefs.Lock
	var parent *safefs.Directory
	var staging string
	if operation == "setup" {
		parent, err = privateManagedDir(filepath.Dir(spec.Root), false)
		if err != nil {
			return zero, err
		}
		defer parent.Close()
		if f, e := parent.Sub(filepath.Base(spec.Root)); e == nil {
			f.Close()
			return zero, fmt.Errorf("installation root already exists; use resume")
		} else if !os.IsNotExist(e) {
			return zero, e
		}
		id, e := safefs.NewID("n")
		if e != nil {
			return zero, e
		}
		staging = ".notification-" + id
		if err = unix.Mkdirat(int(parent.File().Fd()), staging, 0700); err != nil {
			return zero, err
		}
		d, err = parent.Sub(staging)
		if err != nil {
			return zero, err
		}
		// A crash during preparation can leave this private sibling, but never a
		// partial installation at Root or an installed service.
	} else {
		d, err = privateManagedDir(spec.Root, true)
		if err != nil {
			return zero, err
		}
	}
	defer d.Close()
	lock, err = d.Lock("operation.lock", true, operation == "setup")
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	if err = requireManagedNotRetired(d); err != nil {
		return zero, err
	}
	if operation == "setup" {
		lease, e := d.Lock("installation.lock", true, true)
		if e != nil {
			return zero, e
		}
		lease.Close()
	} else {
		j, e := readManagedJournal(d)
		if e != nil {
			return zero, e
		}
		if j.Phase != "complete" {
			return zero, fmt.Errorf("unfinished operation; resume it before updating")
		}
		if e = validateManagedPointer(d, j, true); e != nil {
			return zero, e
		}
		b := j.Intent.After
		if operation != "adopt-recovery" {
			if e = refuseManagedRecoveryPreparation(d, b); e != nil {
				return zero, e
			}
		}
		if b.Mode == "delivery" {
			return zero, fmt.Errorf("deactivate delivery before changing the installation")
		}
		if b.DeliveryOwnerHash != "" && operation != "update" && operation != "upgrade-helper" && operation != "adopt-recovery" {
			return zero, fmt.Errorf("delivery-enrolled state requires an explicit future migration")
		}
		before = &b
		if b.RecoveryCommitHash != "" && (operation == "migrate-schema" || operation == "migrate-led") {
			return zero, fmt.Errorf("recovered runtime migration requires ancestor support")
		}
		if (operation == "adopt-recovery" || operation == "upgrade-helper") && b.Mode != "verification" {
			return zero, fmt.Errorf("helper upgrade/recovery adoption requires an installed verification service")
		}
		if protocol.Hash(b.FreshSignal) != protocol.Hash(spec.FreshSignal) {
			return zero, fmt.Errorf("fresh Signal ownership must remain explicit and unchanged")
		}
		if expected != "" && protocol.Hash(b) != expected {
			return zero, fmt.Errorf("installation changed while preparing rollback; review the current revision")
		}
		if b.Root != spec.Root || b.Paths.State != spec.State || b.UnitDirectory != spec.UnitDirectory || (!isManagedPolicyOperation(operation) && operation != "enroll-bootstrap" && operation != "migrate-led" && operation != "adopt-signal" && !nextSignal && b.ConfigHash != protocol.Hash(c)) {
			return zero, fmt.Errorf("update changes identity, configuration or schema; explicit migration required")
		}
		if operation == "migrate-schema" {
			if b.Schema != 9 || worker.StorageSchemaVersion() != 10 || b.Mode != "verification" {
				return zero, fmt.Errorf("only an installed verification schema 9 -> 10 migration is supported")
			}
		} else if b.Schema != worker.StorageSchemaVersion() {
			return zero, fmt.Errorf("schema change requires migrate-schema")
		}
		var successor *SignalVerificationReference
		if (operation == "supervise-signal" || nextSignal) && b.SignalVerification != nil && b.SignalVerification.ServiceRoot != "" {
			successor = spec.SignalVerification
		}
		if _, e = verifyManagedArtifactsPolicy(d, b, operation == "migrate-schema", "", successor); e != nil {
			return zero, e
		}
		if operation == "adopt-recovery" && b.SignalVerification != nil && !nextSignal {
			return zero, fmt.Errorf("later Signal recovery requires the successor endpoint reference")
		}
		if nextSignal {
			if _, e = validateManagedNextSignalChange(b, c, raw, *spec.SignalVerification, spec.RecoveryCommitHash); e != nil {
				return zero, e
			}
		}
		if operation == "adopt-signal" || operation == "supervise-signal" {
			if _, e = validateManagedSignalTransition(operation, b, c, raw, *spec.SignalVerification); e != nil {
				return zero, e
			}
		}
		if isManagedPolicyOperation(operation) {
			previous, e := managedRuntime(d, b)
			if e != nil {
				return zero, e
			}
			if e = validateManagedBootstrapPolicy(b, previous, c, operation); e != nil {
				return zero, e
			}
			preflight := worker.PreflightBootstrapLimits
			if operation == "enrolled-command-activity" {
				preflight = worker.PreflightEnrolledCommandActivity
			}
			if operation == "enrolled-command-policy" {
				preflight = worker.PreflightEnrolledCommandPolicy
			}
			if operation == "enrolled-commands" {
				preflight = worker.PreflightEnrolledCommands
			}
			if operation == "enrolled-providers" {
				preflight = worker.PreflightEnrolledProviders
			}
			if operation == "enrolled-producer" {
				preflight = worker.PreflightEnrolledProducerPolicy
			}
			if operation == "enrolled-routes" {
				preflight = worker.PreflightEnrolledRoutes
			}
			if operation == "enrolled-freshness" {
				preflight = worker.PreflightEnrolledFreshness
			}
			if operation == "enrolled-limits" {
				preflight = worker.PreflightEnrolledLimits
			}
			if operation == "bootstrap-routes" || operation == "bootstrap-providers" {
				preflight = worker.PreflightBootstrapRoutes
			}
			if e = preflight(ctx, b.Paths.State, b.Identity, previous.Worker, c.Worker); e != nil {
				return zero, e
			}
		}
		if operation == "enroll-bootstrap" {
			previous, e := managedRuntime(d, b)
			if e != nil {
				return zero, e
			}
			if e = validateManagedBootstrapChange(b, previous, c); e != nil {
				return zero, e
			}
			var outputOperation string
			var outputDisabled bool
			if b.LEDOutput != nil {
				outputOperation, outputDisabled = b.LEDOutput.Operation, b.LEDOutput.Disabled
			}
			if e = worker.PreflightBootstrapEnrollment(ctx, b.Paths.State, b.Identity, previous.Worker, c.Worker, outputOperation, outputDisabled); e != nil {
				return zero, e
			}
		}
		if operation == "migrate-led" {
			if b.Mode != "verification" {
				return zero, fmt.Errorf("LED migration requires an installed verification service")
			}
			previous, e := managedRuntime(d, b)
			if e != nil {
				return zero, e
			}
			if e = validateManagedLEDChange(previous, c); e != nil {
				return zero, e
			}
			if e = worker.PreflightManagedLEDBindings(ctx, b.Paths.State, previous.Worker, c.Worker, b.Identity); e != nil {
				return zero, e
			}
		}
	}
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return zero, err
	}
	if before == nil || before.Mode == "uninstalled" {
		if present {
			return zero, fmt.Errorf("refusing to replace an unowned unit")
		}
		s, e := i.services.Inspect(ctx)
		if e != nil {
			return zero, e
		}
		if s.Load != "not-found" || s.Active != "inactive" || s.Fragment != "" || s.DropIns != "" {
			return zero, fmt.Errorf("service name is already owned in user manager")
		}
	} else {
		if !present || managedDigest(unit) != before.UnitHash {
			return zero, fmt.Errorf("installed unit changed")
		}
		if _, err = inspectManagedService(ctx, i.services, *before, false); err != nil {
			return zero, err
		}
	}
	// Setup adopts only an existing inactive store. Update can stage artifacts
	// while its runtime owns the store; resume verifies identity after stopping it.
	var identity protocol.Identity
	if before == nil {
		stateDir, e := privateManagedDir(spec.State, true)
		if e != nil {
			return zero, e
		}
		e = stateDir.ReadJSON("identity.json", &identity)
		stateDir.Close()
		if e != nil {
			return zero, e
		}
		var s *worker.Store
		if c.Bootstrap {
			s, e = worker.OpenBootstrapVerification(spec.State, c.Worker, identity, "")
		} else {
			s, e = worker.OpenManagedVerification(spec.State, c.Worker, identity)
		}
		if e != nil {
			return zero, e
		}
		identity = s.Identity()
		e = s.RequireInactiveProducers()
		closeErr := s.Close()
		if e != nil {
			return zero, e
		}
		if closeErr != nil {
			return zero, closeErr
		}
	} else {
		identity = before.Identity
		if operation == "adopt-recovery" {
			review, e := worker.ReviewPublishedRecovery(ctx, spec.State, c.Worker, spec.RecoveryCommitHash)
			if e != nil {
				return zero, e
			}
			if review.Before != before.Identity {
				return zero, fmt.Errorf("publication does not recover this installation")
			}
			identity = review.After
		}
	}
	revision, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	m := ManagedManifest{FreshSignal: spec.FreshSignal, Version: managedVersion, Installation: id, Revision: revision, Root: spec.Root, UnitDirectory: spec.UnitDirectory, Paths: ServicePaths{Executable: filepath.Join(spec.Root, "releases", revision, "deploy"), Config: filepath.Join(spec.Root, "releases", revision, "runtime.json"), State: spec.State, Socket: filepath.Join(spec.State, "api.sock"), DeniedPaths: spec.DeniedPaths}, Identity: identity, RuntimeHash: managedDigest(raw), ConfigHash: protocol.Hash(c), BinaryHash: spec.BinarySHA256, HelperHash: spec.BinarySHA256, Schema: worker.StorageSchemaVersion(), Mode: "verification"}
	if before != nil {
		m.Installation = before.Installation
		m.HelperHash = before.HelperHash
		m.HelperRevision = before.HelperRevision
		m.RecoveryCommitHash = before.RecoveryCommitHash
		m.TelegramRenewalID = before.TelegramRenewalID
		m.LEDOutput = before.LEDOutput
		m.SignalVerification = before.SignalVerification
		m.SignalProfiles = before.SignalProfiles
		m.DeliveryOwnerHash = before.DeliveryOwnerHash
		m.DeliveryOwnerConfigHash = before.DeliveryOwnerConfigHash
		m.ReportCredentialHistory = before.ReportCredentialHistory
		m.CommandHistoryHash = before.CommandHistoryHash
		m.DeliveryRecovery = before.DeliveryRecovery
	}
	var profileManifest signalProfilesManifest
	var profileRuntime []byte
	var profileOperation string
	if operation == "migrate-signal-profiles" {
		profileOperation, err = safefs.NewID("n")
		if err != nil {
			return zero, err
		}
		if before == nil || before.SignalVerification == nil {
			return zero, fmt.Errorf("Signal profile migration requires an adopted endpoint")
		}
		endpoint, e := readManagedSignalBinding(d, *before)
		if e != nil {
			return zero, e
		}
		profileManifest, profileRuntime, err = prepareSignalProfilesEnrolled(*before, raw, endpoint, profileOperation, spec.SignalConversations)
		if err != nil {
			return zero, err
		}
		m.SignalProfiles = &ManagedSignalProfiles{Operation: profileOperation, Hash: protocol.Hash(profileManifest), Conversations: len(profileManifest.Conversations) != 0}
	}
	var signalEndpoint SignalVerificationManifest
	var signalOperation string
	if operation == "adopt-signal" || operation == "supervise-signal" || nextSignal {
		signalOperation, err = safefs.NewID("n")
		if err != nil {
			return zero, err
		}
		m.SignalVerification = &ManagedSignalBinding{SignalVerificationReference: *spec.SignalVerification, Operation: signalOperation}
		if nextSignal {
			signalEndpoint, err = validateManagedNextSignalChange(*before, c, raw, *spec.SignalVerification, spec.RecoveryCommitHash)
		} else {
			signalEndpoint, err = validateManagedSignalTransition(operation, *before, c, raw, *spec.SignalVerification)
		}
		if err != nil {
			return zero, err
		}
	}
	if m.SignalVerification != nil {
		p, e := inspectSignalVerificationPublication(m.SignalVerification.SignalVerificationReference)
		if e != nil {
			return zero, e
		}
		if managedDigest(raw) != p.NextGatewaySHA256 {
			return zero, fmt.Errorf("Signal verification configuration bytes changed")
		}
	}
	if operation == "adopt-recovery" {
		m.RecoveryCommitHash = spec.RecoveryCommitHash
		m.TelegramRenewalID = ""
		if before.DeliveryOwnerHash != "" {
			m.DeliveryRecovery = &worker.ManagedDeliveryRecovery{Version: 1, Owner: deliveryOwner(*before, c), Before: before.Identity, After: m.Identity, CommitHash: m.RecoveryCommitHash}
			if before.DeliveryRecovery != nil {
				m.DeliveryRecovery.Version = 2
			}
		}
	}
	if nextSignal && before.SignalProfiles != nil {
		profileOperation = signalOperation
		profileManifest, profileRuntime, err = prepareNextSignalProfiles(d, *before, m, raw, signalEndpoint, profileOperation)
		if err != nil {
			return zero, err
		}
		m.SignalProfiles = &ManagedSignalProfiles{Operation: profileOperation, Hash: protocol.Hash(profileManifest), Conversations: before.SignalProfiles.Conversations}
	}
	if operation == "adopt-recovery" && before.CommandHistoryHash != "" {
		r, e := commandRecoveryBinding(*before, m)
		if e != nil {
			return zero, e
		}
		m.CommandHistoryHash = protocol.Hash(r)
	}
	if isManagedPolicyOperation(operation) || operation == "enroll-bootstrap" || operation == "migrate-schema" || operation == "migrate-led" || operation == "adopt-recovery" || operation == "upgrade-helper" || operation == "adopt-signal" || operation == "supervise-signal" || operation == "migrate-signal-profiles" {
		m.HelperRevision = m.Revision
		m.HelperHash = m.BinaryHash
	}
	body, err := renderManagedUnit(m)
	if err != nil {
		return zero, err
	}
	m.UnitHash = managedDigest([]byte(body))
	if err = m.validate(); err != nil {
		return zero, err
	}
	if err = d.Capacity(int64(len(binary))*2 + 32<<20); err != nil {
		return zero, err
	}
	if err = limitManagedHistory(d); err != nil {
		return zero, err
	}
	if err = d.Mkdir("releases/" + revision); err != nil {
		return zero, err
	}
	release, err := d.Sub("releases/" + revision)
	if err != nil {
		return zero, err
	}
	err = frozenManagedFile(release, "deploy", binary, 0700)
	if err == nil {
		err = frozenManagedFile(release, "runtime.json", raw, 0600)
	}
	if err == nil && (isManagedPolicyOperation(operation) || operation == "enroll-bootstrap" || operation == "migrate-schema" || operation == "migrate-led" || operation == "adopt-recovery" || operation == "upgrade-helper" || operation == "adopt-signal" || operation == "supervise-signal" || operation == "migrate-signal-profiles") {
		err = frozenManagedFile(release, "recovery-helper", binary, 0700)
	}
	release.Close()
	if err != nil {
		return zero, err
	}
	if before == nil {
		if err = frozenManagedFile(d, managedHelperName(m), binary, 0700); err != nil {
			return zero, err
		}
	}
	if err = i.probe(ctx, d, "releases/"+revision+"/deploy", m.BinaryHash, m.HelperRevision != ""); err != nil {
		return zero, err
	}
	if err = i.probe(ctx, d, managedHelperName(m), m.HelperHash, m.HelperRevision != ""); err != nil {
		return zero, err
	}
	if operation == "migrate-led" {
		if err = i.probeLEDMapping(ctx, d, m); err != nil {
			return zero, err
		}
	}
	if err = i.probeRecovery(ctx, d, m); err != nil {
		return zero, err
	}
	if operation == "upgrade-helper" {
		if err = i.probeHelperUpgrade(ctx, d, m, c); err != nil {
			return zero, err
		}
	}
	if err = i.probeTelegramRenewal(ctx, d, m); err != nil {
		return zero, err
	}
	if c.Bootstrap || operation == "enroll-bootstrap" {
		probe := i.bootstrapProbe
		if probe == nil {
			probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
				return probeManagedCapability(ctx, d, name, hash, "__managed-bootstrap-probe")
			}
		}
		for _, binary := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
			if err = probe(ctx, d, binary.name, binary.hash); err != nil {
				return m, err
			}
		}
	}
	if err = i.requireBootstrapEnrollmentLEDCapability(ctx, d, m, operation == "enroll-bootstrap" && m.LEDOutput != nil); err != nil {
		return m, err
	}
	if err = i.requireBootstrapEnrollmentCommandsCapability(ctx, d, m, operation == "enroll-bootstrap" && c.Worker.Interaction != nil); err != nil {
		return m, err
	}
	if err = i.requireBootstrapProvidersCapability(ctx, d, m, operation); err != nil {
		return m, err
	}
	if err = i.requireEnrolledLimitsCapability(ctx, d, m, (operation == "enrolled-limits" || operation == "enrolled-freshness" || operation == "enrolled-routes" || operation == "enrolled-producer" || operation == "enrolled-providers" || operation == "enrolled-command-activity" || operation == "enrolled-command-policy" || operation == "enrolled-commands")); err != nil {
		return zero, err
	}
	if err = i.requireBootstrapLimitsCapability(ctx, d, m, isManagedPolicyOperation(operation)); err != nil {
		return m, err
	}
	if operation == "bootstrap-providers" || operation == "enrolled-providers" {
		if err = i.probeSignalDelivery(ctx, d, m, c); err != nil {
			return zero, err
		}
	}
	if err = i.probeLEDEndpoint(ctx, d, m, c); err != nil {
		return m, err
	}
	if err = i.requireLEDMappingOutputCapability(ctx, d, m); err != nil {
		return zero, err
	}
	if err = i.probeLEDOutput(ctx, d, m); err != nil {
		return m, err
	}
	if err = i.probeSignalVerification(ctx, d, m); err != nil {
		return zero, err
	}
	if err = i.probeSignalProfiles(ctx, d, m); err != nil {
		return zero, err
	}
	if err = i.probeDelivery(ctx, d, m); err != nil {
		return zero, err
	}
	if err = i.checkSignalVerification(ctx, m); err != nil {
		return zero, err
	}
	opid, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	if operation == "migrate-signal-profiles" || (nextSignal && before.SignalProfiles != nil) {
		opid = profileOperation
		profileRelease, e := d.Sub("releases/" + revision)
		if e != nil {
			return zero, e
		}
		err = profileRelease.WriteJSON("signal-profiles.json", profileManifest, false)
		if err == nil {
			err = frozenManagedFile(profileRelease, "signal-runtime.json", profileRuntime, 0600)
		}
		profileRelease.Close()
		if err != nil {
			return zero, err
		}
		if err = i.boundary("signal-profiles-retained"); err != nil {
			return zero, err
		}
	}
	if operation == "adopt-signal" || operation == "supervise-signal" || nextSignal {
		opid = signalOperation
		if err = d.Mkdir("operations/" + opid); err != nil {
			return zero, err
		}
		if err = d.WriteJSON("operations/"+opid+"/signal-endpoint.json", signalEndpoint, false); err != nil {
			return zero, err
		}
		if err = i.boundary("signal-endpoint-retained"); err != nil {
			return zero, err
		}
	}
	if before != nil && c.Providers["signal"].SignalDelivery != nil {
		if anchor := stagedProviderCredentialHistory(managedIntent{ID: opid, Operation: operation, Before: before, After: m}); anchor != nil {
			m.ReportCredentialHistory = anchor
		}
	}
	j := managedJournal{Intent: managedIntent{Version: managedVersion, ID: opid, Operation: operation, Before: before, After: m, BeforeUnit: string(unit), AfterUnit: body}, Phase: "prepared"}
	if err = writeManagedIntent(d, j); err != nil {
		return zero, err
	}
	if parent != nil {
		if err = parent.Rename(staging, parent, filepath.Base(spec.Root)); err != nil {
			return zero, err
		}
	}
	if err = i.boundary("prepared"); err != nil {
		return zero, err
	}
	return i.resumeLocked(ctx, d, j, lock)
}

func limitManagedHistory(d *safefs.Directory) error { return limitManagedHistoryFor(d, false) }
func limitManagedHistoryFor(d *safefs.Directory, terminal bool) error {
	for _, name := range []string{"operations", "releases"} {
		items, err := d.Sub(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		names, err := items.File().Readdirnames(36)
		items.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(names) >= 32 && !(terminal && ((name == "releases" && len(names) == 32) || (name == "operations" && len(names) < 35))) {
			return fmt.Errorf("installation history limit reached; offline archival is required")
		}
	}
	return nil
}

func validateManagedPointer(d *safefs.Directory, j managedJournal, requireAfter bool) error {
	var current ManagedManifest
	err := d.ReadJSON("installation.json", &current)
	if previous := j.Intent.Supersedes; previous != nil && !requireAfter {
		if os.IsNotExist(err) && previous.Before == nil {
			return nil
		}
		if err != nil {
			return err
		}
		if protocol.Hash(current) == protocol.Hash(previous.After) || (previous.Before != nil && protocol.Hash(current) == protocol.Hash(*previous.Before)) {
			return nil
		}
	}
	if os.IsNotExist(err) && j.Intent.Before == nil && !requireAfter {
		return nil
	}
	if err != nil {
		return err
	}
	if protocol.Hash(current) == protocol.Hash(j.Intent.After) {
		return nil
	}
	if !requireAfter && j.Intent.Before != nil && protocol.Hash(current) == protocol.Hash(*j.Intent.Before) {
		return nil
	}
	return fmt.Errorf("installation pointer does not match operation")
}

func (i managedInstaller) resume(ctx context.Context, root string) (ManagedManifest, error) {
	var zero ManagedManifest
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
	if j.Intent.After.Root != root {
		return zero, fmt.Errorf("installation root moved")
	}
	return i.resumeLocked(ctx, d, j, lock)
}

func (i managedInstaller) uninstall(ctx context.Context, root string) (ManagedManifest, error) {
	return i.uninstallExpected(ctx, root, "")
}

func (i managedInstaller) uninstallExpected(ctx context.Context, root, expected string) (ManagedManifest, error) {
	var zero ManagedManifest
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
	if j.Intent.After.Root != root {
		return zero, fmt.Errorf("installation root moved")
	}
	if expected != "" && protocol.Hash(j.Intent.After) != expected {
		if j.Intent.Operation != "uninstall" || j.Intent.Before == nil || protocol.Hash(*j.Intent.Before) != expected {
			return zero, fmt.Errorf("installation changed before uninstall")
		}
	}
	interrupted := j.Phase != "complete"
	if interrupted && (isManagedPolicyOperation(j.Intent.Operation) || j.Intent.Operation == "set-led-output" || j.Intent.Operation == "enroll-bootstrap" || j.Intent.Operation == "migrate-schema" || j.Intent.Operation == "migrate-led" || j.Intent.Operation == "adopt-recovery" || j.Intent.Operation == "renew-telegram" || j.Intent.Operation == "upgrade-helper" || j.Intent.Operation == "adopt-signal" || j.Intent.Operation == "supervise-signal" || j.Intent.Operation == "migrate-signal-profiles") {
		return zero, fmt.Errorf("resume the pending migration before uninstalling")
	}
	if interrupted && j.Intent.Operation == "uninstall" {
		return i.resumeLocked(ctx, d, j)
	}
	if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return zero, err
	}
	b := j.Intent.After
	if b.Mode == "delivery" {
		return zero, fmt.Errorf("deactivate delivery before uninstalling")
	}
	if b.Mode == "uninstalled" {
		return i.resumeLocked(ctx, d, j)
	}
	c, err := verifyManagedArtifacts(d, b)
	if err != nil {
		return zero, err
	}
	if c.Bootstrap && c.LEDSocket != "" && (b.LEDOutput == nil || !b.LEDOutput.Disabled) {
		return zero, fmt.Errorf("disable owned LED output before removing the unbound gateway")
	}
	units, err := managedUnitDirectory(b.UnitDirectory)
	if err != nil {
		return zero, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return zero, err
	}
	if interrupted {
		if present && string(unit) != j.Intent.BeforeUnit && string(unit) != j.Intent.AfterUnit {
			return zero, fmt.Errorf("interrupted managed unit changed")
		}
	} else if !present || managedDigest(unit) != b.UnitHash {
		return zero, fmt.Errorf("managed unit changed")
	}
	if _, err = inspectManagedService(ctx, i.services, b, interrupted); err != nil {
		return zero, err
	}
	after := b
	after.Mode = "uninstalled"
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	var previous *managedIntent
	if interrupted {
		p := j.Intent
		previous = &p
	}
	j = managedJournal{Intent: managedIntent{Version: managedVersion, ID: id, Operation: "uninstall", Before: &b, After: after, BeforeUnit: string(unit), Supersedes: previous}, Phase: "prepared"}
	if err = limitManagedHistoryFor(d, true); err != nil {
		return zero, err
	}
	if err = writeManagedIntent(d, j); err != nil {
		return zero, err
	}
	if err = i.boundary("prepared"); err != nil {
		return zero, err
	}
	return i.resumeLocked(ctx, d, j)
}

func (i managedInstaller) resumeLocked(ctx context.Context, d *safefs.Directory, j managedJournal, operationLeases ...*safefs.Lock) (ManagedManifest, error) {
	m := j.Intent.After
	if err := checkManagedDirectoryPath(d, m.Root, true); err != nil {
		return m, err
	}
	if err := j.validate(); err != nil {
		return m, err
	}
	if err := validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return m, err
	}
	if sharedActivityOperation(j.Intent.Operation) {
		if err := i.probeSharedActivity(ctx, d, m); err != nil {
			return m, err
		}
		if _, err := sharedActivityPredecessor(d, j); err != nil {
			return m, err
		}
		if m.DeliveryOwnerHash != "" {
			if _, err := sharedActivityPause(d, j); err != nil {
				return m, err
			}
		}
	}
	if err := i.stageSignalDeliveryIntent(ctx, d, j); err != nil {
		return m, err
	}
	var c Runtime
	var err error
	// Before publication a renewal's receipt may not exist yet. Artifact
	// validation admits only this exact unfinished operation as that exception.
	if j.Intent.Operation == "renew-telegram" && managedPhase(j.Phase) < managedPhase("published") {
		c, err = verifyManagedArtifactsPendingTelegram(d, m, j.Intent.ID)
	} else {
		c, err = verifyManagedArtifacts(d, m)
	}
	if err != nil {
		return m, err
	}
	if isPausedRuntimeChange(j.Intent.Operation) {
		if _, err = deliveryProviderPause(d, j, c); err != nil {
			return m, err
		}
		if _, err = retainedDeliveryCredentials(d, j, c); err != nil {
			return m, err
		}
		if err = i.probePausedRuntimeChange(ctx, d, m, j.Intent.DeliveryProvider.Kind); err != nil {
			return m, err
		}
	}
	// Completed uninstall changes only Mode (proved by journal validation).
	// Its After artifacts already validate the identical retained installation;
	// revalidating Before as live would incorrectly require removed dependencies.
	if j.Intent.Before != nil && !(j.Intent.Operation == "uninstall" && j.Phase == "complete") {
		var successor *SignalVerificationReference
		if (j.Intent.Operation == "supervise-signal" || j.Intent.Operation == "adopt-recovery") && j.Intent.Before.SignalVerification != nil && j.Intent.Before.SignalVerification.ServiceRoot != "" {
			successor = &m.SignalVerification.SignalVerificationReference
		}
		if _, err = verifyManagedArtifactsPolicy(d, *j.Intent.Before, j.Intent.Operation == "migrate-schema", "", successor); err != nil {
			return m, err
		}
	}
	if isManagedPolicyOperation(j.Intent.Operation) {
		if _, err = managedBootstrapLimitsPair(d, j, c); err != nil {
			return m, err
		}
	}
	if m.Mode != "uninstalled" {
		if err = i.requireLEDMappingOutputCapability(ctx, d, m); err != nil {
			return m, err
		}
		if err = i.requireBootstrapEnrollmentLEDCapability(ctx, d, m, j.Intent.Operation == "enroll-bootstrap" && m.LEDOutput != nil); err != nil {
			return m, err
		}
		if err = i.requireBootstrapEnrollmentCommandsCapability(ctx, d, m, j.Intent.Operation == "enroll-bootstrap" && c.Worker.Interaction != nil); err != nil {
			return m, err
		}
		if err = i.requireBootstrapProvidersCapability(ctx, d, m, j.Intent.Operation); err != nil {
			return m, err
		}
		if err = i.requireEnrolledLimitsCapability(ctx, d, m, (j.Intent.Operation == "enrolled-limits" || j.Intent.Operation == "enrolled-freshness" || j.Intent.Operation == "enrolled-routes" || j.Intent.Operation == "enrolled-producer" || j.Intent.Operation == "enrolled-providers" || j.Intent.Operation == "enrolled-command-activity" || j.Intent.Operation == "enrolled-command-policy" || j.Intent.Operation == "enrolled-commands")); err != nil {
			return m, err
		}
		if err = i.requireBootstrapLimitsCapability(ctx, d, m, isManagedPolicyOperation(j.Intent.Operation)); err != nil {
			return m, err
		}
	}
	if j.Intent.Operation == "enroll-bootstrap" {
		if _, err = managedBootstrapPair(d, j, c); err != nil {
			return m, err
		}
	}
	if j.Intent.Operation == "migrate-led" {
		if _, err = managedLEDPair(d, j, c); err != nil {
			return m, err
		}
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return m, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return m, err
	}
	before := []byte(j.Intent.BeforeUnit)
	after := []byte(j.Intent.AfterUnit)
	if present && !bytes.Equal(unit, before) && !bytes.Equal(unit, after) {
		return m, fmt.Errorf("foreign unit content; refusing lifecycle operation")
	}
	if !present && len(before) > 0 && m.Mode != "uninstalled" {
		return m, fmt.Errorf("installed unit is missing")
	}
	if j.Phase == "complete" {
		if m.Mode == "delivery" {
			if err = checkDeliveryAdmission(d, m, c, time.Now().UTC()); err != nil {
				return m, err
			}
			if err = i.inspectDelivery(ctx, m, false); err != nil {
				return m, err
			}
		}
		if j.Intent.Operation == "migrate-led" {
			if err = verifyManagedLEDCompletion(ctx, d, j, c); err != nil {
				return m, err
			}
		}
		if j.Intent.Operation == "migrate-schema" {
			if err = worker.VerifyManagedSchemaMigration(ctx, m.Paths.State, c.Worker, m.Identity, j.Intent.ID, j.Intent.Before.Schema); err != nil {
				return m, err
			}
		}
		if m.Mode == "uninstalled" {
			if present {
				return m, fmt.Errorf("uninstalled service reappeared")
			}
			var state managedServiceState
			state, err = inspectManagedService(ctx, i.services, m, true)
			if err == nil && state.Load != "not-found" {
				err = fmt.Errorf("uninstalled service remains loaded")
			}
			return m, err
		}
		if !present || !bytes.Equal(unit, after) {
			return m, fmt.Errorf("completed unit changed")
		}
		if _, err = inspectManagedService(ctx, i.services, m, false); err != nil {
			return m, err
		}
		if err = i.checkSignalVerification(ctx, m); err != nil {
			return m, err
		}
		return m, i.services.Verify(ctx, m)
	}
	if err = i.probeLEDEndpoint(ctx, d, m, c); err != nil {
		return m, err
	}
	if err = i.probeLEDOutput(ctx, d, m); err != nil {
		return m, err
	}
	if err = i.probeSignalVerification(ctx, d, m); err != nil {
		return m, err
	}
	if err = i.probeSignalProfiles(ctx, d, m); err != nil {
		return m, err
	}
	if err = i.probeDelivery(ctx, d, m); err != nil {
		return m, err
	}
	if m.DeliveryOwnerHash != "" && !sharedActivityOperation(j.Intent.Operation) {
		if err = i.inspectDeliveryTransition(ctx, j); err != nil {
			return m, err
		}
	}
	if m.Mode != "uninstalled" {
		if err = i.checkSignalVerification(ctx, m); err != nil {
			return m, err
		}
	}
	state, err := inspectManagedService(ctx, i.services, m, true)
	if err != nil {
		return m, err
	}
	// A policy transition has already committed its database and published its
	// exact runtime before reloaded. A lost start acknowledgement may leave that
	// revision running. Verify it through the normal health/identity checks before
	// recording completion, rather than stopping and starting it a second time.
	if (isManagedPolicyOperation(j.Intent.Operation) || isPausedRuntimeChange(j.Intent.Operation)) && managedPhase(j.Phase) >= managedPhase("reloaded") && m.Mode == "verification" && present && bytes.Equal(unit, after) && state.Load == "loaded" && state.Active == "active" && state.UnitFile == "enabled" {
		if err = i.services.Verify(ctx, m); err != nil {
			return m, err
		}
		if err = i.advance(d, &j, "started"); err != nil {
			return m, err
		}
		return m, i.advance(d, &j, "complete")
	}
	// Only stop a unit that the manifest pins. An initial cached not-found unit
	// has no service to stop. Stop completion is verified before taking leases.
	if state.Load != "not-found" {
		if err = i.services.Stop(ctx); err != nil {
			return m, err
		}
	}
	if err = i.boundary("stop-returned"); err != nil {
		return m, err
	}
	state, err = inspectManagedService(ctx, i.services, m, true)
	if err != nil {
		return m, err
	}
	if state.Active != "inactive" && state.Active != "failed" {
		return m, fmt.Errorf("managed service has not stopped")
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return m, err
	}
	defer func() {
		if lease != nil {
			lease.Close()
		}
	}()
	if j.Intent.Operation == "rotate-command-delivery-provider" || j.Intent.Operation == "change-command-delivery-destination" || j.Intent.Operation == "change-command-delivery-routes" || j.Intent.Operation == "change-command-delivery-producer" || j.Intent.Operation == "change-command-delivery-freshness" || j.Intent.Operation == "change-command-delivery-limits" || j.Intent.Operation == "change-delivery-limits" || j.Intent.Operation == "change-delivery-producer" || j.Intent.Operation == "change-delivery-routes" || j.Intent.Operation == "change-delivery-destination" || j.Intent.Operation == "change-delivery-provider-enrollment" {
		previous, e := managedRuntime(d, *j.Intent.Before)
		if e != nil {
			return m, e
		}
		migrate := worker.MigrateDeliveryLimits
		if j.Intent.Operation == "change-delivery-destination" || j.Intent.Operation == "change-delivery-provider-enrollment" {
			migrate = worker.MigrateDeliveryProviders
		}
		if j.Intent.Operation == "change-delivery-routes" {
			migrate = worker.MigrateDeliveryRoutes
		}
		if j.Intent.Operation == "change-delivery-producer" {
			migrate = worker.MigrateDeliveryProducerPolicy
		}
		if j.Intent.Operation == "rotate-command-delivery-provider" || j.Intent.Operation == "change-command-delivery-destination" {
			_, err = worker.MigrateCommandDeliveryProviders(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID, m.CommandHistoryHash)
		} else if j.Intent.Operation == "change-command-delivery-limits" {
			_, err = worker.MigrateCommandDeliveryLimits(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID, m.CommandHistoryHash)
		} else if j.Intent.Operation == "change-command-delivery-routes" {
			_, err = worker.MigrateCommandDeliveryRoutes(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID, m.CommandHistoryHash)
		} else if j.Intent.Operation == "change-command-delivery-producer" {
			_, err = worker.MigrateCommandDeliveryProducerPolicy(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID, m.CommandHistoryHash)
		} else if j.Intent.Operation == "change-command-delivery-freshness" {
			_, err = worker.MigrateCommandDeliveryFreshness(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID, m.CommandHistoryHash)
		} else {
			_, err = migrate(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		}
		if err != nil {
			return m, err
		}
		if err = i.boundary("delivery-limits-migrated"); err != nil {
			return m, err
		}
	}
	if isManagedPolicyOperation(j.Intent.Operation) {
		previous, e := managedBootstrapLimitsPair(d, j, c)
		if e != nil {
			return m, e
		}
		migrate := worker.MigrateBootstrapLimits
		if j.Intent.Operation == "bootstrap-routes" || j.Intent.Operation == "bootstrap-providers" {
			migrate = worker.MigrateBootstrapRoutes
		}
		if j.Intent.Operation == "enrolled-command-activity" {
			_, err = worker.MigrateEnrolledCommandActivity(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		} else if j.Intent.Operation == "enrolled-command-policy" {
			_, err = worker.MigrateEnrolledCommandPolicy(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		} else if j.Intent.Operation == "enrolled-commands" {
			_, err = worker.MigrateEnrolledCommands(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		} else if j.Intent.Operation == "enrolled-providers" {
			_, err = worker.MigrateEnrolledProviders(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		} else if j.Intent.Operation == "enrolled-producer" {
			_, err = worker.MigrateEnrolledProducerPolicy(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		} else if j.Intent.Operation == "enrolled-routes" {
			_, err = worker.MigrateEnrolledRoutes(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		} else if j.Intent.Operation == "enrolled-freshness" {
			_, err = worker.MigrateEnrolledFreshness(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		} else if j.Intent.Operation == "enrolled-limits" {
			_, err = worker.MigrateEnrolledLimits(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		} else if j.Intent.Operation == "bootstrap-providers" && protocol.Hash(previous.Worker) == protocol.Hash(c.Worker) {
			// A token/reference rotation for the same bot and destination changes
			// runtime ownership, not the worker's frozen audience. Recheck the
			// stopped database without inventing a different route generation.
			err = worker.PreflightBootstrapRoutes(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker)
		} else {
			_, err = migrate(ctx, m.Paths.State, m.Identity, previous.Worker, c.Worker, j.Intent.ID)
		}
		if err != nil {
			return m, err
		}
		if err = i.boundary(j.Intent.Operation + "-migrated"); err != nil {
			return m, err
		}
	}
	if j.Intent.Operation == "enroll-bootstrap" {
		if _, err = managedBootstrapPair(d, j, c); err != nil {
			return m, err
		}
		if _, err = worker.EnrollBootstrap(ctx, m.Paths.State, m.Identity, c.Worker, j.Intent.ID); err != nil {
			return m, err
		}
		if err = i.boundary("bootstrap-enrolled"); err != nil {
			return m, err
		}
	}
	if j.Intent.Operation == "migrate-schema" {
		// Probe both new executables before touching the old database. The separately
		// retained old helper remains available for inspection of the pre-upgrade copy.
		if err = i.probe(ctx, d, managedHelperName(m), m.HelperHash, m.HelperRevision != ""); err != nil {
			return m, err
		}
		if err = i.probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash, m.HelperRevision != ""); err != nil {
			return m, err
		}
		if err = worker.MigrateManagedSchema(ctx, m.Paths.State, c.Worker, m.Identity, j.Intent.ID, j.Intent.Before.Schema); err != nil {
			return m, err
		}
		if err = i.boundary("schema-migrated"); err != nil {
			return m, err
		}
	}
	if j.Intent.Operation == "migrate-led" {
		if err = i.migrateLEDMapping(ctx, d, j, c); err != nil {
			return m, err
		}
		if err = i.boundary("led-migrated"); err != nil {
			return m, err
		}
	}
	if j.Intent.Operation == "set-led-output" {
		if err = i.configureLEDOutput(ctx, d, j, c); err != nil {
			return m, err
		}
		if err = i.boundary("led-output-complete"); err != nil {
			return m, err
		}
	}
	if j.Intent.Operation == "upgrade-helper" {
		if err = i.probeHelperUpgrade(ctx, d, m, c); err != nil {
			return m, err
		}
	}
	if err = i.adoptRecovery(ctx, d, j, c); err != nil {
		return m, err
	}
	var operationLease *safefs.Lock
	if len(operationLeases) == 1 {
		operationLease = operationLeases[0]
	}
	if err = i.probeTelegramRenewal(ctx, d, m); err != nil {
		return m, err
	}
	if err = i.renewTelegram(ctx, d, j, c, lease, operationLease); err != nil {
		return m, err
	}
	if err = i.deliveryState(ctx, d, j, c); err != nil {
		return m, err
	}
	if err = i.commandState(ctx, d, j, c); err != nil {
		return m, err
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		return m, err
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if s.Identity() != m.Identity {
		return m, fmt.Errorf("worker identity changed; recovery required")
	}
	if j.Intent.Operation == "disable-commands" || isManagedCommandPolicyOperation(j.Intent.Operation) || (j.Intent.Operation == "deactivate-delivery" && m.CommandHistoryHash != "") {
		if err = s.WaitManagedCommandGrants(ctx); err != nil {
			return m, err
		}
		if err = i.boundary("command-grants-drained"); err != nil {
			return m, err
		}
	}
	// Output-only production changes preserve the exact runtime, producer
	// generations and delivery owner; they never transfer reporting authority.
	preservesDeliveryOutput := j.Intent.Operation == "set-led-output" && m.Mode == "delivery"
	preservesOwner := preservesManagedDeliveryOwner(j)
	if preservesOwner {
		if err = i.probeDeliveryPause(ctx, d, m); err != nil {
			return m, err
		}
		if err = requireDeliveryOwner(s, m, c); err != nil {
			return m, err
		}
	}
	if !preservesOwner && !preservesDeliveryOutput && j.Intent.Operation != "enable-commands" && j.Intent.Operation != "disable-commands" && !isManagedCommandPolicyOperation(j.Intent.Operation) {
		if err = s.RequireInactiveProducers(); err != nil {
			return m, err
		}
	}
	if j.Intent.Operation == "migrate-led" {
		if err = verifyManagedLEDCompletion(ctx, d, j, c); err != nil {
			return m, err
		}
	}
	if err = i.advance(d, &j, "stopped"); err != nil {
		return m, err
	}
	if err = i.probe(ctx, d, managedHelperName(m), m.HelperHash, m.HelperRevision != ""); err != nil {
		return m, err
	}
	if err = i.probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash, m.HelperRevision != ""); err != nil {
		return m, err
	}
	if err = i.advance(d, &j, "files"); err != nil {
		return m, err
	}
	if m.Mode == "delivery" {
		if err = checkDeliveryAdmission(d, m, c, time.Now().UTC()); err != nil {
			return m, err
		}
	}
	if m.SignalVerification != nil {
		if _, err = readManagedSignalBinding(d, m); err != nil {
			return m, err
		}
		if m.Mode != "uninstalled" {
			if err = i.checkSignalVerification(ctx, m); err != nil {
				return m, err
			}
		}
	}
	// Re-read after service/worker I/O so an operator edit is preserved.
	if err = checkManagedDirectoryPath(d, m.Root, true); err != nil {
		return m, err
	}
	if err = checkManagedDirectoryPath(units, m.UnitDirectory, false); err != nil {
		return m, err
	}
	current, exists, err := managedUnitBytes(units)
	if err != nil {
		return m, err
	}
	if exists != present || !bytes.Equal(current, unit) {
		return m, fmt.Errorf("unit changed during operation")
	}
	if m.Mode == "uninstalled" {
		if !exists && state.UnitFile == "enabled" {
			return m, fmt.Errorf("enabled unit disappeared before disable could be confirmed")
		}
		if exists && state.Load != "not-found" {
			if err = i.services.Disable(ctx); err != nil {
				return m, err
			}
		}
		if err = i.boundary("disable-returned"); err != nil {
			return m, err
		}
		latest, stillExists, e := managedUnitBytes(units)
		if e != nil {
			return m, e
		}
		if stillExists != exists || !bytes.Equal(latest, current) {
			return m, fmt.Errorf("unit changed during disable")
		}
		if exists {
			if err = unix.Unlinkat(int(units.File().Fd()), managedUnit, 0); err != nil {
				return m, err
			}
			if err = units.File().Sync(); err != nil {
				return m, err
			}
		}
	} else if !bytes.Equal(current, after) {
		if err = safefs.ReplaceFile(units, managedUnit, current, after, 0600); err != nil {
			return m, err
		}
	}
	if err = i.boundary("unit-published"); err != nil {
		return m, err
	}
	if err = d.WriteJSON("installation.json", m, true); err != nil {
		return m, err
	}
	if err = i.boundary("manifest-published"); err != nil {
		return m, err
	}
	if err = i.advance(d, &j, "published"); err != nil {
		return m, err
	}
	if err = i.services.Reload(ctx); err != nil {
		return m, err
	}
	if err = i.boundary("reload-returned"); err != nil {
		return m, err
	}
	if err = i.advance(d, &j, "reloaded"); err != nil {
		return m, err
	}
	if err = s.Close(); err != nil {
		s = nil
		return m, err
	}
	s = nil
	lease.Close()
	lease = nil
	if m.Mode == "verification" || m.Mode == "delivery" {
		if m.Mode == "delivery" {
			if err = checkDeliveryAdmission(d, m, c, time.Now().UTC()); err != nil {
				return m, err
			}
			if err = i.inspectDelivery(ctx, m, false); err != nil {
				return m, err
			}
		}
		if _, err = inspectManagedService(ctx, i.services, m, false); err != nil {
			return m, err
		}
		if err = i.checkSignalVerification(ctx, m); err != nil {
			return m, err
		}
		if err = i.services.EnableStart(ctx); err != nil {
			return m, err
		}
		if err = i.boundary("start-returned"); err != nil {
			return m, err
		}
		if err = i.advance(d, &j, "started"); err != nil {
			return m, err
		}
		if _, err = inspectManagedService(ctx, i.services, m, false); err != nil {
			return m, err
		}
		if err = i.checkSignalVerification(ctx, m); err != nil {
			return m, err
		}
		if err = i.services.Verify(ctx, m); err != nil {
			return m, err
		}
	} else {
		st, e := inspectManagedService(ctx, i.services, m, true)
		if e != nil {
			return m, e
		}
		if st.Load != "not-found" {
			return m, fmt.Errorf("removed unit remains loaded")
		}
		if err = i.advance(d, &j, "started"); err != nil {
			return m, err
		}
	}
	if err = i.advance(d, &j, "complete"); err != nil {
		return m, err
	}
	return m, nil
}

func managedPhase(p string) int {
	return map[string]int{"prepared": 1, "stopped": 2, "files": 3, "published": 4, "reloaded": 5, "started": 6, "complete": 7}[p]
}
func (i managedInstaller) advance(d *safefs.Directory, j *managedJournal, phase string) error {
	if managedPhase(j.Phase) >= managedPhase(phase) {
		return nil
	}
	j.Phase = phase
	if err := d.WriteJSON("lifecycle.json", j, true); err != nil {
		return err
	}
	return i.boundary(phase)
}
