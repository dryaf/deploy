package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Offline recovery uses the installed private configuration, never inventory
// defaults or a state initializer. Both producer and LED coordinators share the
// installation validation used by normal backup operations.
func readBackupRecoveryInstallation(path string) (c WorkerInstallation, err error) {
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(path), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return c, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return c, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || info.Size() > safefs.MaxRecordBytes {
		return c, fmt.Errorf("backup recovery configuration must be an owned private regular file (0600), without aliases, within the size limit")
	}
	raw, err := io.ReadAll(io.LimitReader(f, safefs.MaxRecordBytes+1))
	if err != nil {
		return c, err
	}
	if err = safefs.DecodeJSON(raw, &c, safefs.MaxRecordBytes); err != nil {
		return c, err
	}
	return c, validateWorkerInstallation(c)
}
