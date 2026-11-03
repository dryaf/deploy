package safefs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func ReplaceFile(dir *Directory, name string, expected, body []byte, mode uint32) error {
	// Caller holds its operation coordinator. Compare existing content again so
	// unrelated edits observed during preparation are preserved, never truncated.
	var current []byte
	file, err := dir.Open(name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err == nil {
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() {
			file.Close()
			return fmt.Errorf("managed destination is not regular")
		}
		current, err = io.ReadAll(io.LimitReader(file, 8<<20+1))
		file.Close()
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return fmt.Errorf("managed destination changed during preparation")
	}
	id, err := NewID("n")
	if err != nil {
		return err
	}
	tmp := "." + id
	f, err := dir.Open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(dir.file.Fd()), tmp, 0)
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = RenameAt(int(dir.file.Fd()), tmp, int(dir.file.Fd()), name, 0); err != nil {
		return err
	}
	return dir.file.Sync()
}
