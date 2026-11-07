package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

type freshSignalManager interface {
	Inspect(context.Context, FreshSignalManifest) (signalServiceState, error)
	Reload(context.Context) error
	Start(context.Context) error
	Verify(context.Context, FreshSignalManifest) error
}
type freshSignalInstaller struct {
	manager      freshSignalManager
	probe        func(context.Context, *safefs.Directory, FreshSignalManifest) error
	restartProbe func(context.Context, *safefs.Directory, FreshSignalManifest) error
	fault        func(string) error
}

// EnsureFreshSignal installs/resumes one exact first-install declaration. The
// coordinator creates a private empty service root and canonical inbox first.
// This API does not send a provider message or qualify delivery.
func EnsureFreshSignal(ctx context.Context, s FreshSignalSpec) (FreshSignalManifest, error) {
	return (freshSignalInstaller{manager: userFreshSignalManager{}}).setup(ctx, s)
}
func (i freshSignalInstaller) boundary(ctx context.Context, phase string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.fault != nil {
		return i.fault(phase)
	}
	return nil
}
func (i freshSignalInstaller) advance(ctx context.Context, d *safefs.Directory, m FreshSignalManifest, j *signalServiceJournal, phase string) error {
	if freshSignalPhase(j.Phase) >= freshSignalPhase(phase) {
		return nil
	}
	j.Phase = phase
	if err := d.WriteJSON("lifecycle.json", j, true); err != nil {
		return err
	}
	return i.boundary(ctx, phase)
}
func (i freshSignalInstaller) setup(ctx context.Context, s FreshSignalSpec) (m FreshSignalManifest, err error) {
	if err = s.validate(); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	d, lock, err := openSignalServiceRoot(s.Root, true)
	if err != nil {
		return m, err
	}
	defer d.Close()
	defer lock.Close()
	m, err = readFreshSignal(d, "intent.json")
	if err == nil {
		if protocol.Hash(s) != protocol.Hash(m.Spec) {
			return m, fmt.Errorf("fresh Signal service declaration changed; review retained installation")
		}
		return i.resume(ctx, d, m)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return m, err
	}
	names, err := signalVerificationNames(d)
	if err != nil {
		return m, err
	}
	for _, name := range names {
		if name != "operation.lock" && name != "installation.lock" && !signalServiceTemp(name) {
			return m, fmt.Errorf("fresh Signal root contains foreign or lost authority")
		}
	}
	if err = relay.InspectFreshSignalInbox(s.Inbox); err != nil {
		return m, err
	}
	units, err := freshSignalUnits(s.UnitDirectory)
	if err != nil {
		return m, err
	}
	defer units.Close()
	_, exists, err := freshSignalUnitBytes(units)
	if err != nil {
		return m, err
	}
	if exists {
		return m, fmt.Errorf("refusing to replace an unowned fresh Signal unit")
	}
	m = FreshSignalManifest{Spec: s}
	state, err := i.manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if state.Loaded || state.Active || state.Enabled {
		return m, fmt.Errorf("fresh Signal service name already owned")
	}
	lease, err := d.Lock("installation.lock", true, true)
	if err != nil {
		return m, err
	}
	defer lease.Close()
	m.RootDevice, m.RootInode, err = verificationFileIdentity(d.File())
	if err != nil {
		return m, err
	}
	m.OperationDevice, m.OperationInode, err = signalServiceFilePin(d, "operation.lock")
	if err != nil {
		return m, err
	}
	m.RuntimeDevice, m.RuntimeInode, err = signalServiceFilePin(d, "installation.lock")
	if err != nil {
		return m, err
	}
	body, err := renderFreshSignal(m)
	if err != nil {
		return m, err
	}
	m.UnitHash = managedDigest([]byte(body))
	m.ManifestHash = protocol.Hash(m)
	if err = m.validate(); err != nil {
		return m, err
	}
	// Verify every source before reserving an installation; secret values never
	// enter lifecycle records, argv, health responses or diagnostics.
	var size int64
	for _, input := range s.inputs() {
		raw, e := nativeSignalRead(input.path, input.limit, input.mode == 0600)
		if e != nil {
			return m, e
		}
		if managedDigest(raw) != input.hash {
			return m, fmt.Errorf("fresh Signal input changed: %s", input.name)
		}
		if input.mode == 0700 {
			if e = checkManagedELF(raw); e != nil {
				return m, e
			}
		}
		size += int64(len(raw))
	}
	if err = d.Capacity(size + (32 << 20)); err != nil {
		return m, err
	}
	if err = d.WriteJSON("intent.json", m, false); err != nil {
		return m, err
	}
	if err = i.boundary(ctx, "intent"); err != nil {
		return m, err
	}
	lease.Close()
	return i.resume(ctx, d, m)
}
func (i freshSignalInstaller) resume(ctx context.Context, d *safefs.Directory, m FreshSignalManifest) (FreshSignalManifest, error) {
	if err := freshSignalNotRemoving(d); err != nil {
		return m, err
	}
	if err := checkFreshSignalAuthority(d, m); err != nil {
		return m, err
	}
	j, err := readFreshSignalJournal(d, m)
	if errors.Is(err, os.ErrNotExist) {
		// An intent/journal crash gap is safe only before artifacts, account binding
		// or publication. A lost later journal must never recreate ownership.
		names, e := signalVerificationNames(d)
		if e != nil {
			return m, e
		}
		for _, name := range names {
			if name != "operation.lock" && name != "installation.lock" && name != "intent.json" && !signalServiceTemp(name) {
				return m, fmt.Errorf("fresh Signal lifecycle authority missing")
			}
		}
		j = signalServiceJournal{ManifestHash: m.ManifestHash, Phase: "prepared"}
		if err = d.WriteJSON("lifecycle.json", j, false); err != nil {
			return m, err
		}
		if err = i.boundary(ctx, "prepared"); err != nil {
			return m, err
		}
	} else if err != nil {
		return m, err
	}
	if err = ctx.Err(); err != nil {
		return m, err
	}
	if j.Phase == "prepared" {
		if err = stageFreshSignal(d, m); err != nil {
			return m, err
		}
		probe := i.probe
		if probe == nil {
			probe = func(ctx context.Context, d *safefs.Directory, m FreshSignalManifest) error {
				return probeManagedCapability(ctx, d, "deploy", m.Spec.BinarySHA256, "__fresh-signal-probe")
			}
		}
		if err = probe(ctx, d, m); err != nil {
			return m, err
		}
		if err = i.advance(ctx, d, m, &j, "staged"); err != nil {
			return m, err
		}
	}
	published := freshSignalPhase(j.Phase) >= freshSignalPhase("published")
	if err = checkFreshSignalFiles(d, m, published); err != nil {
		return m, err
	}
	units, err := freshSignalUnits(m.Spec.UnitDirectory)
	if err != nil {
		return m, err
	}
	defer units.Close()
	raw, exists, err := freshSignalUnitBytes(units)
	if err != nil {
		return m, err
	}
	if exists && managedDigest(raw) != m.UnitHash {
		return m, fmt.Errorf("foreign fresh Signal unit content")
	}
	if exists && freshSignalPhase(j.Phase) < freshSignalPhase("bound") {
		return m, fmt.Errorf("fresh Signal unit appeared before account binding")
	}
	if !exists && freshSignalPhase(j.Phase) >= freshSignalPhase("unit") {
		return m, fmt.Errorf("fresh Signal unit disappeared")
	}
	// Reload is safe only after this operation owns the exact unit. It recovers
	// lost reload responses without accepting aliases or foreign loaded commands.
	if exists && j.Phase != "complete" {
		if err = i.manager.Reload(ctx); err != nil {
			return m, err
		}
	}
	state, err := i.manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if state.Loaded && !exists {
		return m, fmt.Errorf("fresh Signal manager loaded an unowned unit")
	}
	if freshSignalPhase(j.Phase) >= freshSignalPhase("bound") {
		if err = signalaccount.InspectNativeService(m.Spec.AccountState, m.binding()); err != nil {
			return m, err
		}
	}
	if state.Active {
		if !published {
			return m, fmt.Errorf("fresh Signal service active before publication")
		}
		if j.Phase == "complete" {
			if err = verifyFreshSignalComplete(d, m); err != nil {
				return m, err
			}
		}
		if err = i.manager.Verify(ctx, m); err != nil {
			return m, err
		}
		return i.complete(ctx, d, m, j)
	}
	if j.Phase == "complete" {
		return m, fmt.Errorf("completed fresh Signal service is stopped; inspect service health before restarting")
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return m, err
	}
	defer lease.Close()
	if j.Phase == "staged" {
		identity, e := signalaccount.InspectNativeAccount(ctx, m.Spec.AccountState, filepath.Join(m.Spec.Root, "account"), signalaccount.NativeCLI{Path: filepath.Join(m.Spec.Root, "signal-cli"), SHA256: m.Spec.CLISHA256, Version: m.Spec.CLIVersion}, m.Spec.LinkOperation, m.Spec.LinkHash)
		if e != nil {
			return m, e
		}
		if identity != m.Spec.Inbox.Account {
			return m, fmt.Errorf("fresh Signal account differs from canonical inbox")
		}
		if err = i.advance(ctx, d, m, &j, "verified"); err != nil {
			return m, err
		}
	}
	if j.Phase == "verified" {
		if err = signalaccount.BindNativeService(m.Spec.AccountState, m.binding()); err != nil {
			return m, err
		}
		if err = i.boundary(ctx, "binding-returned"); err != nil {
			return m, err
		}
		if err = i.advance(ctx, d, m, &j, "bound"); err != nil {
			return m, err
		}
	}
	if err = signalaccount.InspectNativeService(m.Spec.AccountState, m.binding()); err != nil {
		return m, err
	}
	if err = checkManagedDirectoryPath(units, m.Spec.UnitDirectory, false); err != nil {
		return m, err
	}
	current, present, err := freshSignalUnitBytes(units)
	if err != nil {
		return m, err
	}
	if present != exists || string(current) != string(raw) {
		return m, fmt.Errorf("fresh Signal unit changed during preparation")
	}
	if !exists {
		body, e := renderFreshSignal(m)
		if e != nil {
			return m, e
		}
		if err = frozenManagedFile(units, freshSignalUnit, []byte(body), 0600); err != nil {
			return m, err
		}
		if err = i.boundary(ctx, "unit-returned"); err != nil {
			return m, err
		}
	}
	if err = i.advance(ctx, d, m, &j, "unit"); err != nil {
		return m, err
	}
	if err = runtimeJSONOnceForService(d, "ready.json", m); err != nil {
		return m, err
	}
	if err = i.advance(ctx, d, m, &j, "published"); err != nil {
		return m, err
	}
	if err = i.manager.Reload(ctx); err != nil {
		return m, err
	}
	state, err = i.manager.Inspect(ctx, m)
	if err != nil {
		return m, err
	}
	if !state.Loaded || state.Active {
		return m, fmt.Errorf("fresh Signal manager did not load the staged unit")
	}
	if err = checkFreshSignalFiles(d, m, true); err != nil {
		return m, err
	}
	lease.Close()
	if err = ctx.Err(); err != nil {
		return m, err
	}
	if err = i.manager.Start(ctx); err != nil {
		return m, err
	}
	if err = i.boundary(ctx, "start-returned"); err != nil {
		return m, err
	}
	if err = i.advance(ctx, d, m, &j, "started"); err != nil {
		return m, err
	}
	if err = i.manager.Verify(ctx, m); err != nil {
		return m, err
	}
	return i.complete(ctx, d, m, j)
}
func (i freshSignalInstaller) complete(ctx context.Context, d *safefs.Directory, m FreshSignalManifest, j signalServiceJournal) (FreshSignalManifest, error) {
	if j.Phase == "complete" {
		return m, verifyFreshSignalComplete(d, m)
	}
	if err := runtimeJSONOnceForService(d, "complete.json", m); err != nil {
		return m, err
	}
	return m, i.advance(ctx, d, m, &j, "complete")
}
func verifyFreshSignalComplete(d *safefs.Directory, m FreshSignalManifest) error {
	for _, name := range []string{"ready.json", "complete.json"} {
		saved, err := readFreshSignal(d, name)
		if err != nil {
			return err
		}
		if saved.ManifestHash != m.ManifestHash {
			return fmt.Errorf("fresh Signal completion changed")
		}
	}
	return nil
}

