package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
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

func getCmdOutput(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out))
}

func genFile(path string, tmplStr string, data any) {
	if dryRun {
		return
	}
	t, _ := template.New("t").Parse(tmplStr)
	f, _ := os.Create(path)
	defer f.Close()
	t.Execute(f, data)
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

func runCommand(desc string, cmd *exec.Cmd) error {
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
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s\nSTDOUT:\n%s\nSTDERR:\n%s", err, outBuf.String(), errBuf.String())
		}
		return nil
	}
	return cmd.Run()
}

func runCommandRaw(name string, args ...string) error {
	if dryRun {
		fmt.Printf("[DRY] %s %v\n", name, args)
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- SSH & Rsync with Multiplexing ---

func getSSHBaseArgs(env Environment) []string {
	args := []string{}
	// SSH Multiplexing for performance
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("deploy-%s-%s", env.User, env.Host))

	args = append(args, "-o", "ControlMaster=auto")
	args = append(args, "-o", "ControlPersist=5m")
	args = append(args, "-o", fmt.Sprintf("ControlPath=%s", socketPath))

	if env.SSHKey != "" {
		args = append(args, "-i", env.SSHKey)
	}
	args = append(args, "-p", fmt.Sprintf("%d", env.Port))
	args = append(args, fmt.Sprintf("%s@%s", env.User, env.Host))
	return args
}

func runSSH(env Environment, cmd string) error {
	args := getSSHBaseArgs(env)
	args = append(args, cmd)

	if dryRun {
		logDebug("[SSH] %s", cmd)
		return nil
	}
	c := exec.Command("ssh", args...)
	return runCommand("SSH", c)
}

func runSSHStream(env Environment, cmd string) error {
	args := getSSHBaseArgs(env)
	args = append(args, cmd)
	if dryRun {
		logDebug("[SSH-STREAM] %s", cmd)
		return nil
	}
	c := exec.Command("ssh", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func runRsync(env Environment, sources []string, dest string, extraArgs ...string) {
	if err := runRsyncSafe(env, sources, dest, extraArgs...); err != nil {
		logFatal("Rsync failed: %v", err)
	}
}

func runRsyncSafe(env Environment, sources []string, dest string, extraArgs ...string) error {
	args := []string{"-avz"}

	sshCmd := "ssh"
	// Reuse multiplexed socket
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("deploy-%s-%s", env.User, env.Host))
	sshCmd += fmt.Sprintf(" -o ControlMaster=auto -o ControlPath=%s -o ControlPersist=5m", socketPath)

	needsE := false
	if env.Port != 0 && env.Port != 22 {
		sshCmd += fmt.Sprintf(" -p %d", env.Port)
		needsE = true
	}
	if env.SSHKey != "" {
		sshCmd += fmt.Sprintf(" -i %s", env.SSHKey)
		needsE = true
	}
	if needsE || true { // Always valid to pass -e
		args = append(args, "-e", sshCmd)
	}

	args = append(args, extraArgs...)
	args = append(args, sources...)
	args = append(args, dest)

	return runCommandRaw("rsync", args...)
}

func fetchLatestGitHubRelease(repo string) (string, error) {
	resp, err := http.Get(fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo))
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
