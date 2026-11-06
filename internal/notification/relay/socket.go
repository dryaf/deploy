package relay

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Socket creation and cleanup stay relative to the retained directory. Neither
// a renamed directory nor a replacement socket may redirect authority or cleanup.
func openRelaySocket(path string) (l *net.UnixListener, check func() error, cleanup func(), err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) >= 108 {
		return nil, nil, nil, fmt.Errorf("invalid relay socket path")
	}
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	dirInfo, err := d.File().Stat()
	if err != nil {
		return nil, nil, nil, err
	}
	st, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || dirInfo.Mode().Perm() != 0700 {
		return nil, nil, nil, fmt.Errorf("relay socket directory must be private")
	}
	name := filepath.Base(path)
	bound := fmt.Sprintf("/proc/self/fd/%d/%s", d.File().Fd(), name)
	old, err := os.Lstat(bound)
	if err == nil {
		st, ok := old.Sys().(*syscall.Stat_t)
		if !ok || old.Mode()&os.ModeSocket == 0 || old.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) {
			return nil, nil, nil, fmt.Errorf("foreign relay socket")
		}
		conn, e := net.DialTimeout("unix", bound, 200*time.Millisecond)
		if e == nil {
			conn.Close()
			return nil, nil, nil, fmt.Errorf("relay socket is live")
		}
		if !errors.Is(e, syscall.ECONNREFUSED) {
			return nil, nil, nil, fmt.Errorf("relay socket liveness uncertain")
		}
		current, e := os.Lstat(bound)
		if e != nil || !os.SameFile(current, old) {
			return nil, nil, nil, fmt.Errorf("relay socket replaced")
		}
		if err = unix.Unlinkat(int(d.File().Fd()), name, 0); err != nil {
			return nil, nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, nil, err
	}
	l, err = net.ListenUnix("unix", &net.UnixAddr{Name: bound, Net: "unix"})
	if err != nil {
		return nil, nil, nil, err
	}
	l.SetUnlinkOnClose(false)
	info, err := os.Lstat(bound)
	if err != nil {
		l.Close()
		return nil, nil, nil, err
	}
	cleanup = func() {
		l.Close()
		if current, e := os.Lstat(bound); e == nil && os.SameFile(current, info) {
			_ = unix.Unlinkat(int(d.File().Fd()), name, 0)
		}
		d.Close()
	}
	if err = unix.Fchmodat(int(d.File().Fd()), name, 0600, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	check = func() error {
		current, e := os.Lstat(filepath.Dir(path))
		if e != nil {
			return e
		}
		if !os.SameFile(current, dirInfo) {
			return fmt.Errorf("relay socket directory changed")
		}
		current, e = os.Lstat(bound)
		if e != nil {
			return e
		}
		if !os.SameFile(current, info) || current.Mode().Perm() != 0600 {
			return fmt.Errorf("relay socket changed")
		}
		return nil
	}
	return l, check, cleanup, nil
}
