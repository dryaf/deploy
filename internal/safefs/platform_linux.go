//go:build linux

package safefs

import (
	"golang.org/x/sys/unix"
)

func openAt(directory int, path string, flags int, mode uint32) (int, error) {
	return unix.Openat2(directory, path, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK), Mode: uint64(mode), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
}

func RenameAt(oldDirectory int, old string, newDirectory int, name string, flags uint) error {
	return unix.Renameat2(oldDirectory, old, newDirectory, name, flags)
}
