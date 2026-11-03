package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type signalPodmanProcessProof struct {
	PID              uint32
	ExecutableSHA256 string
	Mounts           []signalPodmanMount
	Account, Inbox   string
}

func (p signalPodmanProcessProof) verify() error {
	if os.Getuid() != 0 || p.PID == 0 || !safefs.DigestPattern.MatchString(p.ExecutableSHA256) || len(p.Mounts) != 6 || !managedPath(p.Account) || !managedPath(p.Inbox) {
		return fmt.Errorf("invalid rootless Signal process proof")
	}
	root := "/proc/" + strconv.FormatUint(uint64(p.PID), 10)
	before, err := os.ReadFile(root + "/stat")
	if err != nil {
		return err
	}
	owner, err := os.Stat(root)
	if err != nil || owner.Sys().(*syscall.Stat_t).Uid != 0 {
		return fmt.Errorf("Signal container process owner differs")
	}
	args, err := os.ReadFile(root + "/cmdline")
	if err != nil || !bytes.Equal(args, []byte(strings.Join(signalContainerArgv(), "\x00")+"\x00")) {
		return fmt.Errorf("Signal container process arguments differ")
	}
	f, err := os.Open(root + "/exe")
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxManagedBinary+1))
	if err != nil || len(b) > maxManagedBinary || managedDigest(b) != p.ExecutableSHA256 || checkManagedELF(b) != nil {
		return fmt.Errorf("running Signal container executable differs from pin")
	}
	i, err := f.Stat()
	if err != nil {
		return err
	}
	j, err := os.Stat(root + "/root" + podmanSignalExecutable)
	if err != nil || !os.SameFile(i, j) {
		return fmt.Errorf("Signal container executable inode replaced")
	}
	seen := map[string]bool{}
	for _, m := range p.Mounts {
		if !managedPath(m.Source) || !managedPath(m.Destination) || seen[m.Destination] {
			return fmt.Errorf("invalid Signal proof mount")
		}
		seen[m.Destination] = true
		// The host side must be canonical, while /proc/PID/root intentionally
		// traverses the container namespace. Compare the actual inode, not text.
		d, e := safefs.Open(m.Source)
		var host os.FileInfo
		if e == nil {
			host, e = d.File().Stat()
			d.Close()
		} else {
			h, _, readErr := nativeSignalFile(m.Source, 256<<10, false)
			if readErr != nil {
				return fmt.Errorf("Signal proof source unavailable")
			}
			host, e = h.Stat()
			h.Close()
		}
		if e != nil {
			return e
		}
		inside, e := os.Stat(root + "/root" + m.Destination)
		if e != nil || !os.SameFile(host, inside) {
			return fmt.Errorf("Signal runtime mount inode differs")
		}
	}
	if err = checkSignalProcessLease(root, p.Account, "/signal-state/deploy-account.lock"); err != nil {
		return err
	}
	if err = checkSignalProcessLease(root, p.Inbox, "/signal-inbox/inbox.lock"); err != nil {
		return err
	}
	after, err := os.ReadFile(root + "/stat")
	start := func(b []byte) string {
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			return ""
		}
		v := strings.Fields(string(b[i+1:]))
		if len(v) < 20 {
			return ""
		}
		return v[19]
	}
	if err != nil || start(before) == "" || start(before) != start(after) {
		return fmt.Errorf("Signal container process changed during inspection")
	}
	return nil
}

func signalPodmanProof(ctx context.Context, run signalPodmanRun, p signalPodmanProcessProof) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	// The production runner passes this installer's already-open executable
	// descriptor through unshare. A parent /proc/PID/exe link is not accessible
	// after entering the child user namespace on some kernels.
	self := "/proc/self/fd/3"
	b, err = signalPodmanCommand(ctx, run, "unshare", self, "notification-worker", "__signal-podman-proof", string(b))
	if err != nil {
		return err
	}
	var v struct {
		Version   int
		ProofHash string `json:"proof_hash"`
	}
	if safefs.DecodeJSON(b, &v, 4096) != nil || v.Version != 1 || v.ProofHash != protocol.Hash(p) {
		return fmt.Errorf("Signal container process proof failed")
	}
	return nil
}

func signalPodmanLocalCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	if command != "podman" || len(args) < 3 || args[0] != "--remote=false" || args[1] != "unshare" || args[2] != "/proc/self/fd/3" {
		return nativeSignalCommand(ctx, command, args...)
	}
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.ExtraFiles = []*os.File{f}
	b, err := process.LimitedOutput(cmd, 45*time.Second, 4096)
	if err != nil {
		return nil, fmt.Errorf("Signal rootless process proof command failed")
	}
	return b, nil
}

func runSignalPodmanProofCLI(args []string, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("Signal process proof requires one bounded request")
	}
	var p signalPodmanProcessProof
	if safefs.DecodeJSON([]byte(args[0]), &p, 32768) != nil {
		return fmt.Errorf("invalid Signal process proof request")
	}
	if err := p.verify(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Version   int    `json:"version"`
		ProofHash string `json:"proof_hash"`
	}{1, protocol.Hash(p)})
}

func runSignalPodmanCapabilityCLI(args []string, out io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("Signal Podman capability probe takes no arguments")
	}
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxManagedBinary+1))
	if err != nil || len(b) > maxManagedBinary || checkManagedELF(b) != nil {
		return fmt.Errorf("invalid Signal Podman probe executable")
	}
	return json.NewEncoder(out).Encode(struct {
		Version          int    `json:"version"`
		ExecutableSHA256 string `json:"executable_sha256"`
	}{signalaccount.RecoveryVersion, managedDigest(b)})
}
