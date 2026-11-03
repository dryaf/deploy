package relay

import (
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"syscall"
)

func runtimeInstallationRecord(d *safefs.Directory, name string, want SignalRuntimeInstallation) (bool, error) {
	var got SignalRuntimeInstallation
	err := d.ReadJSON(name, &got)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if protocol.Hash(got) != protocol.Hash(want) {
		return false, fmt.Errorf("Signal runtime installation record changed: %s", name)
	}
	return true, nil
}
func runtimeJSONOnce[T any](d *safefs.Directory, name string, want T) error {
	var got T
	err := d.ReadJSON(name, &got)
	if os.IsNotExist(err) {
		return d.WriteJSON(name, want, false)
	}
	if err != nil {
		return err
	}
	if protocol.Hash(got) != protocol.Hash(want) {
		return fmt.Errorf("Signal runtime file changed: %s", name)
	}
	return nil
}
func absentRuntimeFile(d *safefs.Directory, name string) error {
	f, err := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err == nil {
		f.Close()
		return fmt.Errorf("unowned Signal runtime artifact: %s", name)
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func emptyRuntimeDestination(d *safefs.Directory) error { return runtimeDestinationNames(d, false) }
func onlyRuntimeLease(d *safefs.Directory) error        { return runtimeDestinationNames(d, true) }
func runtimeDestinationNames(d *safefs.Directory, lease bool) error {
	// Open a fresh directory description: Readdir on the retained handle would
	// otherwise hide entries on retries after its offset reached EOF.
	fresh, err := safefs.Open(d.File().Name())
	if err != nil {
		return err
	}
	defer fresh.Close()
	names, err := fresh.File().Readdirnames(34)
	if len(names) > 33 {
		return fmt.Errorf("Signal destination exceeds artifact budget")
	}
	if err != nil && err != io.EOF {
		return err
	}
	for _, name := range names {
		if lease && name == "inbox.lock" {
			continue
		}
		if lease && safefs.IsJSONTemporary(name) {
			continue
		}
		return fmt.Errorf("Signal destination is not empty")
	}
	return nil
}
func checkRuntimeDirectory(path string, d *safefs.Directory) error {
	current, err := privateInboxDirectory(path)
	if err != nil {
		return err
	}
	defer current.Close()
	a, err := d.File().Stat()
	if err != nil {
		return err
	}
	b, err := current.File().Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return fmt.Errorf("Signal installation directory replaced")
	}
	return nil
}
func checkRuntimeLease(d *safefs.Directory, l *safefs.Lock) error {
	if l == nil || l.File() == nil {
		return fmt.Errorf("Signal installation lease missing")
	}
	a, err := l.File().Stat()
	if err != nil {
		return err
	}
	f, err := d.Open("inbox.lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return fmt.Errorf("Signal installation lease replaced")
	}
	return nil
}
func resetRuntimeCopy(d *safefs.Directory, name string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if name == runtimeInstallImage {
			if err := removeOwnedRuntimeFile(d, name+suffix); err != nil {
				return err
			}
		} else if err := absentRuntimeFile(d, name+suffix); err != nil {
			return err
		}
	}
	return removeOwnedRuntimeFile(d, name)
}

func removeOwnedRuntimeFile(d *safefs.Directory, name string) error {
	f, err := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 34<<20 {
		return fmt.Errorf("unsafe partial Signal runtime copy")
	}
	if err = unix.Unlinkat(int(d.File().Fd()), name, 0); err != nil {
		return err
	}
	return d.File().Sync()
}
