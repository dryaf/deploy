package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func checkLocalCapacity(destination string, required int64, storage StorageConfig) error {
	dir := filepath.Dir(destination)
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return err
	}
	reserve := storage.MinFreeBytes
	if reserve == 0 {
		reserve = 64 << 20
	}
	inodes := storage.MinFreeInodes
	if inodes == 0 {
		inodes = 1024
	}
	if required < 0 || required > 1<<62 || uint64(required+reserve) > stat.Bavail*uint64(stat.Bsize) {
		return fmt.Errorf("insufficient disk space at %s for backup, replacement and reserve", dir)
	}
	if stat.Files > 0 && uint64(inodes) > uint64(stat.Ffree) {
		return fmt.Errorf("insufficient free inodes at %s", dir)
	}
	return nil
}

func databaseDiskBytes(file string) (int64, error) {
	var size int64
	for _, suffix := range []string{"", "-wal", "-journal"} {
		info, err := os.Lstat(file + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("unsafe database path: %s", file+suffix)
		}
		if info.Size() > 1<<60 {
			return 0, fmt.Errorf("database size exceeds supported capacity estimate")
		}
		size += info.Size()
	}
	return size, nil
}
