// This offline qualification helper runs inside the pinned Signal container,
// using only its temporary filesystem and no network or real account data.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Signal JVM inherited account lease: PASS")
}
func check() error {
	f, err := os.OpenFile("/tmp/deploy-account.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/opt/signal-cli/bin/signal-cli", "--config", "/tmp/signal-fixture", "jsonRpc", "--receive-mode=manual")
	cmd.Env = []string{"PATH=/opt/signal-cli/bin:/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "LANG=C.UTF-8", "JAVA_TOOL_OPTIONS=-Xmx256m"}
	cmd.ExtraFiles = []*os.File{f}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr // This fixture has no real account or provider data.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}()
	response := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024), 16384)
		if !scanner.Scan() {
			response <- fmt.Errorf("no Signal readiness response")
			return
		}
		var result struct {
			ID     string          `json:"id"`
			Result []any           `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &result) != nil || result.ID != "lease-check" || len(result.Error) != 0 || result.Result == nil || len(result.Result) != 0 {
			response <- fmt.Errorf("unexpected synthetic-account readiness response")
			return
		}
		response <- nil
	}()
	if _, err = io.WriteString(stdin, "{\"jsonrpc\":\"2.0\",\"method\":\"listAccounts\",\"id\":\"lease-check\"}\n"); err != nil {
		return err
	}
	select {
	case err = <-response:
		if err != nil {
			return err
		}
	case <-time.After(8 * time.Second):
		return fmt.Errorf("Signal JVM did not become ready")
	}
	executable, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", cmd.Process.Pid))
	if !strings.HasSuffix(executable, "/java") {
		return fmt.Errorf("Signal did not reach JVM execution")
	}
	var temporaryBytes int64
	if err = filepath.Walk("/tmp", func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			temporaryBytes += info.Size()
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("Signal temporary files after native-library initialization: %d bytes\n", temporaryBytes)
	select {
	case err := <-done:
		done <- err
		return fmt.Errorf("Signal process exited during startup: %v", err)
	default:
	}
	if err = f.Close(); err != nil {
		return err
	}
	contender, err := os.OpenFile("/tmp/deploy-account.lock", os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer contender.Close()
	err = syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return fmt.Errorf("Signal JVM dropped the inherited account lease")
	}
	if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
		return err
	}
	stdin.Close()
	cancel()
	select {
	case <-done:
		done <- nil
	case <-time.After(3 * time.Second):
		return fmt.Errorf("Signal process did not stop")
	}
	if err = syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("account lease remained after JVM exit: %w", err)
	}
	return nil
}
