package safefs

import (
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

func ReadFile(dir *Directory, name string, limit int64) ([]byte, error) {
	f, err := dir.Open(name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("managed file is not regular")
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("managed file exceeds bound")
	}
	return body, nil
}
