//go:build notificationintegration

package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestSignalVerificationNativeExecutable(t *testing.T) {
	s := signalVerificationFixture(t)
	base := t.TempDir()
	binary := filepath.Join(base, "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, e := process.LimitedOutput(build, time.Minute, 16384); e != nil {
		t.Fatal(e, string(out))
	}
	marker := filepath.Join(base, "cli-called")
	if err := os.WriteFile(filepath.Join(base, "signal-cli"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(base, "spec.json")
	raw, _ := json.Marshal(s)
	if err := os.WriteFile(spec, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "notification-worker", "signal-verification-prepare", "--local", "--spec", spec)
	cmd.Env = append(os.Environ(), "PATH="+base+":"+os.Getenv("PATH"))
	out, err := process.LimitedOutput(cmd, 30*time.Second, safefs.MaxRecordBytes)
	if err != nil {
		t.Fatal(err, string(out))
	}
	var m SignalVerificationManifest
	if err = json.Unmarshal(out, &m); err != nil || m.validate() != nil {
		t.Fatal(err, string(out))
	}
	for attempt := 0; attempt < 2; attempt++ {
		cmd = exec.Command(binary, "notification-worker", "signal-verification-serve", "--local", "--root", s.Root, "--manifest-hash", m.ManifestHash)
		cmd.Env = append(os.Environ(), "PATH="+base+":"+os.Getenv("PATH"))
		var output process.Buffer
		output.Limit = 16384
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		child := cmd.Process
		t.Cleanup(func() { _ = child.Kill() })
		h := waitSignalVerification(t, signalVerificationHTTP(filepath.Join(s.Root, "api.sock")), done)
		if !h.VerificationOnly || h.IntakeEnabled || h.ManifestHash != m.ManifestHash {
			t.Fatal(h)
		}
		if attempt == 0 {
			// Leave a real stale Unix socket behind, then restart the enrolled endpoint.
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
		} else if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
			if attempt == 1 && err != nil {
				t.Fatal(err, output.String())
			}
			if attempt == 0 && err == nil {
				t.Fatal("missing SIGKILL")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("native verifier did not stop")
		}
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("verification invoked signal-cli", err)
	}
	if _, err = os.Stat(filepath.Join(s.Root, "api.sock")); !os.IsNotExist(err) {
		t.Fatal("graceful restart left socket", err)
	}
}