// InspectFreshSignal verifies completed retained authority; optional health also
// requires the exact service process and its peer-authenticated Unix endpoint.
func InspectFreshSignal(ctx context.Context, ref FreshSignalReference, health bool) (FreshSignalManifest, error) {
	var m FreshSignalManifest
	if !managedPath(ref.Root) || !safefs.DigestPattern.MatchString(ref.ManifestHash) {
		return m, fmt.Errorf("fresh Signal service reference required")
	}
	d, err := privateManagedDir(ref.Root, true)
	if err != nil {
		return m, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return m, err
	}
	defer lock.Close()
	if err = freshSignalNotRemoving(d); err != nil {
		return m, err
	}
	m, err = readFreshSignal(d, "complete.json")
	if err != nil {
		return m, err
	}
	if m.reference() != ref {
		return m, fmt.Errorf("fresh Signal service reference changed")
	}
	if err = checkFreshSignalFiles(d, m, true); err != nil {
		return m, err
	}
	if err = verifyFreshSignalComplete(d, m); err != nil {
		return m, err
	}
	j, err := readFreshSignalJournal(d, m)
	if err != nil {
		return m, err
	}
	if j.Phase != "complete" {
		return m, fmt.Errorf("fresh Signal service operation incomplete")
	}
	if err = signalaccount.InspectNativeService(m.Spec.AccountState, m.binding()); err != nil {
		return m, err
	}
	restart, _, e := readFreshSignalRestart(d, m, j, false)
	if e != nil {
		return m, e
	}
	if restart != nil && !restart.Complete {
		return m, FreshSignalResumeRequired{Reference: m.reference()}
	}
	if health {
		manager := userFreshSignalManager{}
		state, e := manager.Inspect(ctx, m)
		if e != nil {
			return m, e
		}
		if state.Loaded && state.Enabled && !state.Active {
			return m, FreshSignalResumeRequired{Reference: m.reference()}
		}
		err = manager.Verify(ctx, m)
	}
	return m, err
}

