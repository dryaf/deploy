package main

import (
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
