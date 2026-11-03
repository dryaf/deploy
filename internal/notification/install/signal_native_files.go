package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

const nativeSignalUnit = "deploy-notification-signal.service"

// NativeSignalServiceSpec describes an existing native user service. It does not
// authorize installing a unit, starting a receiver or using a container wrapper.
type NativeSignalServiceSpec struct {
	UnitFile         string `json:"unit_file"`
	UnitSHA256       string `json:"unit_sha256"`
	Executable       string `json:"executable"`
	ExecutableSHA256 string `json:"executable_sha256"`
	RelayConfig      string `json:"relay_config"`
	RelaySHA256      string `json:"relay_sha256"`
}

func (s NativeSignalServiceSpec) validate() error {
	for _, p := range []string{s.UnitFile, s.Executable, s.RelayConfig} {
		if !managedPath(p) {
			return fmt.Errorf("native Signal service requires clean absolute paths")
		}
	}
	for _, h := range []string{s.UnitSHA256, s.ExecutableSHA256, s.RelaySHA256} {
		if !safefs.DigestPattern.MatchString(h) {
			return fmt.Errorf("native Signal service requires exact file digests")
		}
	}
	config, err := os.UserConfigDir()
	if err != nil || !managedPath(config) || s.UnitFile != filepath.Join(config, "systemd", "user", nativeSignalUnit) {
		return fmt.Errorf("native Signal unit must be in the current user's systemd configuration directory")
	}
	return nil
}

func nativeSignalFile(path string, limit int64, private bool) (*os.File, []byte, error) {
	if !managedPath(path) {
		return nil, nil, fmt.Errorf("invalid native Signal file path")
	}
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(path), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && (info.Size() > limit || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) || (private && info.Mode().Perm() != 0600)) {
		err = fmt.Errorf("unsafe native Signal file owner, permissions or size")
	}
	var b []byte
	if err == nil {
		b, err = io.ReadAll(io.LimitReader(f, limit+1))
		if int64(len(b)) > limit {
			err = fmt.Errorf("native Signal file exceeds limit")
		}
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, b, nil
}

func nativeSignalRead(path string, limit int64, private bool) ([]byte, error) {
	f, b, err := nativeSignalFile(path, limit, private)
	if err == nil {
		f.Close()
	}
	return b, err
}

func (s NativeSignalServiceSpec) binary() (*os.File, error) {
	f, b, err := nativeSignalFile(s.Executable, maxManagedBinary, false)
	if err != nil {
		return nil, err
	}
	i, err := f.Stat()
	if err == nil && (i.Mode().Perm()&0100 == 0 || managedDigest(b) != s.ExecutableSHA256) {
		err = fmt.Errorf("native Signal executable differs from pin or is not executable")
	}
	if err == nil {
		err = checkManagedELF(b)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (s NativeSignalServiceSpec) files() (relay.Config, string, error) {
	var c relay.Config
	if err := s.validate(); err != nil {
		return c, "", err
	}
	b, err := nativeSignalRead(s.UnitFile, 65536, false)
	if err != nil || managedDigest(b) != s.UnitSHA256 {
		return c, "", fmt.Errorf("native Signal unit missing, unsafe or changed")
	}
	b, err = nativeSignalRead(s.RelayConfig, safefs.MaxRecordBytes, true)
	if err != nil || managedDigest(b) != s.RelaySHA256 {
		return c, "", fmt.Errorf("native Signal relay configuration missing, unsafe or changed")
	}
	if err = safefs.DecodeJSON(b, &c, safefs.MaxRecordBytes); err != nil {
		return c, "", fmt.Errorf("invalid native Signal relay configuration")
	}
	if c.Interactive == nil || c.Interactive.Validate() != nil {
		return c, "", fmt.Errorf("native Signal recovery requires an interactive relay")
	}
	for _, p := range []string{c.StateDir, c.Socket, c.AccountFile, c.RecipientFile, c.Interactive.InboxDir} {
		if !managedPath(p) {
			return c, "", fmt.Errorf("native Signal relay paths must be clean absolute paths")
		}
	}
	for _, p := range []string{s.UnitFile, s.Executable, s.RelayConfig, c.AccountFile, c.RecipientFile} {
		for _, root := range []string{c.StateDir, c.Interactive.InboxDir, filepath.Dir(c.Socket)} {
			if managedWithin(p, root) {
				return c, "", fmt.Errorf("native Signal inputs must be outside writable account/inbox/socket directories")
			}
		}
	}
	a, err := nativeSignalRead(c.AccountFile, 4096, true)
	if err != nil || len(a) == 0 {
		return c, "", fmt.Errorf("native Signal account credential missing or unsafe")
	}
	r, err := nativeSignalRead(c.RecipientFile, 4096, true)
	if err != nil || len(r) == 0 {
		return c, "", fmt.Errorf("native Signal recipient credential missing or unsafe")
	}
	f, err := s.binary()
	if err != nil {
		return c, "", err
	}
	f.Close()
	return c, protocol.Hash(struct {
		Spec               NativeSignalServiceSpec
		Account, Recipient string
	}{s, managedDigest(a), managedDigest(r)}), nil
}

func (s NativeSignalServiceSpec) probe(ctx context.Context) error {
	f, err := s.binary()
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "notification-worker", "__signal-recovery-probe")
	cmd.ExtraFiles = []*os.File{f}
	b, err := process.LimitedOutput(cmd, 10*time.Second, 4096)
	if err != nil {
		return fmt.Errorf("native Signal recovery capability probe failed")
	}
	var v struct {
		Version int `json:"version"`
	}
	if safefs.DecodeJSON(b, &v, 4096) != nil || v.Version != signalaccount.RecoveryVersion {
		return fmt.Errorf("native Signal executable lacks recovery fence support")
	}
	return nil
}
