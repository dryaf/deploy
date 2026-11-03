package install

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const signalVerificationIntent = "intent.json"
const signalVerificationComplete = "complete.json"
const signalVerificationGateway = "gateway-next.json"
const signalVerificationLock = "operation.lock"

type signalVerificationRoot struct {
	dir   *safefs.Directory
	lease *safefs.Lock
}

func openSignalVerificationRoot(path string, create bool) (_ *signalVerificationRoot, err error) {
	d, err := privateManagedDir(path, true)
	if err != nil {
		return nil, err
	}
	r := &signalVerificationRoot{dir: d}
	defer func() {
		if err != nil {
			r.close()
		}
	}()
	r.lease, err = d.Lock(signalVerificationLock, true, false)
	if os.IsNotExist(err) && create {
		names, e := signalVerificationNames(d)
		if e != nil {
			return nil, e
		}
		if len(names) != 0 {
			return nil, fmt.Errorf("verification root lost its operation lease")
		}
		r.lease, err = d.Lock(signalVerificationLock, true, true)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
func signalVerificationNames(d *safefs.Directory) ([]string, error) {
	f, err := d.Open(".", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}
func (r *signalVerificationRoot) close() {
	if r.lease != nil {
		r.lease.Close()
	}
	if r.dir != nil {
		r.dir.Close()
	}
}
func verificationFileIdentity(f *os.File) (uint64, uint64, error) {
	i, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	st := i.Sys().(*syscall.Stat_t)
	return uint64(st.Dev), st.Ino, nil
}
func (r *signalVerificationRoot) pin(m *SignalVerificationManifest) (err error) {
	m.RootDevice, m.RootInode, err = verificationFileIdentity(r.dir.File())
	if err != nil {
		return
	}
	m.LeaseDevice, m.LeaseInode, err = verificationFileIdentity(r.lease.File())
	return
}
func (r *signalVerificationRoot) check(m SignalVerificationManifest) error {
	if err := checkManagedDirectoryPath(r.dir, m.Spec.Root, true); err != nil {
		return err
	}
	copy := m
	if err := r.pin(&copy); err != nil {
		return err
	}
	if copy.RootDevice != m.RootDevice || copy.RootInode != m.RootInode || copy.LeaseDevice != m.LeaseDevice || copy.LeaseInode != m.LeaseInode {
		return fmt.Errorf("verification root/lease pin changed")
	}
	f, err := r.dir.Open(signalVerificationLock, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return err
	}
	st := i.Sys().(*syscall.Stat_t)
	if !i.Mode().IsRegular() || i.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || uint64(st.Dev) != m.LeaseDevice || st.Ino != m.LeaseInode {
		return fmt.Errorf("verification operation lease replaced or unsafe")
	}
	return nil
}
func readSignalVerificationManifest(d *safefs.Directory, name string) (m SignalVerificationManifest, err error) {
	raw, err := readSignalVerificationFile(d, name)
	if err != nil {
		return m, err
	}
	if err = safefs.DecodeJSON(raw, &m, safefs.MaxRecordBytes); err != nil {
		return m, err
	}
	err = m.validate()
	return
}
func (r *signalVerificationRoot) audit(m SignalVerificationManifest, complete bool) error {
	if err := r.check(m); err != nil {
		return err
	}
	files := []string{signalVerificationIntent}
	if complete {
		files = append(files, signalVerificationComplete)
	}
	for _, name := range files {
		saved, err := readSignalVerificationManifest(r.dir, name)
		if err != nil {
			return err
		}
		if protocol.Hash(saved) != protocol.Hash(m) {
			return fmt.Errorf("verification publication diverged")
		}
	}
	raw, err := readSignalVerificationFile(r.dir, signalVerificationGateway)
	if err != nil {
		return err
	}
	if managedDigest(raw) != m.NextGatewaySHA256 {
		return fmt.Errorf("verification staged gateway changed")
	}
	return nil
}

func readSignalVerificationFile(d *safefs.Directory, name string) ([]byte, error) {
	f, err := d.Open(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st := i.Sys().(*syscall.Stat_t)
	if !i.Mode().IsRegular() || i.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || i.Size() > safefs.MaxRecordBytes {
		return nil, fmt.Errorf("unsafe verification artifact")
	}
	raw, err := io.ReadAll(io.LimitReader(f, safefs.MaxRecordBytes+1))
	if len(raw) > safefs.MaxRecordBytes {
		return nil, fmt.Errorf("verification artifact exceeds limit")
	}
	return raw, err
}
