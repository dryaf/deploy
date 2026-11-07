package install

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Native executables can be hundreds of megabytes. Stream their digest instead
// of allocating a full copy on every runtime/health check. A process-local cache
// remembers only verified executable metadata, never credentials or authority.
// Every use reopens without symlinks and checks ownership, mode, link count and
// ctime as well as mtime; replacing or rewriting a file invalidates the entry.
type freshArtifactKey struct{ path, digest string }
type freshArtifactPin struct {
	device, inode, links uint64
	size                 int64
	mode, uid, gid       uint32
	modified, changed    unix.Timespec
}
type freshArtifactCache struct {
	mu   sync.Mutex
	pins map[freshArtifactKey]freshArtifactPin
}

var freshSignalExecutables freshArtifactCache

func freshArtifactIdentity(f *os.File, input freshSignalInput) (freshArtifactPin, error) {
	var s unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &s); err != nil {
		return freshArtifactPin{}, err
	}
	if s.Mode&unix.S_IFMT != unix.S_IFREG || s.Mode&07777 != input.mode || s.Uid != uint32(os.Getuid()) || s.Nlink != 1 || s.Size <= 0 || s.Size > input.limit {
		return freshArtifactPin{}, fmt.Errorf("unsafe fresh Signal file: %s", input.name)
	}
	return freshArtifactPin{uint64(s.Dev), s.Ino, uint64(s.Nlink), s.Size, s.Mode, s.Uid, s.Gid, s.Mtim, s.Ctim}, nil
}

func (c *freshArtifactCache) verify(root string, input freshSignalInput) error {
	d, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer d.Close()
	f, err := d.Open(input.name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	before, err := freshArtifactIdentity(f, input)
	if err != nil {
		return err
	}
	key := freshArtifactKey{filepath.Join(root, input.name), input.hash}
	// Serialize cache checks with digest publication. There are only two large
	// artifacts per service; private selector/recipient files are never cached.
	c.mu.Lock()
	defer c.mu.Unlock()
	if input.mode == 0700 && c.pins != nil && c.pins[key] == before {
		return nil
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, input.limit+1))
	if err != nil {
		return err
	}
	after, err := freshArtifactIdentity(f, input)
	if err != nil {
		return err
	}
	if before != after || n != before.size || fmt.Sprintf("%x", h.Sum(nil)) != input.hash {
		return fmt.Errorf("fresh Signal file changed: %s", input.name)
	}
	if input.mode == 0700 {
		if len(c.pins) >= 64 {
			c.pins = nil
		}
		if c.pins == nil {
			c.pins = map[freshArtifactKey]freshArtifactPin{}
		}
		c.pins[key] = after
	}
	return nil
}
