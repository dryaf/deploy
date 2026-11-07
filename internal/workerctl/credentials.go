package workerctl

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// ReadCredentialInput returns bounded private bytes. Callers must keep them in
// private stdin/descriptors and never put them in errors, argv or public digests.
func ReadCredentialInput(file string) ([]byte, error) {
	dir, err := safefs.Open(filepath.Dir(file))
	if err != nil {
		return nil, fmt.Errorf("credential input unavailable")
	}
	defer dir.Close()
	f, err := dir.Open(filepath.Base(file), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("credential input unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("credential input unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 4096 {
		return nil, fmt.Errorf("credential input must be an owned private file of 1..4096 bytes")
	}
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(body) == 0 || len(body) > 4096 || strings.ContainsAny(string(body), "\x00\r") {
		return nil, fmt.Errorf("invalid credential input")
	}
	return body, nil
}

// StageCredentialInput never replaces an active credential. The returned local
// reference is saved to YAML; apply later transfers it through the domain's fence.
func StageCredentialInput(inventory, server, provider, field string, body []byte) (string, error) {
	if !safeStepName(provider) || !config.ValidInventoryName(server) || !safeStepName(strings.ReplaceAll(field, "_", "-")) || len(body) == 0 || len(body) > 4096 || strings.ContainsAny(string(body), "\x00\r\n") {
		return "", fmt.Errorf("invalid credential input")
	}
	absolute, err := filepath.Abs(inventory)
	if err != nil {
		return "", err
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("operator config directory unavailable")
	}
	if !filepath.IsAbs(base) || filepath.Clean(base) != base || base == "/" {
		return "", fmt.Errorf("operator config directory must be an absolute non-root path")
	}
	root, err := ensureConfigDirectory(base)
	if err != nil {
		return "", err
	}
	defer root.Close()
	scope := fmt.Sprintf("%x", sha256.Sum256([]byte(absolute)))
	parts := []string{"deploy", "worker-inputs", scope, server}
	current := root
	for index, part := range parts {
		if err = unix.Mkdirat(int(current.File().Fd()), part, 0700); err != nil && err != unix.EEXIST {
			return "", fmt.Errorf("cannot create private input directory")
		}
		next, openErr := current.Sub(part)
		if openErr != nil {
			return "", fmt.Errorf("unsafe private input directory")
		}
		defer next.Close()
		info, statErr := next.File().Stat()
		if statErr != nil {
			return "", statErr
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || (index == 0 && info.Mode().Perm()&0022 != 0) || (index != 0 && info.Mode().Perm() != 0700) {
			return "", fmt.Errorf("credential staging directories must be owned and mode 0700")
		}
		if err = current.File().Sync(); err != nil {
			return "", err
		}
		current = next
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return "", err
	}
	name := provider + "-" + field + "-" + id
	f, err := current.Open(name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return "", fmt.Errorf("cannot stage private input")
	}
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("private input staging incomplete")
	}
	if err = current.File().Sync(); err != nil {
		return "", err
	}
	return filepath.Join(base, "deploy", "worker-inputs", scope, server, name), nil
}

func ensureConfigDirectory(base string) (*safefs.Directory, error) {
	parent := base
	for {
		_, err := os.Lstat(parent)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) || parent == "/" {
			return nil, fmt.Errorf("operator config directory unavailable")
		}
		parent = filepath.Dir(parent)
	}
	dir, err := safefs.Open(parent)
	if err != nil {
		return nil, err
	}
	if parent != base {
		relative, relErr := filepath.Rel(parent, base)
		if relErr == nil {
			relErr = dir.Mkdir(relative)
		}
		dir.Close()
		if relErr != nil {
			return nil, relErr
		}
		dir, err = safefs.Open(base)
		if err != nil {
			return nil, err
		}
	}
	info, err := dir.File().Stat()
	if err != nil {
		dir.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
		dir.Close()
		return nil, fmt.Errorf("operator config directory must be owned and not writable by others")
	}
	return dir, nil
}
