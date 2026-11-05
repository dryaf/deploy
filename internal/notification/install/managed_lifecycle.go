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
	deliveryProbe       func(context.Context, *safefs.Directory, string, string) error
	deliveryInspect     func(context.Context, ManagedManifest, bool) error
	services            managedServices
	signalProbe         func(context.Context, *safefs.Directory, string, string) error
	signalProfilesProbe func(context.Context, *safefs.Directory, string, string) error
	signalCheck         func(context.Context, ManagedManifest) error
	probe               func(context.Context, *safefs.Directory, string, string, bool) error
	recoveryProbe       func(context.Context, *safefs.Directory, string, string) error
	helperProbe         func(context.Context, *safefs.Directory, string, string) error
	telegramProbe       func(context.Context, *safefs.Directory, string, string) error
	telegramTransport   http.RoundTripper
	ledProbe            func(context.Context, *safefs.Directory, string, string) error
	ledAPI              func(string) worker.LEDAPI
	fault               func(string) error
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
	if operation != "setup" && operation != "update" && operation != "migrate-schema" && operation != "migrate-led" && operation != "adopt-recovery" && operation != "upgrade-helper" && operation != "adopt-signal" && operation != "supervise-signal" && operation != "migrate-signal-profiles" {
		return zero, fmt.Errorf("invalid preparation operation")
	}
	if (operation == "adopt-signal" || operation == "supervise-signal") != (spec.SignalVerification != nil) {
		return zero, fmt.Errorf("adopt-signal and supervise-signal require signal_verification")
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
		if b.Mode == "delivery" {
			return zero, fmt.Errorf("deactivate delivery before changing the installation")
		}
		if b.DeliveryOwnerHash != "" && operation != "update" && operation != "upgrade-helper" {
			return zero, fmt.Errorf("delivery-enrolled state requires an explicit future migration")
		}
		before = &b
		if b.RecoveryCommitHash != "" && (operation == "adopt-recovery" || operation == "migrate-schema" || operation == "migrate-led") {
			return zero, fmt.Errorf("recovered runtime migration requires ancestor support")
		}
		if (operation == "adopt-recovery" || operation == "upgrade-helper") && b.Mode != "verification" {
			return zero, fmt.Errorf("helper upgrade/recovery adoption requires an installed verification service")
		}
		if expected != "" && protocol.Hash(b) != expected {
			return zero, fmt.Errorf("installation changed while preparing rollback; review the current revision")
		}
		if b.Root != spec.Root || b.Paths.State != spec.State || b.UnitDirectory != spec.UnitDirectory || (operation != "migrate-led" && operation != "adopt-signal" && b.ConfigHash != protocol.Hash(c)) {
			return zero, fmt.Errorf("update changes identity, configuration or schema; explicit migration required")
		}
		if operation == "migrate-schema" {
			if b.Schema != 9 || worker.StorageSchemaVersion() != 10 || b.Mode != "verification" {
				return zero, fmt.Errorf("only an installed verification schema 9 -> 10 migration is supported")
			}
		} else if b.Schema != worker.StorageSchemaVersion() {
			return zero, fmt.Errorf("schema change requires migrate-schema")
		}
		if _, e = verifyManagedArtifactsVersion(d, b, operation == "migrate-schema"); e != nil {
			return zero, e
		}
		if operation == "adopt-signal" || operation == "supervise-signal" {
			if _, e = validateManagedSignalTransition(operation, b, c, raw, *spec.SignalVerification); e != nil {
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
		s, e := worker.OpenManagedVerification(spec.State, c.Worker, identity)
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
	m := ManagedManifest{Version: managedVersion, Installation: id, Revision: revision, Root: spec.Root, UnitDirectory: spec.UnitDirectory, Paths: ServicePaths{Executable: filepath.Join(spec.Root, "releases", revision, "deploy"), Config: filepath.Join(spec.Root, "releases", revision, "runtime.json"), State: spec.State, Socket: filepath.Join(spec.State, "api.sock"), DeniedPaths: spec.DeniedPaths}, Identity: identity, RuntimeHash: managedDigest(raw), ConfigHash: protocol.Hash(c), BinaryHash: spec.BinarySHA256, HelperHash: spec.BinarySHA256, Schema: worker.StorageSchemaVersion(), Mode: "verification"}
	if before != nil {
		m.Installation = before.Installation
		m.HelperHash = before.HelperHash
		m.HelperRevision = before.HelperRevision
		m.RecoveryCommitHash = before.RecoveryCommitHash
		m.TelegramRenewalID = before.TelegramRenewalID
		m.SignalVerification = before.SignalVerification
		m.SignalProfiles = before.SignalProfiles
		m.DeliveryOwnerHash = before.DeliveryOwnerHash
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
		profileManifest, profileRuntime, err = prepareSignalProfiles(*before, raw, endpoint, profileOperation)
		if err != nil {
			return zero, err
		}
		m.SignalProfiles = &ManagedSignalProfiles{Operation: profileOperation, Hash: protocol.Hash(profileManifest)}
	}
	var signalEndpoint SignalVerificationManifest
	var signalOperation string
	if operation == "adopt-signal" || operation == "supervise-signal" {
		signalOperation, err = safefs.NewID("n")
		if err != nil {
			return zero, err
		}
		m.SignalVerification = &ManagedSignalBinding{SignalVerificationReference: *spec.SignalVerification, Operation: signalOperation}
		signalEndpoint, err = validateManagedSignalTransition(operation, *before, c, raw, *spec.SignalVerification)
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
	}
	if operation == "migrate-schema" || operation == "migrate-led" || operation == "adopt-recovery" || operation == "upgrade-helper" || operation == "adopt-signal" || operation == "supervise-signal" || operation == "migrate-signal-profiles" {
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
	if err == nil && (operation == "migrate-schema" || operation == "migrate-led" || operation == "adopt-recovery" || operation == "upgrade-helper" || operation == "adopt-signal" || operation == "supervise-signal" || operation == "migrate-signal-profiles") {
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
	if operation == "migrate-signal-profiles" {
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
	if operation == "adopt-signal" || operation == "supervise-signal" {
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

func limitManagedHistory(d *safefs.Directory) error {
	for _, name := range []string{"operations", "releases"} {
		items, err := d.Sub(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		names, err := items.File().Readdirnames(33)
		items.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(names) >= 32 {
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
	interrupted := j.Phase != "complete"
	if interrupted && (j.Intent.Operation == "migrate-schema" || j.Intent.Operation == "migrate-led" || j.Intent.Operation == "adopt-recovery" || j.Intent.Operation == "renew-telegram" || j.Intent.Operation == "upgrade-helper" || j.Intent.Operation == "adopt-signal" || j.Intent.Operation == "supervise-signal" || j.Intent.Operation == "migrate-signal-profiles") {
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
	if _, err = verifyManagedArtifacts(d, b); err != nil {
		return zero, err
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
	if err = limitManagedHistory(d); err != nil {
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
	// Completed uninstall changes only Mode (proved by journal validation).
	// Its After artifacts already validate the identical retained installation;
	// revalidating Before as live would incorrectly require removed dependencies.
	if j.Intent.Before != nil && !(j.Intent.Operation == "uninstall" && j.Phase == "complete") {
		if _, err = verifyManagedArtifactsVersion(d, *j.Intent.Before, j.Intent.Operation == "migrate-schema"); err != nil {
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
	if err = i.probeSignalVerification(ctx, d, m); err != nil {
		return m, err
	}
	if err = i.probeSignalProfiles(ctx, d, m); err != nil {
		return m, err
	}
	if err = i.probeDelivery(ctx, d, m); err != nil {
		return m, err
	}
	if m.DeliveryOwnerHash != "" {
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
	if err = s.RequireInactiveProducers(); err != nil {
		return m, err
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
