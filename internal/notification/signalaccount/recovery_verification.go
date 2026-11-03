package signalaccount

import (
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/dryaf/deploy/internal/safefs"
)

// RecoveryVerification holds the account without granting runtime or maintenance
// authority. It only accepts an already completed, exactly pinned recovery.
type RecoveryVerification struct {
	mu                 sync.Mutex
	path               string
	dir                *safefs.Directory
	operation, account *safefs.Lock
	expected           RecoveryStatus
	controlIdentity    os.FileInfo
}

func OpenRecoveryVerification(path string, expected RecoveryStatus) (_ *RecoveryVerification, err error) {
	if expected.Phase != "complete" || expected.RuntimeAllowed || !safefs.DigestPattern.MatchString(expected.ResultHash) {
		return nil, fmt.Errorf("completed fenced Signal recovery required")
	}
	if err = (RecoveryRequest{expected.Operation, expected.Binding, expected.Target}).validate(); err != nil {
		return nil, err
	}
	v := &RecoveryVerification{path: path, expected: expected}
	defer func() {
		if err != nil {
			v.Close()
		}
	}()
	if v.dir, err = accountDirectory(path); err != nil {
		return nil, err
	}
	// Existing inodes only: a verifier must never repair missing coordination.
	if v.operation, err = v.dir.Lock("deploy-service-link.lock", false, false); err != nil {
		return nil, err
	}
	if v.account, err = v.dir.Lock("deploy-account.lock", true, false); err != nil {
		return nil, err
	}
	if err = v.Check(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *RecoveryVerification) Check() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dir == nil {
		return fmt.Errorf("Signal recovery verifier closed")
	}
	d, err := accountDirectory(v.path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = sameAccountDirectory(v.dir, d); err != nil {
		return err
	}
	guard, err := d.Lock("deploy-account-control.lock", false, false)
	if err != nil {
		return err
	}
	defer guard.Close()
	controlInfo, err := guard.File().Stat()
	if err != nil {
		return err
	}
	if v.controlIdentity != nil && !os.SameFile(v.controlIdentity, controlInfo) {
		return fmt.Errorf("Signal control lease replaced")
	}
	v.controlIdentity = controlInfo
	for name, lease := range map[string]*safefs.Lock{"deploy-service-link.lock": v.operation, "deploy-account.lock": v.account, "deploy-account-control.lock": guard} {
		i, e := lease.File().Stat()
		if e != nil {
			return e
		}
		st := i.Sys().(*syscall.Stat_t)
		if !i.Mode().IsRegular() || i.Mode().Perm() != 0600 || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("unsafe Signal verification lease")
		}
		if err = sameAccountLease(d, name, lease); err != nil {
			return err
		}
	}
	r, err := readRecovery(d)
	if err != nil {
		return err
	}
	if r.status() != v.expected {
		return fmt.Errorf("Signal recovery verification pins changed")
	}
	current, err := accountDirectory(v.path)
	if err != nil {
		return err
	}
	defer current.Close()
	return sameAccountDirectory(v.dir, current)
}

func (v *RecoveryVerification) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.account != nil {
		v.account.Close()
		v.account = nil
	}
	if v.operation != nil {
		v.operation.Close()
		v.operation = nil
	}
	if v.dir != nil {
		v.dir.Close()
		v.dir = nil
	}
}
