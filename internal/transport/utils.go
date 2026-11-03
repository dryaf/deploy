package transport

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/ui"
)

// --- SSH & Rsync with Multiplexing ---

func GetSSHBaseArgs(env config.Environment) []string {
	args := []string{}
	// Ports and identities distinguish environments behind the same hostname.
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s", env.User, env.Host, config.SSHPort(env), env.SSHKey, env.KnownHostsFile)))
	socketPath := filepath.Join(SshControlDir(), fmt.Sprintf("%x", key[:12]))

	args = append(args, "-o", "ControlMaster=auto")
	args = append(args, "-o", "BatchMode=yes")
	args = append(args, "-o", fmt.Sprintf("ConnectTimeout=%d", max(1, int(config.Budget(env.Timeouts.Connect, "10s").Seconds()))), "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3", "-o", "StrictHostKeyChecking=yes")
	args = append(args, "-o", "ControlPersist=5m")
	args = append(args, "-o", fmt.Sprintf("ControlPath=%s", socketPath))
	if env.KnownHostsFile != "" {
		args = append(args, "-o", "UserKnownHostsFile="+env.KnownHostsFile)
	}

	if env.SSHKey != "" {
		args = append(args, "-i", env.SSHKey)
	}
	args = append(args, "-p", fmt.Sprintf("%d", config.SSHPort(env)))
	args = append(args, fmt.Sprintf("%s@%s", env.User, env.Host))
	return args
}

func SshControlDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("deploy-ssh-%d", os.Getuid()))
}

func PrepareSSHControlDir() error {
	dir := SshControlDir()
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	s, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if owner, ok := s.Sys().(*syscall.Stat_t); !ok || owner.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("SSH control directory belongs to another user")
	}
	if !s.IsDir() || s.Mode().Perm() != 0700 {
		return fmt.Errorf("SSH control directory must be a private directory: %s", dir)
	}
	return nil
}

func RunSSH(env config.Environment, cmd string) error {
	args := GetSSHBaseArgs(env)
	args = append(args, cmd)

	if ui.DryRun {
		ui.LogDebug("[SSH] %s", cmd)
		return nil
	}
	if err := PrepareSSHControlDir(); err != nil {
		return err
	}
	c := exec.Command("ssh", args...)
	return process.RunCommandBudget("SSH", c, SshBudget(env), config.Budget(env.Timeouts.ShutdownGrace, "10s"))
}

func RunSSHStream(env config.Environment, cmd string) error {
	args := GetSSHBaseArgs(env)
	args = append(args, cmd)
	if ui.DryRun {
		ui.LogDebug("[SSH-STREAM] %s", cmd)
		return nil
	}
	if err := PrepareSSHControlDir(); err != nil {
		return err
	}
	c := exec.Command("ssh", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return process.Run(c, SshBudget(env), config.Budget(env.Timeouts.ShutdownGrace, "10s"))
}

func RunRsync(env config.Environment, sources []string, dest string, extraArgs ...string) {
	if err := RunRsyncSafe(env, sources, dest, extraArgs...); err != nil {
		ui.LogFatal("Rsync failed: %v", err)
	}
}

// shellQuote quotes a single POSIX shell argument, including empty strings.
func ShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func RunSSHOutput(env config.Environment, script string) (string, error) {
	if ui.DryRun {
		return "", nil
	}
	return RunSSHOutputRead(env, script)
}

func RunSSHOutputRead(env config.Environment, script string) (string, error) {
	if err := PrepareSSHControlDir(); err != nil {
		return "", err
	}
	args := append(GetSSHBaseArgs(env), "bash -c "+ShellQuote(script))
	cmd := exec.Command("ssh", args...)
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs
	if err := process.Run(cmd, SshBudget(env), config.Budget(env.Timeouts.ShutdownGrace, "10s")); err != nil {
		return out.String(), fmt.Errorf("SSH: %w: %s", err, strings.TrimSpace(errs.String()))
	}
	if errs.Len() > 0 {
		fmt.Fprint(os.Stderr, errs.String())
	}
	return out.String(), nil
}

func RunRsyncSafe(env config.Environment, sources []string, dest string, extraArgs ...string) error {
	// Secluded arguments bypass shell tilde expansion. Keep home-relative paths.
	if colon := strings.Index(dest, "]:"); colon >= 0 {
		dest = dest[:colon+2] + strings.TrimPrefix(dest[colon+2:], "~/")
	} else if colon := strings.Index(dest, ":"); colon >= 0 {
		dest = dest[:colon+1] + strings.TrimPrefix(dest[colon+1:], "~/")
	}
	args := []string{"-az", "--protect-args"}
	sshArgs := GetSSHBaseArgs(env)
	transport := []string{"ssh"}
	for _, a := range sshArgs[:len(sshArgs)-1] {
		transport = append(transport, ShellQuote(a))
	}
	args = append(args, "-e", strings.Join(transport, " "))
	args = append(args, extraArgs...)
	args = append(args, "--")
	args = append(args, sources...)
	args = append(args, dest)
	// Never print payload contents or verbose rsync file listings.
	if ui.DryRun {
		return nil
	}
	if err := PrepareSSHControlDir(); err != nil {
		return err
	}
	cmd := exec.Command("rsync", args...)
	cmd.Stderr = os.Stderr
	return process.Run(cmd, config.Budget(env.Timeouts.Transfer, "10m"), config.Budget(env.Timeouts.ShutdownGrace, "10s"))
}
