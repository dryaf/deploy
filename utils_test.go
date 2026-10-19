package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetSSHBaseArgs(t *testing.T) {
	env := Environment{
		Host:   "host.com",
		User:   "user",
		Port:   2222,
		SSHKey: "key.pem",
	}

	args := getSSHBaseArgs(env)
	cmd := strings.Join(args, " ")

	if !strings.Contains(cmd, "-p 2222") {
		t.Errorf("Expected port 2222 in args: %s", cmd)
	}
	if !strings.Contains(cmd, "-i key.pem") {
		t.Errorf("Expected identity key in args: %s", cmd)
	}
	if !strings.Contains(cmd, "user@host.com") {
		t.Errorf("Expected user@host in args: %s", cmd)
	}
	if !strings.Contains(cmd, "ControlMaster=auto") {
		t.Errorf("Expected ControlMaster in args: %s", cmd)
	}
}

func TestShellQuoteKeepsArgumentsLiteral(t *testing.T) {
	for _, value := range []string{"", "space and 'single' quotes", "$(touch should-not-exist); `echo nope`", "a\nb", "$HOME%literal"} {
		cmd := exec.Command("bash", "-c", "printf '%s' "+shellQuote(value))
		out, err := cmd.Output()
		if err != nil || string(out) != value {
			t.Fatalf("roundtrip %q: %q %v", value, out, err)
		}
	}
}

func TestRsyncTransportQuotesSSHKey(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "rsync"), []byte("#!/bin/bash\nprintf '%s\\0' \"$@\" > \"$ARG_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("ARG_LOG", log)
	env := Environment{User: "bob", Host: "example.com", Port: 2222, SSHKey: "/tmp/a key'$(false)"}
	if err := runRsyncSafe(env, []string{"./file with spaces"}, "bob@example.com:/tmp/remote dir/"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(data), "\x00")
	var transport string
	for i, a := range args {
		if a == "-e" {
			transport = args[i+1]
		}
	}
	if !strings.Contains(transport, shellQuote(env.SSHKey)) {
		t.Fatal(transport)
	}
	if !strings.Contains(string(data), "--protect-args\x00") {
		t.Fatal(args)
	}
}

func TestSSHOutputSeparatesDiagnostics(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/bash\nprintf 'machine-result'\nprintf 'diagnostic' >&2\nexit \"${SSH_TEST_EXIT:-0}\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	e := Environment{User: "bob", Host: "example.com"}
	out, err := runSSHOutput(e, "true")
	if err != nil || out != "machine-result" {
		t.Fatal(out, err)
	}
	t.Setenv("SSH_TEST_EXIT", "7")
	out, err = runSSHOutput(e, "false")
	if err == nil || !strings.Contains(err.Error(), "diagnostic") || out != "machine-result" {
		t.Fatal(out, err)
	}
}
