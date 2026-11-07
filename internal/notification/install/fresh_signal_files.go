package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/safefs"
)

type freshSignalInput struct {
	name, path, hash string
	limit            int64
	mode             uint32
}

func (s FreshSignalSpec) inputs() []freshSignalInput {
	return []freshSignalInput{
		{"deploy", s.BinaryFile, s.BinarySHA256, maxManagedBinary, 0700},
		{"signal-cli", s.CLIFile, s.CLISHA256, maxSignalCLI, 0700},
		{"account", s.AccountFile, s.AccountSHA256, 4096, 0600},
		{"recipient", s.RecipientFile, s.RecipientSHA256, 4096, 0600},
	}
}
func readFreshSignal(d *safefs.Directory, name string) (m FreshSignalManifest, err error) {
	raw, err := readSignalVerificationFile(d, name)
	if err != nil {
		return m, err
	}
	if err = safefs.DecodeJSON(raw, &m, safefs.MaxRecordBytes); err != nil {
		return m, err
	}
	return m, m.validate()
}
func checkFreshSignalAuthority(d *safefs.Directory, m FreshSignalManifest) error {
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
		return fmt.Errorf("fresh Signal service root replaced")
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
			return fmt.Errorf("fresh Signal service lease replaced")
		}
	}
	retained, err := readFreshSignal(d, "intent.json")
	if err != nil {
		return err
	}
	if protocol.Hash(retained) != protocol.Hash(m) {
		return fmt.Errorf("fresh Signal service intent changed")
	}
	return relay.InspectFreshSignalInbox(m.Spec.Inbox)
}
func stageFreshSignal(d *safefs.Directory, m FreshSignalManifest) error {
	for _, input := range m.Spec.inputs() {
		raw, err := ownedManagedFile(d, input.name, input.limit)
		if errors.Is(err, os.ErrNotExist) {
			raw, err = nativeSignalRead(input.path, input.limit, input.mode == 0600)
		}
		if err != nil {
			return err
		}
		if managedDigest(raw) != input.hash {
			return fmt.Errorf("fresh Signal input changed: %s", input.name)
		}
		if input.mode == 0700 {
			if err = checkManagedELF(raw); err != nil {
				return err
			}
		}
		if err = frozenManagedFile(d, input.name, raw, input.mode); err != nil {
			return err
		}
	}
	return checkFreshSignalFiles(d, m, false)
}
func freshSignalUnits(path string) (*safefs.Directory, error) {
	d, err := privateManagedDir(path, false)
	if err != nil {
		return nil, err
	}
	f, err := d.Sub(freshSignalUnit + ".d")
	if err == nil {
		f.Close()
		d.Close()
		return nil, fmt.Errorf("fresh Signal unit has foreign drop-ins")
	}
	if !errors.Is(err, os.ErrNotExist) {
		d.Close()
		return nil, err
	}
	return d, nil
}
func freshSignalUnitBytes(d *safefs.Directory) ([]byte, bool, error) {
	raw, err := readSignalVerificationFile(d, freshSignalUnit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return raw, err == nil, err
}
func checkFreshSignalFiles(d *safefs.Directory, m FreshSignalManifest, published bool) error {
	if err := checkFreshSignalAuthority(d, m); err != nil {
		return err
	}
	for _, input := range m.Spec.inputs() {
		if err := freshSignalExecutables.verify(m.Spec.Root, input); err != nil {
			return err
		}
	}
	if _, err := provider.ReadSecret(filepath.Join(m.Spec.Root, "account")); err != nil {
		return err
	}
	if _, err := provider.ReadSecret(filepath.Join(m.Spec.Root, "recipient")); err != nil {
		return err
	}
	if published {
		retained, err := readFreshSignal(d, "ready.json")
		if err != nil {
			return err
		}
		if retained.ManifestHash != m.ManifestHash {
			return fmt.Errorf("fresh Signal publication changed")
		}
		units, err := freshSignalUnits(m.Spec.UnitDirectory)
		if err != nil {
			return err
		}
		defer units.Close()
		raw, exists, err := freshSignalUnitBytes(units)
		if err != nil {
			return err
		}
		if !exists || managedDigest(raw) != m.UnitHash {
			return fmt.Errorf("fresh Signal unit changed")
		}
	}
	return nil
}
func freshSignalHealth(m FreshSignalManifest) (FreshSignalHealth, error) {
	account, err := provider.ReadSecret(filepath.Join(m.Spec.Root, "account"))
	if err != nil {
		return FreshSignalHealth{}, err
	}
	recipient, err := provider.ReadSecret(filepath.Join(m.Spec.Root, "recipient"))
	if err != nil {
		return FreshSignalHealth{}, err
	}
	return FreshSignalHealth{Version: 1, ServiceHash: m.ManifestHash, Account: m.Spec.Inbox.Account, Inbox: m.Spec.Inbox.Identity, Destination: relay.Destination(account, recipient)}, nil
}
func readFreshSignalJournal(d *safefs.Directory, m FreshSignalManifest) (j signalServiceJournal, err error) {
	err = d.ReadJSON("lifecycle.json", &j)
	if err == nil && (j.ManifestHash != m.ManifestHash || freshSignalPhase(j.Phase) == 0) {
		err = fmt.Errorf("fresh Signal lifecycle changed")
	}
	if err == nil {
		_, _, err = readFreshSignalRestart(d, m, j, false)
	}
	return
}
