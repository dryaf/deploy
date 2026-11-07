package install

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func checkSignalServiceBinding(d *safefs.Directory, want signalServiceBinding) error {
	var actual signalServiceBinding
	raw, err := readSignalVerificationFile(d, signalServiceEndpointBinding)
	if os.IsNotExist(err) && want == (signalServiceBinding{}) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = safefs.DecodeJSON(raw, &actual, 4096); err != nil {
		return err
	}
	if actual != want || want.Root == "" || !managedPath(want.Root) || !safefs.DigestPattern.MatchString(want.ManifestHash) {
		return fmt.Errorf("Signal endpoint is bound to another managed service")
	}
	return nil
}
func readSignalService(d *safefs.Directory, name string) (m SignalServiceManifest, err error) {
	b, err := readSignalVerificationFile(d, name)
	if err != nil {
		return m, err
	}
	if err = safefs.DecodeJSON(b, &m, safefs.MaxRecordBytes); err != nil {
		return
	}
	err = m.validate()
	return
}
func signalServiceUnits(path string) (*safefs.Directory, error) {
	d, err := privateManagedDir(path, false)
	if err != nil {
		return nil, err
	}
	f, e := d.Sub(signalServiceUnit + ".d")
	if e == nil {
		f.Close()
		d.Close()
		return nil, fmt.Errorf("Signal verifier has foreign drop-ins")
	}
	if !os.IsNotExist(e) {
		d.Close()
		return nil, e
	}
	return d, nil
}
func signalServiceUnitBytes(d *safefs.Directory) ([]byte, bool, error) {
	raw, err := readSignalVerificationFile(d, signalServiceUnit)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return raw, err == nil, err
}
func signalServiceFilePin(d *safefs.Directory, name string) (uint64, uint64, error) {
	f, err := d.Open(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	st := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) {
		return 0, 0, fmt.Errorf("unsafe Signal service coordination file")
	}
	return uint64(st.Dev), st.Ino, nil
}
func checkSignalServiceAuthority(d *safefs.Directory, m SignalServiceManifest) error {
	if err := m.validate(); err != nil {
		return err
	}
	if err := checkManagedDirectoryPath(d, m.Spec.Root, true); err != nil {
		return err
	}
	dev, ino, err := verificationFileIdentity(d.File())
	if err != nil {
		return err
	}
	if dev != m.RootDevice || ino != m.RootInode {
		return fmt.Errorf("Signal service root replaced")
	}
	for _, p := range []struct {
		name     string
		dev, ino uint64
	}{{"operation.lock", m.OperationDevice, m.OperationInode}, {"installation.lock", m.RuntimeDevice, m.RuntimeInode}} {
		dev, ino, err = signalServiceFilePin(d, p.name)
		if err != nil {
			return err
		}
		if dev != p.dev || ino != p.ino {
			return fmt.Errorf("Signal service lease replaced")
		}
	}
	saved, err := readSignalService(d, "intent.json")
	if err != nil {
		return err
	}
	if protocol.Hash(saved) != protocol.Hash(m) {
		return fmt.Errorf("Signal service intent changed")
	}
	return nil
}
func checkSignalServiceFiles(d *safefs.Directory, m SignalServiceManifest, published bool) error {
	if err := checkSignalServiceAuthority(d, m); err != nil {
		return err
	}
	if err := checkSignalProductionFiles(m); err != nil {
		return err
	}
	executable, raw, err := nativeSignalFile(filepath.Join(m.Spec.Root, "deploy"), maxManagedBinary, false)
	if err != nil {
		return err
	}
	info, err := executable.Stat()
	executable.Close()
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0700 {
		return fmt.Errorf("Signal service executable permissions changed")
	}
	if managedDigest(raw) != m.Spec.BinarySHA256 {
		return fmt.Errorf("Signal service executable changed")
	}
	if err = checkManagedELF(raw); err != nil {
		return err
	}
	var p SignalVerificationManifest
	if err = d.ReadJSON("endpoint.json", &p); err != nil {
		return err
	}
	if protocol.Hash(p) != protocol.Hash(m.Endpoint) {
		return fmt.Errorf("retained Signal endpoint changed")
	}
	actual, err := inspectSignalVerificationPublication(m.Spec.Endpoint)
	if err != nil {
		return err
	}
	if protocol.Hash(actual) != protocol.Hash(m.Endpoint) {
		return fmt.Errorf("Signal endpoint publication changed")
	}
	if m.Spec.Previous != nil {
		j, err := readSignalServiceJournal(d, m)
		if err != nil {
			return err
		}
		if j.Phase != "prepared" {
			if err := checkSignalServiceReplacement(d, m); err != nil {
				return err
			}
		}
	}
	if published {
		ready, err := readSignalService(d, "ready.json")
		if err != nil {
			return err
		}
		if protocol.Hash(ready) != protocol.Hash(m) {
			return fmt.Errorf("Signal service publication changed")
		}
		units, err := signalServiceUnits(m.Spec.UnitDirectory)
		if err != nil {
			return err
		}
		defer units.Close()
		raw, exists, err := signalServiceUnitBytes(units)
		if err != nil {
			return err
		}
		if !exists || managedDigest(raw) != m.UnitHash {
			return fmt.Errorf("Signal verifier unit changed")
		}
	}
	return nil
}
func inspectSignalService(ref SignalVerificationReference) (SignalServiceManifest, error) {
	return inspectSignalServiceRetained(ref, false)
}
func inspectSignalServiceRetained(ref SignalVerificationReference, allowRemoval bool) (m SignalServiceManifest, err error) {
	if ref.ServiceRoot == "" || ref.validate() != nil {
		return m, fmt.Errorf("managed Signal service pin required")
	}
	d, err := privateManagedDir(ref.ServiceRoot, true)
	if err != nil {
		return m, err
	}
	defer d.Close()
	m, err = readSignalService(d, "complete.json")
	if err != nil {
		return
	}
	if m.Spec.Root != ref.ServiceRoot || m.ManifestHash != ref.ServiceHash || m.Spec.Endpoint.Root != ref.Root || m.Spec.Endpoint.ManifestHash != ref.ManifestHash {
		return m, fmt.Errorf("Signal service reference changed")
	}
	var removal signalServiceBinding
	removing := false
	err = d.ReadJSON("remove.json", &removal)
	if err == nil {
		if !allowRemoval || removal != m.binding() {
			return m, fmt.Errorf("Signal service has a removal fence")
		}
		removing = true
	} else if !os.IsNotExist(err) {
		return m, err
	}
	if err = checkSignalServiceFiles(d, m, !removing); err != nil {
		return
	}
	if err = verifySignalServiceComplete(d, m); err != nil {
		return
	}
	var j signalServiceJournal
	if err = d.ReadJSON("lifecycle.json", &j); err != nil {
		return
	}
	if j.Restart != "" || j.ManifestHash != m.ManifestHash || (j.Phase != "complete" && !(removing && (j.Phase == "removing" || j.Phase == "removed"))) {
		return m, fmt.Errorf("Signal service lifecycle incomplete")
	}
	endpoint, err := privateManagedDir(ref.Root, true)
	if err != nil {
		return m, err
	}
	defer endpoint.Close()
	err = checkSignalServiceBinding(endpoint, m.binding())
	return
}
