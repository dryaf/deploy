//go:build notificationintegration

// Package ledfixture runs only a pinned offline daemon with mock hardware.
package ledfixture

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const Commit = "fbe71f9f6edd108beceb01d568a0b17b5a6eb85c"

// Start never discovers source or downloads dependencies. The explicit fixture
// must be clean and pinned; all writable state is in a disposable short path.
func Start(t *testing.T) string {
	t.Helper()
	source := os.Getenv("DEPLOY_LED_FIXTURE")
	if !filepath.IsAbs(source) {
		t.Fatal("DEPLOY_LED_FIXTURE must name an absolute pinned offline checkout")
	}
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "HEAD"}, Commit},
		{[]string{"status", "--porcelain"}, ""},
	} {
		cmd := exec.Command("git", append([]string{"-C", source}, check.args...)...)
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		b, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(b)) != check.want {
			t.Fatalf("LED fixture identity/cleanliness check failed: %v", err)
		}
	}
	dir, err := os.MkdirTemp("", "deploy-led-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket, configuration := filepath.Join(dir, "led.sock"), filepath.Join(dir, "config.toml")
	body := "[hardware]\ndriver = \"mock\"\n[led.green]\ndefault_pattern = \"off\"\n[persistence]\nenabled = true\npath = " + strconv.Quote(filepath.Join(dir, "state.json")) + "\n"
	if err = os.WriteFile(configuration, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "python3", "-m", "pistatus.daemon", "--config", configuration, "--socket", socket, "--mock")
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(source, "src"), "PYTHONDONTWRITEBYTECODE=1")
	logPath := filepath.Join(dir, "daemon.log")
	log, err := os.Create(logPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		log.Close()
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("mock daemon did not stop")
			}
		}
		log.Close()
	})
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			joined = true
			data, _ := os.ReadFile(logPath)
			t.Fatalf("mock daemon exited: %v: %s", err, data)
		default:
		}
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return socket
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(logPath)
	t.Fatal(fmt.Sprintf("mock daemon failed to start: %s", data))
	return ""
}
