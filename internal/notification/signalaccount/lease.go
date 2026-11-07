// Package signalaccount owns the account-wide lease shared by both Signal relays
// and maintenance children. The lock inode must never be removed or replaced.
package signalaccount

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/safefs"
)

type Lease struct {
	dir  *safefs.Directory
	lock *safefs.Lock
}

func accountDirectory(path string) (*safefs.Directory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
		return nil, fmt.Errorf("Signal account directory must be a clean absolute path")
	}
	d, err := safefs.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := d.File().Stat()
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
			err = fmt.Errorf("Signal account directory must be private and owned by the runtime user")
		}
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func control(path string, exclusive bool) (*safefs.Directory, *safefs.Lock, error) {
	d, err := accountDirectory(path)
	if err != nil {
		return nil, nil, err
	}
	native, err := nativeServicePresent(d)
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	l, err := d.Lock("deploy-account-control.lock", exclusive, !native)
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	return d, l, nil
}

func acquire(path string) (*Lease, error) {
	d, err := accountDirectory(path)
	if err != nil {
		return nil, err
	}
	l, err := d.Lock("deploy-account.lock", true, true)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("Signal account is busy; stop its relay and wait for all children: %w", err)
	}
	return &Lease{d, l}, nil
}

// AcquireRuntime refuses an unfinished or uncertain linking operation. A new
// socket path, legacy relay or restart cannot bypass account maintenance.
func AcquireRuntime(path string) (*Lease, error) {
	d, guard, err := control(path, false)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	defer guard.Close()
	if err = requireNoRecovery(d); err != nil {
		return nil, err
	}
	g, err := readMaintenance(d)
	if err != nil {
		return nil, err
	}
	if !g.runtimeAllowed() {
		return nil, fmt.Errorf("Signal relay is fenced by service maintenance")
	}
	l, err := acquire(path)
	if err != nil {
		return nil, err
	}
	h, err := readHistory(l.dir)
	if err == nil {
		err = maintenanceLinkEvidence(g, h)
	}
	if err == nil && !h.runtimeAllowed() {
		err = fmt.Errorf("Signal linking needs explicit account-state review before relay startup")
	}
	if err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (l *Lease) File() *os.File { return l.lock.File() }
func (l *Lease) Close() {
	if l == nil {
		return
	}
	// Never LOCK_UN: a CLI/JVM child inherits this open-file description.
	l.lock.Close()
	l.dir.Close()
}