// InspectFreshSignalSetup distinguishes a resumable first installation from a
// completed canonical receipt. Missing completion after a complete journal is
// an error, never permission to recreate the service or relink the account.
func InspectFreshSignalSetup(ctx context.Context, s FreshSignalSpec, health bool) (FreshSignalManifest, bool, error) {
	var m FreshSignalManifest
	if err := s.validate(); err != nil {
		return m, false, err
	}
	d, err := privateManagedDir(s.Root, true)
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	defer d.Close()
	m, err = readFreshSignal(d, "intent.json")
	if errors.Is(err, os.ErrNotExist) {
		names, e := signalVerificationNames(d)
		if e != nil {
			return m, false, e
		}
		for _, name := range names {
			if name != "operation.lock" && name != "installation.lock" && !signalServiceTemp(name) {
				return m, false, fmt.Errorf("fresh Signal intent lost or root unowned")
			}
		}
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if protocol.Hash(m.Spec) != protocol.Hash(s) {
		return m, false, fmt.Errorf("fresh Signal setup pins changed")
	}
	if err = checkFreshSignalAuthority(d, m); err != nil {
		return m, false, err
	}
	j, err := readFreshSignalJournal(d, m)
	if errors.Is(err, os.ErrNotExist) {
		names, e := signalVerificationNames(d)
		if e != nil {
			return m, false, e
		}
		for _, name := range names {
			if name != "operation.lock" && name != "installation.lock" && name != "intent.json" && !signalServiceTemp(name) {
				return m, false, fmt.Errorf("fresh Signal lifecycle lost")
			}
		}
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if j.Phase != "complete" {
		return m, false, nil
	}
	m, err = InspectFreshSignal(ctx, m.reference(), health)
	return m, err == nil, err
}
