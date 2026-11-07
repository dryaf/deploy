package ledartifact

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/process"
)

// An explicit clean upstream fixture keeps routine tests offline. This runs the
// actual packaged daemon with mock hardware; it is not physical qualification.
func TestPinnedArchiveRunsOfflineMockDaemon(t *testing.T) {
	source := os.Getenv("DEPLOY_LED_FIXTURE")
	if source == "" {
		t.Skip("set DEPLOY_LED_FIXTURE to the pinned clean upstream checkout")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "led-archive-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	archive := filepath.Join(root, "daemon.pyz")
	builder := exec.Command(python, "../../../scripts/build-led-artifact.py", source, archive)
	if out, err := process.LimitedOutput(builder, 15*time.Second, 4096); err != nil {
		t.Fatal(err, string(out))
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(archive, hash(raw)); err != nil {
		t.Fatal(err)
	}
	again := filepath.Join(root, "repeat.pyz")
	if out, err := process.LimitedOutput(exec.Command(python, "../../../scripts/build-led-artifact.py", source, again), 15*time.Second, 4096); err != nil {
		t.Fatal(err, string(out))
	}
	repeat, err := os.ReadFile(again)
	if err != nil || hash(repeat) != hash(raw) {
		t.Fatal("archive is not reproducible", err)
	}
	socket := filepath.Join(root, "api.sock")
	config := filepath.Join(root, "config.toml")
	body := fmt.Sprintf("[api]\nsocket_path=%q\n[hardware]\ndriver=\"mock\"\n[led.green]\nenabled=true\ndevice=\"ACT\"\ndefault_pattern=\"off\"\n[led.red]\nenabled=false\n[persistence]\npath=%q\n", socket, filepath.Join(root, "states.json"))
	if err = os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", archive, "--config", config)
	cmd.Env = append(os.Environ(), "PYTHONPATH=/nonexistent-untrusted", "PISTATUS_SOCKET=")
	// A previous environment must not accidentally attach to real systemd sockets.
	var env []string
	for _, v := range cmd.Env {
		if !strings.HasPrefix(v, "LISTEN_") && !strings.HasPrefix(v, "NOTIFY_SOCKET=") && !strings.HasPrefix(v, "WATCHDOG_") {
			env = append(env, v)
		}
	}
	cmd.Env = env
	var output process.Buffer
	output.Limit = 4096
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	joined := false
	defer func() {
		if !joined {
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	client := led.Client{Socket: socket, Budget: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, status, e := led.InspectEndpoint(ctx, socket)
		if e == nil {
			if status.Daemon != "pistatusd" || status.Version != "0.1.0" || status.Degraded {
				t.Fatal("bad packaged daemon health", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("packaged daemon did not start", e, string(output.Bytes()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	states, err := client.List(ctx)
	if err != nil || len(states) != 0 {
		t.Fatal("fresh archive invented registry state", states, err)
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		joined = true
		if err != nil {
			t.Fatal("packaged daemon shutdown failed", err, string(output.Bytes()))
		}
	case <-ctx.Done():
		t.Fatal("packaged daemon did not stop")
	}
	if _, err = os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatal("owned socket remained after shutdown", err)
	}
}
