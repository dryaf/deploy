package safefs

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Locks protect stable path names, not database inodes which a rename can replace.
// Keep the inode after unlocking: unlinking permits two independent lock holders.
type Lock struct {
	file *os.File
	once sync.Once
}

func AcquireLocalLock(kind, name string) (*Lock, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	identity := filepath.Join(parent, filepath.Base(abs))
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cache, "deploy", "locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("lock directory must be private: %s", dir)
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("lock directory belongs to another user: %s", dir)
	}
	file := filepath.Join(dir, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(kind+"\x00"+identity))))
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, fmt.Errorf("unsafe local lock file: %s", file)
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != uint32(os.Getuid()) || owner.Nlink != 1 {
		f.Close()
		return nil, fmt.Errorf("unsafe ownership or hardlink on local lock: %s", file)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s lock busy for %s; retry after the current operation: %w", kind, identity, err)
	}
	return &Lock{file: f}, nil
}

// File returns the lease descriptor for explicit child inheritance. Release the
// parent's reference with Close; LOCK_UN would also unlock surviving children.
func (l *Lock) File() *os.File { return l.file }

func (l *Lock) Close() {
	if l == nil {
		return
	}
	// Closing the last reference releases flock. An explicit LOCK_UN would
	// also unlock copies intentionally inherited by a still-running capture.
	l.once.Do(func() { _ = l.file.Close() })
}

func LockProject() (*Lock, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return AcquireLocalLock("project", root)
}
