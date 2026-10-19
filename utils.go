package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"
)

const (
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Blue   = "\033[34m"
	Gray   = "\033[37m"
)

func logFatal(f string, a ...any)   { fmt.Printf(Red+"[FATAL] "+Reset+f+"\n", a...); os.Exit(1) }
func logInfo(f string, a ...any)    { fmt.Printf(Blue+"[INFO] "+Reset+f+"\n", a...) }
func logSuccess(f string, a ...any) { fmt.Printf(Green+"[DONE] "+Reset+f+"\n", a...) }
func logWarn(f string, a ...any)    { fmt.Printf(Yellow+"[WARN] "+Reset+f+"\n", a...) }
func logError(f string, a ...any)   { fmt.Printf(Red+"[ERR] "+Reset+f+"\n", a...) }
func logDebug(f string, a ...any) {
	if verbose {
		fmt.Printf(Gray+f+Reset+"\n", a...)
	}
}

func confirm(prompt string) bool {
	if dryRun {
		return true
	}
	fmt.Printf("%s [y/N]: ", prompt)
	r := bufio.NewReader(os.Stdin)
	res, _ := r.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(res)) == "y"
}

func prompt(label string) string {
	fmt.Printf("%s: ", label)
	r := bufio.NewReader(os.Stdin)
	res, _ := r.ReadString('\n')
	return strings.TrimSpace(res)
}

func getCmdOutput(name string, args ...string) string {
	out, _ := boundedOutput(exec.Command(name, args...), 2*time.Minute)
	return strings.TrimSpace(string(out))
}

func genFile(path string, tmplStr string, data any) error {
	if dryRun {
		return nil
	}
	t, err := template.New("t").Parse(tmplStr)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0600)
}

func runCommand(desc string, cmd *exec.Cmd) error {
	return runCommandBudget(desc, cmd, 15*time.Minute, 10*time.Second)
}

func runCommandBudget(desc string, cmd *exec.Cmd, duration, grace time.Duration) error {
	if dryRun {
		logDebug("[DRY] %s", cmd.String())
		return nil
	}
	if verbose {
		logDebug("[EXEC] %s", cmd.String())
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := runBounded(cmd, duration, grace); err != nil {
			return fmt.Errorf("%s: %w\nSTDOUT:\n%s\nSTDERR:\n%s", desc, err, outBuf.String(), errBuf.String())
		}
		return nil
	}
	return runBounded(cmd, duration, grace)
}

func runCommandRaw(name string, args ...string) error {
	if dryRun {
		fmt.Printf("[DRY] %s %v\n", name, args)
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return runBounded(cmd, 2*time.Minute, 10*time.Second)
}

// --- SSH & Rsync with Multiplexing ---

func getSSHBaseArgs(env Environment) []string {
	args := []string{}
	// Ports and identities distinguish environments behind the same hostname.
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s", env.User, env.Host, sshPort(env), env.SSHKey, env.KnownHostsFile)))
	socketPath := filepath.Join(sshControlDir(), fmt.Sprintf("%x", key[:12]))

	args = append(args, "-o", "ControlMaster=auto")
	args = append(args, "-o", "BatchMode=yes")
	args = append(args, "-o", fmt.Sprintf("ConnectTimeout=%d", max(1, int(budget(env.Timeouts.Connect, "10s").Seconds()))), "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3", "-o", "StrictHostKeyChecking=yes")
	args = append(args, "-o", "ControlPersist=5m")
	args = append(args, "-o", fmt.Sprintf("ControlPath=%s", socketPath))
	if env.KnownHostsFile != "" {
		args = append(args, "-o", "UserKnownHostsFile="+env.KnownHostsFile)
	}

	if env.SSHKey != "" {
		args = append(args, "-i", env.SSHKey)
	}
	args = append(args, "-p", fmt.Sprintf("%d", sshPort(env)))
	args = append(args, fmt.Sprintf("%s@%s", env.User, env.Host))
	return args
}

func sshControlDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("deploy-ssh-%d", os.Getuid()))
}

func prepareSSHControlDir() error {
	dir := sshControlDir()
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

func runSSH(env Environment, cmd string) error {
	args := getSSHBaseArgs(env)
	args = append(args, cmd)

	if dryRun {
		logDebug("[SSH] %s", cmd)
		return nil
	}
	if err := prepareSSHControlDir(); err != nil {
		return err
	}
	c := exec.Command("ssh", args...)
	return runCommandBudget("SSH", c, sshBudget(env), budget(env.Timeouts.ShutdownGrace, "10s"))
}

func runSSHStream(env Environment, cmd string) error {
	args := getSSHBaseArgs(env)
	args = append(args, cmd)
	if dryRun {
		logDebug("[SSH-STREAM] %s", cmd)
		return nil
	}
	if err := prepareSSHControlDir(); err != nil {
		return err
	}
	c := exec.Command("ssh", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return runBounded(c, sshBudget(env), budget(env.Timeouts.ShutdownGrace, "10s"))
}

func runRsync(env Environment, sources []string, dest string, extraArgs ...string) {
	if err := runRsyncSafe(env, sources, dest, extraArgs...); err != nil {
		logFatal("Rsync failed: %v", err)
	}
}

// shellQuote quotes a single POSIX shell argument, including empty strings.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func runSSHOutput(env Environment, script string) (string, error) {
	if dryRun {
		return "", nil
	}
	return runSSHOutputRead(env, script)
}

func runSSHOutputRead(env Environment, script string) (string, error) {
	if err := prepareSSHControlDir(); err != nil {
		return "", err
	}
	args := append(getSSHBaseArgs(env), "bash -c "+shellQuote(script))
	cmd := exec.Command("ssh", args...)
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs
	if err := runBounded(cmd, sshBudget(env), budget(env.Timeouts.ShutdownGrace, "10s")); err != nil {
		return out.String(), fmt.Errorf("SSH: %w: %s", err, strings.TrimSpace(errs.String()))
	}
	if errs.Len() > 0 {
		fmt.Fprint(os.Stderr, errs.String())
	}
	return out.String(), nil
}

func runRsyncSafe(env Environment, sources []string, dest string, extraArgs ...string) error {
	// Secluded arguments bypass shell tilde expansion. Keep home-relative paths.
	if colon := strings.Index(dest, "]:"); colon >= 0 {
		dest = dest[:colon+2] + strings.TrimPrefix(dest[colon+2:], "~/")
	} else if colon := strings.Index(dest, ":"); colon >= 0 {
		dest = dest[:colon+1] + strings.TrimPrefix(dest[colon+1:], "~/")
	}
	args := []string{"-az", "--protect-args"}
	sshArgs := getSSHBaseArgs(env)
	transport := []string{"ssh"}
	for _, a := range sshArgs[:len(sshArgs)-1] {
		transport = append(transport, shellQuote(a))
	}
	args = append(args, "-e", strings.Join(transport, " "))
	args = append(args, extraArgs...)
	args = append(args, "--")
	args = append(args, sources...)
	args = append(args, dest)
	// Never print payload contents or verbose rsync file listings.
	if dryRun {
		return nil
	}
	if err := prepareSSHControlDir(); err != nil {
		return err
	}
	cmd := exec.Command("rsync", args...)
	cmd.Stderr = os.Stderr
	return runBounded(cmd, budget(env.Timeouts.Transfer, "10m"), budget(env.Timeouts.ShutdownGrace, "10s"))
}

func fetchLatestGitHubRelease(repo string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.TagName, nil
}

func sshPort(env Environment) int {
	if env.Port == 0 {
		return 22
	}
	return env.Port
}
