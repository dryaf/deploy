package signalaccount

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// NativeCLI binds guided linking to its selected native release, independently
// of PATH. It is retained in the link intent so retries cannot swap executables.
type NativeCLI struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

func (n NativeCLI) validate() error {
	if !filepath.IsAbs(n.Path) || filepath.Clean(n.Path) != n.Path || n.Path == "/" || strings.ContainsAny(n.Path, "\x00\r\n") || !safefs.DigestPattern.MatchString(n.SHA256) || n.Version != "0.14.9" {
		return fmt.Errorf("pinned native Signal CLI path, digest and supported version required")
	}
	return nil
}

func (n NativeCLI) open() (*os.File, error) {
	if err := n.validate(); err != nil {
		return nil, err
	}
	d, err := safefs.Open(filepath.Dir(n.Path))
	if err != nil {
		return nil, fmt.Errorf("native Signal CLI parent unavailable or unsafe")
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(n.Path), os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("native Signal CLI unavailable")
	}
	good := false
	defer func() {
		if !good {
			f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0100 == 0 || info.Size() <= 0 || info.Size() > 512<<20 {
		return nil, fmt.Errorf("native Signal CLI type, permissions or size is unsafe")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("native Signal CLI must be owned by the runtime user")
	}
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, 512<<20+1)); err != nil || fmt.Sprintf("%x", h.Sum(nil)) != n.SHA256 {
		return nil, fmt.Errorf("native Signal CLI digest changed")
	}
	binary, err := elf.NewFile(f)
	if err != nil {
		return nil, fmt.Errorf("Signal CLI must be a native ELF executable")
	}
	defer binary.Close()
	expected := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	if runtime.GOOS != "linux" || expected == 0 || binary.Machine != expected || binary.Class != elf.ELFCLASS64 || binary.Data != elf.ELFDATA2LSB || (binary.Type != elf.ET_EXEC && binary.Type != elf.ET_DYN) {
		return nil, fmt.Errorf("native Signal CLI architecture mismatch")
	}
	good = true
	return f, nil
}

// LinkNative preserves Link's account-wide lease and uncertain-outcome fence.
// The URI goes only to the supplied interactive writer, never to retained state.
// The selected executable descriptor stays open across validation and exec.
func LinkNative(ctx context.Context, path, name, operation string, native NativeCLI, out io.Writer) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	if native.Path == path || strings.HasPrefix(native.Path, path+string(os.PathSeparator)) {
		return Status{}, fmt.Errorf("native Signal CLI must be staged outside account state")
	}
	f, err := native.open()
	if err != nil {
		return Status{}, err
	}
	defer f.Close()
	command := func(l *Lease, args ...string) *exec.Cmd {
		cmd := exec.Command("/proc/self/fd/4", args...)
		cmd.ExtraFiles = []*os.File{l.File(), f}
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + path, "LANG=C.UTF-8"}
		cmd.Stderr = io.Discard
		return cmd
	}
	return (linker{now: time.Now, native: &native, prepare: func(ctx context.Context, l *Lease) error {
		var version process.Buffer
		version.Limit = 4096
		probe := command(l, "--version")
		probe.Stdout = &version
		if e := process.RunContext(ctx, probe, 15*time.Second, time.Second); e != nil || strings.TrimSpace(version.String()) != "signal-cli "+native.Version {
			return fmt.Errorf("native Signal CLI version probe failed")
		}
		return nil
	}, run: func(ctx context.Context, l *Lease, name string, out io.Writer) error {
		cmd := command(l, "--config", path, "link", "--name", name)
		cmd.Stdout = &linkOutput{writer: out, remaining: 32 << 10}
		return process.RunContext(ctx, cmd, 5*time.Minute, 5*time.Second)
	}}).link(ctx, path, name, operation, out)
}
