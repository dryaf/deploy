//go:build linux

package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSharedOwnershipBlocksLegacySetupBoundaries(t *testing.T) {
	for _, marker := range []string{"fenced", "shared", "malformed"} {
		t.Run(marker, func(t *testing.T) {
			home := t.TempDir()
			setupRecoveryFixture(t, home)
			bin := t.TempDir()
			for _, name := range []string{"systemctl", "podman", "ssh"} {
				testutil.Must(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 93\n"), 0700))
			}
			t.Setenv("PATH", bin)
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			stateRoot, err := root.Sub(".local/state/deploy/backup-worker")
			testutil.Must(t, err)
			defer stateRoot.Close()
			var journal WorkerSetupJournal
			testutil.Must(t, stateRoot.ReadJSON("setup.json", &journal))
			journal.StateInitialized = true
			state, err := openBackupStateWithLease(filepath.Join(home, ".local/state/deploy/backup-worker/state"), false, false, true)
			testutil.Must(t, err)
			defer state.Close()
			if marker == "malformed" {
				_, err = state.db.Exec("INSERT INTO meta(key,value) VALUES('notification_owner','{')")
			} else {
				err = state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: marker})
			}
			testutil.Must(t, err)
			// Every guard must run before archive dereference, file restoration,
			// source probes, service-manager calls or journal mutation.
			checks := map[string]func() error{
				"setup":            func() error { return finishWorkerSetup(home, root, stateRoot, &journal, WorkerSetupRequest{}, nil) },
				"prepare":          func() error { return prepareWorkerRecovery(root, stateRoot, &journal, state, nil) },
				"rollback files":   func() error { return rollbackWorkerInstallation(root, stateRoot, &journal, state, nil) },
				"restore activity": func() error { return finishWorkerRollback(stateRoot, &journal, state) },
				"recovery":         func() error { return recoverWorkerSetupLocked(home, root, stateRoot, &journal) },
			}
			for name, check := range checks {
				if err = check(); err == nil || !strings.Contains(err.Error(), "fenced") {
					t.Fatalf("%s bypassed delivery fence: %v", name, err)
				}
			}
			if journal.RecoveryNeeded || journal.BeforeHelper != nil {
				t.Fatal("fenced lifecycle modified recovery evidence")
			}
		})
	}
}

func TestSharedOwnershipBlocksInterruptedLegacySetupRecovery(t *testing.T) {
	home := t.TempDir()
	setupRecoveryFixture(t, home)
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BACKUP_RECOVERY_CALLS\"\ncase \"$*\" in\n *is-enabled*) echo enabled;;\n *show*) echo loaded;;\nesac\n"
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("BACKUP_RECOVERY_CALLS", log)
	cmd := startInterruptedSetup(t, home)
	testutil.Must(t, cmd.Process.Kill())
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected interrupted setup")
	}
	state, err := openBackupStateWithLease(filepath.Join(home, ".local/state/deploy/backup-worker/state"), false, false, true)
	testutil.Must(t, err)
	testutil.Must(t, state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: "shared"}))
	testutil.Must(t, state.Close())
	paths := []string{
		filepath.Join(home, ".local/lib/deploy/backup-worker/deploy"),
		filepath.Join(home, recoveryFixtureConfig),
		filepath.Join(home, recoveryFixtureTrust),
		filepath.Join(home, ".local/state/deploy/backup-worker/setup.json"),
		filepath.Join(home, ".local/state/deploy/backup-worker/state/installation.pending"),
		log,
	}
	before := map[string][]byte{}
	for _, path := range paths {
		before[path], err = os.ReadFile(path)
		testutil.Must(t, err)
	}
	// A stale recovery journal is not authority to undo notification ownership.
	for i := 0; i < 2; i++ {
		if err = recoverWorkerSetup(home); err == nil || !strings.Contains(err.Error(), "fenced") {
			t.Fatal("interrupted setup bypassed delivery fence", err)
		}
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		testutil.Must(t, err)
		if !bytes.Equal(before[path], after) {
			t.Fatalf("fenced recovery changed %s", path)
		}
	}
}

func TestLegacySetupOwnershipCheckRefusesMissingState(t *testing.T) {
	home := t.TempDir()
	for _, journal := range []WorkerSetupJournal{
		{StateInitialized: true}, {RecoveryNeeded: true}, {Updating: true}, {Stage: "complete"},
	} {
		if err := requireLegacyWorkerSetup(home, &journal); err == nil {
			t.Fatal("lifecycle inferred legacy ownership from missing enrolled state", journal)
		}
	}
	testutil.Must(t, requireLegacyWorkerSetup(home, &WorkerSetupJournal{}))
}
