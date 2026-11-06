package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type signalServiceInstaller struct {
	manager signalServiceManager
	probe   func(context.Context, *safefs.Directory, SignalServiceManifest) error
	fault   func(string) error
}

func newSignalServiceInstaller() signalServiceInstaller {
	return signalServiceInstaller{manager: userSignalServiceManager{}}
}
func (i signalServiceInstaller) boundary(at string) error {
	if i.fault != nil {
		return i.fault(at)
	}
	return nil
}
func (i signalServiceInstaller) capability(ctx context.Context, d *safefs.Directory, m SignalServiceManifest) error {
	if i.probe != nil {
		return i.probe(ctx, d, m)
	}
	capability := "__signal-verification-service-probe"
	if m.Spec.Previous != nil {
		capability = "__signal-verification-replacement-probe"
	}
	if m.Spec.Runtime != nil {
		capability = "__signal-production-service-probe"
	}
	return probeManagedCapability(ctx, d, "deploy", m.Spec.BinarySHA256, capability)
}
func (i signalServiceInstaller) advance(d *safefs.Directory, m SignalServiceManifest, j *signalServiceJournal, phase string) error {
	if signalServicePhase(j.Phase) >= signalServicePhase(phase) {
		return nil
	}
	j.Phase = phase
	if err := d.WriteJSON("lifecycle.json", *j, true); err != nil {
		return err
	}
	return i.boundary(phase)
}
func openSignalServiceRoot(path string, create bool) (*safefs.Directory, *safefs.Lock, error) {
	d, err := privateManagedDir(path, true)
	if err != nil {
		return nil, nil, err
	}
	lock, err := d.Lock("operation.lock", true, false)
	if create && os.IsNotExist(err) {
		names, e := signalVerificationNames(d)
		if e != nil {
			d.Close()
			return nil, nil, e
		}
		if len(names) != 0 {
			d.Close()
			return nil, nil, fmt.Errorf("Signal service root lost its coordination lease")
		}
		lock, err = d.Lock("operation.lock", true, true)
	}
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	return d, lock, nil
}
func (i signalServiceInstaller) setup(ctx context.Context, s SignalServiceSpec) (out SignalServiceManifest, err error) {
	if err = s.validate(); err != nil {
		return
	}
	d, lock, err := openSignalServiceRoot(s.Root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	defer lock.Close()
	m, err := readSignalService(d, "intent.json")
	if err == nil {
		if protocol.Hash(m.Spec) != protocol.Hash(s) {
			return out, fmt.Errorf("Signal service is enrolled with different pins")
		}
		return i.resumeLocked(ctx, d, m)
	}
	if !os.IsNotExist(err) {
		return out, err
	}
	names, err := signalVerificationNames(d)
	if err != nil {
		return out, err
	}
	for _, name := range names {
		if name != "operation.lock" && name != "installation.lock" && !signalServiceTemp(name) {
			return out, fmt.Errorf("Signal service root contains foreign or lost evidence")
		}
	}
	p, err := inspectSignalVerificationPublication(s.Endpoint)
	if err != nil {
		return out, err
	}
	if s.Previous != nil {
		if _, err = signalServicePredecessor(s); err != nil {
			return out, err
		}
	}
	// No endpoint/account mutation before verifying an unowned unit and executable.
	m = SignalServiceManifest{Spec: s, Endpoint: p}
	units, err := signalServiceUnits(s.UnitDirectory)
	if err != nil {
		return out, err
	}
	defer units.Close()
	_, exists, err := signalServiceUnitBytes(units)
	if err != nil {
		return out, err
	}
	if exists {
		return out, fmt.Errorf("refusing to replace an unowned Signal verifier unit")
	}
	state, err := i.manager.Inspect(ctx, m)
	if err != nil {
		return out, err
	}
	if state.Loaded || state.Active || state.Enabled {
		return out, fmt.Errorf("Signal verifier name is already owned")
	}
	lease, err := d.Lock("installation.lock", true, true)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	m.RootDevice, m.RootInode, err = verificationFileIdentity(d.File())
	if err != nil {
		return out, err
	}
	m.OperationDevice, m.OperationInode, err = signalServiceFilePin(d, "operation.lock")
	if err != nil {
		return out, err
	}
	m.RuntimeDevice, m.RuntimeInode, err = signalServiceFilePin(d, "installation.lock")
	if err != nil {
		return out, err
	}
	body, err := renderSignalService(m)
	if err != nil {
		return out, err
	}
	m.UnitHash = managedDigest([]byte(body))
	m.ManifestHash = protocol.Hash(m)
	if err = m.validate(); err != nil {
		return out, err
	}
	if err = checkSignalProductionInputs(m); err != nil {
		return out, err
	}
	raw, err := nativeSignalRead(s.BinaryFile, maxManagedBinary, false)
	if err != nil {
		return out, err
	}
	if managedDigest(raw) != s.BinarySHA256 {
		return out, fmt.Errorf("Signal service input executable hash changed")
	}
	if err = checkManagedELF(raw); err != nil {
		return out, err
	}
	if err = d.Capacity(int64(len(raw)) + 32<<20); err != nil {
		return out, err
	}
	if err = d.WriteJSON("intent.json", m, false); err != nil {
		return out, err
	}
	// Only locks/intent exist before resume: it may safely finish staging after a crash.
	lease.Close()
	return i.resumeLocked(ctx, d, m)
}
func (i signalServiceInstaller) resume(ctx context.Context, root string) (SignalServiceManifest, error) {
	d, lock, err := openSignalServiceRoot(root, false)
	if err != nil {
		return SignalServiceManifest{}, err
	}
	defer d.Close()
	defer lock.Close()
	m, err := readSignalService(d, "intent.json")
	if err != nil {
		return m, err
	}
	if m.Spec.Root != root {
		return m, fmt.Errorf("Signal service root changed")
	}
	return i.resumeLocked(ctx, d, m)
}
func readSignalServiceJournal(d *safefs.Directory, m SignalServiceManifest) (j signalServiceJournal, err error) {
	err = d.ReadJSON("lifecycle.json", &j)
	if err != nil {
		return
	}
	if j.ManifestHash != m.ManifestHash || signalServicePhase(j.Phase) == 0 {
		err = fmt.Errorf("Signal service journal changed")
	}
	return
}
func (i signalServiceInstaller) resumeLocked(ctx context.Context, d *safefs.Directory, m SignalServiceManifest) (SignalServiceManifest, error) {
	if err := checkSignalServiceAuthority(d, m); err != nil {
		return m, err
	}
	j, err := readSignalServiceJournal(d, m)
	if os.IsNotExist(err) {
		// Recover the intent->journal gap only before any externally bound effect.
		ed, e := privateManagedDir(m.Spec.Endpoint.Root, true)
		if e != nil {
			return m, e
		}
		expected := signalServiceBinding{}
		if m.Spec.Previous != nil {
			expected = signalServiceBinding{m.Spec.Previous.ServiceRoot, m.Spec.Previous.ServiceHash}
		}
		e = checkSignalServiceBinding(ed, expected)
		ed.Close()
		if e != nil {
			return m, e
		}
		for _, name := range []string{"ready.json", "complete.json", "remove.json"} {
			if e = absentServiceFile(d, name); e != nil {
				return m, e
			}
		}
		j = signalServiceJournal{m.ManifestHash, "prepared"}
		if err = d.WriteJSON("lifecycle.json", j, false); err != nil {
			return m, err
		}
		if err = i.boundary("prepared"); err != nil {
			return m, err
		}
	} else if err != nil {
		return m, err
	}
	var removing signalServiceBinding
	e := d.ReadJSON("remove.json", &removing)
	if e == nil {
		if removing != m.binding() {
			return m, fmt.Errorf("Signal service removal proof changed")
		}
		return i.removeLocked(ctx, d, m, j)
	}
	if !os.IsNotExist(e) {
		return m, e
	}
	if signalServicePhase(j.Phase) >= signalServicePhase("removing") {
		return m, fmt.Errorf("Signal service removal intent lost")
	}
	if j.Phase == "prepared" {
		endpoint, e := privateManagedDir(m.Spec.Endpoint.Root, true)
		if e != nil {
			return m, e
		}
		var bound signalServiceBinding
		e = endpoint.ReadJSON(signalServiceEndpointBinding, &bound)
		endpoint.Close()
		if e == nil {
			if bound != m.binding() && (m.Spec.Previous == nil || bound != (signalServiceBinding{m.Spec.Previous.ServiceRoot, m.Spec.Previous.ServiceHash})) {
				return m, fmt.Errorf("Signal endpoint belongs to another service")
			}
			// This installation's binding proves staging already finished.
			if bound == m.binding() {
				if e = checkSignalServiceFiles(d, m, false); e != nil {
					return m, e
				}
			}
		} else if !os.IsNotExist(e) {
			return m, e
		}
		raw, e := ownedManagedFile(d, "deploy", maxManagedBinary)
		if os.IsNotExist(e) {
			raw, e = nativeSignalRead(m.Spec.BinaryFile, maxManagedBinary, false)
		}
		if e != nil {
			return m, e
		}
		if managedDigest(raw) != m.Spec.BinarySHA256 {
			return m, fmt.Errorf("Signal service executable hash changed")
		}
		if e = checkManagedELF(raw); e != nil {
			return m, e
		}
		if e = frozenManagedFile(d, "deploy", raw, 0700); e != nil {
			return m, e
		}
		if e = stageSignalProduction(d, m); e != nil {
			return m, e
		}
		if e = runtimeJSONOnceForService(d, "endpoint.json", m.Endpoint); e != nil {
			return m, e
		}
	}
	published := signalServicePhase(j.Phase) >= signalServicePhase("published")
	if err = checkSignalServiceFiles(d, m, published); err != nil {
		return m, err
	}
	if err = i.capability(ctx, d, m); err != nil {
		return m, err
	}
	units, err := signalServiceUnits(m.Spec.UnitDirectory)
	if err != nil {
		return m, err
	}
	defer units.Close()
	raw, exists, err := signalServiceUnitBytes(units)
	if err != nil {
		return m, err
	}
	if exists && managedDigest(raw) != m.UnitHash {
		return m, fmt.Errorf("foreign Signal verifier unit content")
	}
	if !exists && signalServicePhase(j.Phase) >= signalServicePhase("unit") {
		return m, fmt.Errorf("Signal verifier unit disappeared")
	}
	// Reconcile a lost reload reply only after this transaction owns the exact unit.
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
		return m, fmt.Errorf("Signal verifier loaded without its owned unit")
	}
	if state.Active {
		if !published {
			return m, fmt.Errorf("Signal verifier active before publication")
		}
		if j.Phase == "complete" {
			if e = verifySignalServiceComplete(d, m); e != nil {
				return m, e
			}
		}
		if err = i.manager.Verify(ctx, m); err != nil {
			return m, err
		}
		return i.complete(d, m, j)
	}
	if j.Phase == "complete" {
		if err = verifySignalServiceComplete(d, m); err != nil {
			return m, err
		}
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return m, err
	}
	defer lease.Close()
	if m.Spec.Previous != nil && j.Phase == "prepared" {
		if err = i.replaceBinding(ctx, d, m); err != nil {
			return m, err
		}
	}
	endpoint, err := privateManagedDir(m.Spec.Endpoint.Root, true)
	if err != nil {
		return m, err
	}
	var binding signalServiceBinding
	err = endpoint.ReadJSON(signalServiceEndpointBinding, &binding)
	endpoint.Close()
	if os.IsNotExist(err) {
		if j.Phase != "prepared" {
			return m, fmt.Errorf("Signal service endpoint binding lost")
		}
		binding = signalServiceBinding{}
	} else if err != nil {
		return m, err
	} else if binding != m.binding() {
		return m, fmt.Errorf("Signal endpoint belongs to another service")
	}
	v, err := openSignalEndpointVerifierService(ctx, m.Spec.Endpoint.Root, m.Spec.Endpoint.ManifestHash, binding)
	if err != nil {
		return m, err
	}
	defer v.close()
	if binding == (signalServiceBinding{}) {
		if err = v.root.dir.WriteJSON(signalServiceEndpointBinding, m.binding(), false); err != nil {
			return m, err
		}
		v.service = m.binding()
	}
	if err = i.advance(d, m, &j, "bound"); err != nil {
		return m, err
	}
	if err = v.check(ctx); err != nil {
		return m, err
	}
	body, err := renderSignalService(m)
	if err != nil {
		return m, err
	}
	if err = checkManagedDirectoryPath(units, m.Spec.UnitDirectory, false); err != nil {
		return m, err
	}
	current, present, err := signalServiceUnitBytes(units)
	if err != nil {
		return m, err
	}
	if present != exists || string(current) != string(raw) {
		return m, fmt.Errorf("Signal unit changed during preparation")
	}
	if !exists {
		if err = frozenManagedFile(units, signalServiceUnit, []byte(body), 0600); err != nil {
			return m, err
		}
	}
	if err = i.advance(d, m, &j, "unit"); err != nil {
		return m, err
	}
	if err = runtimeJSONOnceForService(d, "ready.json", m); err != nil {
		return m, err
	}
	if err = i.advance(d, m, &j, "published"); err != nil {
		return m, err
	}
	if err = i.manager.Reload(ctx); err != nil {
		return m, err
	}
	if err = i.advance(d, m, &j, "reloaded"); err != nil {
		return m, err
	}
	if _, err = i.manager.Inspect(ctx, m); err != nil {
		return m, err
	}
	if err = checkSignalServiceFiles(d, m, true); err != nil {
		return m, err
	}
	if err = v.check(ctx); err != nil {
		return m, err
	}
	v.close()

	lease.Close()
	if err = i.manager.Start(ctx); err != nil {
		return m, err
	}
	if err = i.boundary("start-returned"); err != nil {
		return m, err
	}
	if err = i.advance(d, m, &j, "started"); err != nil {
		return m, err
	}
	if err = i.manager.Verify(ctx, m); err != nil {
		return m, err
	}
	return i.complete(d, m, j)
}
func (i signalServiceInstaller) complete(d *safefs.Directory, m SignalServiceManifest, j signalServiceJournal) (SignalServiceManifest, error) {
	if j.Phase == "complete" {
		return m, verifySignalServiceComplete(d, m)
	}
	if err := runtimeJSONOnceForService(d, "complete.json", m); err != nil {
		return m, err
	}
	return m, i.advance(d, m, &j, "complete")
}
func verifySignalServiceComplete(d *safefs.Directory, m SignalServiceManifest) error {
	ready, err := readSignalService(d, "ready.json")
	if err != nil {
		return err
	}
	if protocol.Hash(ready) != protocol.Hash(m) {
		return fmt.Errorf("Signal service publication changed")
	}
	saved, err := readSignalService(d, "complete.json")
	if err != nil {
		return err
	}
	if protocol.Hash(saved) != protocol.Hash(m) {
		return fmt.Errorf("Signal service completion changed")
	}
	return nil
}
func absentServiceFile(d *safefs.Directory, name string) error {
	f, err := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err == nil {
		f.Close()
		return fmt.Errorf("unexpected Signal service artifact")
	}
	return err
}
func runtimeJSONOnceForService(d *safefs.Directory, name string, value any) error {
	want, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw, err := readSignalVerificationFile(d, name)
	if os.IsNotExist(err) {
		return d.WriteJSON(name, value, false)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, want) {
		return fmt.Errorf("immutable Signal service record changed")
	}
	return nil
}
