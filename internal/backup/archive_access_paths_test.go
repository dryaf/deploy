package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
)

func TestBackupAdminCommandSelectsDeclaredTargetHelper(t *testing.T) {
	for _, mode := range []string{"legacy", "home", "absolute"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			directory := filepath.Join(home, ".local/lib/deploy/backup-worker")
			s := config.ServerConfig{Host: "fixture.invalid", User: "pi"}
			if mode != "legacy" {
				directory = filepath.Join(home, "custom helper")
				declared := directory
				if mode == "home" {
					declared = "~/custom helper"
				}
				s.Desired = &config.DesiredWorkers{Backup: &config.BackupDeclaration{Paths: config.WorkerPaths{Installation: declared}}}
			}
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "deploy"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"status", "literal $(exit 19)", "a'b"}
			ssh := workerAdminCommand(s, args)
			// Exercise only the remote shell command with a disposable helper.
			// No SSH connection or operator HOME expansion occurs.
			cmd := exec.Command("sh", "-c", ssh.Args[len(ssh.Args)-1])
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != "backup-worker\n"+strings.Join(args, "\n")+"\n" {
				t.Fatal("wrong target executable or shell expansion", string(out), err)
			}
		})
	}
}
