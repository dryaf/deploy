package safefs

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// EnsureOwnedDirectory creates missing components with mode 0700 through held
// descriptors. Existing directories are never chmod/chowned. The returned leaf
// must belong to this UID and must not be writable by another UID/group.
func EnsureOwnedDirectory(path string) (*Directory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("clean absolute directory required")
	}
	parent := path
	for {
		_, err := os.Lstat(parent)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) || parent == "/" {
			return nil, fmt.Errorf("managed directory parent unavailable")
		}
		parent = filepath.Dir(parent)
	}
	d, err := Open(parent)
	if err != nil {
		return nil, err
	}
	if parent != path {
		relative, e := filepath.Rel(parent, path)
		if e == nil {
			e = d.Mkdir(relative)
		}
		d.Close()
		if e != nil {
			return nil, e
		}
		d, err = Open(path)
		if err != nil {
			return nil, err
		}
	}
	info, err := d.File().Stat()
	if err != nil {
		d.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
		d.Close()
		return nil, fmt.Errorf("managed directory must be owned and not writable by others")
	}
	return d, nil
}
