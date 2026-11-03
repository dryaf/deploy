package safefs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Every archive path is relative to a held directory descriptor. Linux openat2
// refuses symlinks at every component, including in a concurrent path swap.
type Directory struct{ file *os.File }

const MaxRecordBytes = 8 << 20

const RenameNoReplace = 1

// Keep cleanup recognition identical to WriteJSON's atomic publication names.
func IsJSONTemporary(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".json") &&
		ValidID(strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json"), "n")
}

func Open(path string) (*Directory, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(root)
	relative := strings.TrimPrefix(absolute, "/")
	if relative == "" {
		return nil, fmt.Errorf("filesystem root is not an archive directory")
	}
	fd, err := openAt(root, relative, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open managed directory without symlinks (Linux openat2 required): %w", err)
	}
	return &Directory{os.NewFile(uintptr(fd), absolute)}, nil
}

// File exposes the open directory for descriptor-relative operations. The
// Directory retains ownership of the handle.
func (d *Directory) File() *os.File { return d.file }

func (d *Directory) Close() error { return d.file.Close() }

func (d *Directory) Open(relative string, flags int, mode uint32) (*os.File, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, "../") {
		return nil, fmt.Errorf("invalid managed relative path")
	}
	fd, err := openAt(int(d.file.Fd()), relative, flags, mode)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(d.file.Name(), relative))
	if flags&unix.O_DIRECTORY == 0 {
		info, e := f.Stat()
		if e != nil {
			f.Close()
			return nil, e
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || info.Mode().Perm()&0022 != 0 {
			f.Close()
			return nil, fmt.Errorf("unsafe managed file type, permissions or hardlink")
		}
	}
	return f, nil
}

func (d *Directory) Sub(relative string) (*Directory, error) {
	f, err := d.Open(relative, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	return &Directory{f}, nil
}

func (d *Directory) Mkdir(relative string) error {
	parts := strings.Split(relative, "/")
	current := d
	var owned []*Directory
	defer func() {
		for _, v := range owned {
			v.Close()
		}
	}()
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("unsafe directory component")
		}
		if err := unix.Mkdirat(int(current.file.Fd()), part, 0700); err != nil && err != unix.EEXIST {
			return err
		}
		next, err := current.Sub(part)
		if err != nil {
			return err
		}
		if err = current.file.Sync(); err != nil {
			next.Close()
			return err
		}
		owned = append(owned, next)
		current = next
	}
	return current.file.Sync()
}

func (d *Directory) ReadJSON(relative string, dest any) error {
	f, err := d.Open(relative, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() > MaxRecordBytes {
		return fmt.Errorf("invalid managed JSON file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxRecordBytes+1))
	if err != nil {
		return err
	}
	return DecodeJSON(data, dest, MaxRecordBytes)
}

func (d *Directory) WriteJSON(relative string, value any, replace bool) error {
	parent, err := d.Sub(filepath.Dir(relative))
	if err != nil {
		return err
	}
	defer parent.Close()
	id, err := NewID("n")
	if err != nil {
		return err
	}
	tmp := "." + id + ".json"
	f, err := parent.Open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(parent.file.Fd()), tmp, 0)
	data, err := json.Marshal(value)
	if err != nil {
		f.Close()
		return err
	}
	if len(data) > MaxRecordBytes {
		f.Close()
		return fmt.Errorf("managed JSON exceeds bound")
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	flags := uint(RenameNoReplace)
	if replace {
		flags = 0
	}
	if err = RenameAt(int(parent.file.Fd()), tmp, int(parent.file.Fd()), filepath.Base(relative), flags); err != nil {
		return err
	}
	return parent.file.Sync()
}

func (d *Directory) Rename(old string, dest *Directory, new string) error {
	if strings.Contains(old, "/") || strings.Contains(new, "/") || old == "." || new == "." || old == ".." || new == ".." {
		return fmt.Errorf("rename requires basename components")
	}
	if err := RenameAt(int(d.file.Fd()), old, int(dest.file.Fd()), new, RenameNoReplace); err != nil {
		return err
	}
	if err := dest.file.Sync(); err != nil {
		return err
	}
	return d.file.Sync()
}

func (d *Directory) Lock(name string, exclusive, create bool) (*Lock, error) {
	flags := unix.O_RDONLY | unix.O_NONBLOCK
	if create {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_NONBLOCK
	}
	f, err := d.Open(name, flags, func() uint32 {
		if create {
			return 0600
		}
		return 0
	}())
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("invalid archive lock file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 || info.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, fmt.Errorf("unsafe managed lock permissions or hardlink")
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	if err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("managed scope busy: %w", err)
	}
	if create {
		if err = f.Sync(); err == nil {
			parent, openErr := d.Sub(filepath.Dir(name))
			if openErr != nil {
				err = openErr
			} else {
				err = parent.file.Sync()
				parent.Close()
			}
		}
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	return &Lock{file: f}, nil
}

func (d *Directory) FilesystemID() (string, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(d.file.Fd()), &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x", stat.Type, uint32(stat.Fsid.Val[0]), uint32(stat.Fsid.Val[1])), nil
}

func (d *Directory) Capacity(required int64) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(d.file.Fd()), &stat); err != nil {
		return err
	}
	if required < 0 || stat.Bsize <= 0 || stat.Bavail < uint64(required)/uint64(stat.Bsize)+1 || (stat.Files > 0 && stat.Ffree < 32) {
		return fmt.Errorf("insufficient archive space or inodes")
	}
	return nil
}
