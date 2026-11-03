//go:build notificationintegration

package install

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestSignalRecoveryFenceNativeExecutable(t *testing.T) {
	c, spec, svc := signalServiceRecoveryFixture(t)
	_, status, err := InstallSignalRecoveryWithService(context.Background(), c, spec, svc)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	binary := filepath.Join(base, "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, e := process.LimitedOutput(build, time.Minute, 16384); e != nil {
		t.Fatal(e, string(out))
	}
	marker := filepath.Join(base, "cli-called")
	if err = os.WriteFile(filepath.Join(base, "signal-cli"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(binary, append([]string{"notification-worker"}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+base+":"+os.Getenv("PATH"))
		var stdout, stderr process.Buffer
		stdout.Limit, stderr.Limit = 16384, 4096
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := process.Run(cmd, 20*time.Second, time.Second)
		return append(stdout.Bytes(), stderr.Bytes()...), err
	}
	raw, err := run("__signal-recovery-probe")
	var cap struct {
		Version int `json:"version"`
	}
	if err != nil || json.Unmarshal(raw, &cap) != nil || cap.Version != signalaccount.RecoveryVersion {
		t.Fatal(err, string(raw))
	}
	raw, err = run("signal-recovery-status", "--local", "--state-dir", spec.AccountState)
	var got signalaccount.RecoveryStatus
	if err != nil || json.Unmarshal(raw, &got) != nil || got != status {
		t.Fatal(err, string(raw))
	}
	id, _ := safefs.NewID("n")
	out, err := run("__signal-link", "--local", "--state-dir", spec.AccountState, "--operation", id, "--name", "fixture")
	if err == nil || !strings.Contains(string(out), "fenced by recovery") {
		t.Fatal("native linking bypassed fence", err, string(out))
	}
	// Both legacy and interactive relay startup share the account gate, before
	// binding a socket or invoking signal-cli. No provider credential is available.
	account, recipient := filepath.Join(base, "account"), filepath.Join(base, "recipient")
	for _, name := range []string{account, recipient} {
		if err = os.WriteFile(name, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rc := relay.Config{StateDir: spec.AccountState, Socket: filepath.Join(base, "relay.sock"), AccountFile: account, RecipientFile: recipient}
	for _, interactive := range []bool{false, true} {
		if interactive {
			rc.Interactive = &relay.InteractiveConfig{Version: "0.14.9", Account: c.Signal.Account, InboxDir: spec.RelayDestination, Destinations: c.Worker.Interaction.Destinations}
		}
		data, e := json.Marshal(rc)
		if e != nil {
			t.Fatal(e)
		}
		config := filepath.Join(base, "relay.json")
		if e = os.WriteFile(config, data, 0600); e != nil {
			t.Fatal(e)
		}
		out, e = run("__signal-relay", "--local", "--config", config)
		if e == nil || !strings.Contains(string(out), "fenced by recovery") {
			t.Fatal("native relay bypassed fence", e, string(out))
		}
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("signal-cli invoked", err)
	}
	if _, err = os.Stat(rc.Socket); !os.IsNotExist(err) {
		t.Fatal("relay socket created", err)
	}
}
