package backup

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Retain both stable source leases across worker calls and the final local
// commit. Missing files are lost authority, never an invitation to recreate it.
func lockBackupRecovery(ctx context.Context, s *backupState) (check func() error, release func(), err error) {
	dir, err := safefs.Open(s.directory)
	if err != nil {
		return nil, nil, err
	}
	coordinator, err := dir.Lock("notification-recovery.lock", true, false)
	if err != nil {
		dir.Close()
		return nil, nil, err
	}
	notifier, err := dir.Lock("notifier.lock", true, false)
	if err != nil {
		coordinator.Close()
		dir.Close()
		return nil, nil, err
	}
	release = func() { notifier.Close(); coordinator.Close(); dir.Close() }
	check = func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for name, lock := range map[string]*safefs.Lock{"notification-recovery.lock": coordinator, "notifier.lock": notifier} {
			held, err := lock.File().Stat()
			if err != nil {
				return err
			}
			f, err := dir.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			current, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if !os.SameFile(held, current) {
				return fmt.Errorf("backup recovery lease changed: %s", name)
			}
		}
		return nil
	}
	if err = requireBackupRecoveryHistory(s); err != nil {
		release()
		return nil, nil, err
	}
	return
}
