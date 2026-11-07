package backup

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupActivityChecksLoadedUnitOwnershipAndSkipsCompletedStop(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
if [ "$2" = show ]; then
 active=active; enabled=enabled
 if [ -f "$TEST_ACTIVITY_STOPPED" ]; then active=inactive; enabled=disabled; fi
 if [ "$TEST_ACTIVITY_GENERATED" = yes ]; then enabled=generated; fi
 printf 'LoadState=loaded\nActiveState=%s\nUnitFileState=%s\nFragmentPath=%s\nSourcePath=%s\nDropInPaths=%s\nNames=%s\n' "$active" "$enabled" "$TEST_ACTIVITY_FRAGMENT" "$TEST_ACTIVITY_SOURCE" "$TEST_ACTIVITY_DROPINS" "$TEST_ACTIVITY_NAMES"
 exit
fi
[ "$2" = disable ] && [ "$3" = --now ] && [ "$4" = deploy-backup.timer ] || exit 9
printf stopped > "$TEST_ACTIVITY_STOPPED"
printf stop >> "$TEST_ACTIVITY_CALLS"
`
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TEST_ACTIVITY_STOPPED", filepath.Join(home, "stopped"))
	t.Setenv("TEST_ACTIVITY_CALLS", filepath.Join(home, "calls"))
	u := backupActivityUnit{Name: "deploy-backup.timer", Source: ".config/systemd/user/deploy-backup.timer", Activation: true}
	t.Setenv("TEST_ACTIVITY_FRAGMENT", filepath.Join(home, u.Source))
	t.Setenv("TEST_ACTIVITY_SOURCE", "")
	t.Setenv("TEST_ACTIVITY_DROPINS", "")
	t.Setenv("TEST_ACTIVITY_NAMES", u.Name)
	ctx := context.Background()
	if stopped, err := inspectBackupActivityUnits(ctx, home, []backupActivityUnit{u}); err != nil || stopped {
		t.Fatal(stopped, err)
	}
	for range 2 {
		testutil.Must(t, stopBackupActivityUnits(ctx, home, []backupActivityUnit{u}))
	}
	raw, err := os.ReadFile(filepath.Join(home, "calls"))
	testutil.Must(t, err)
	if string(raw) != "stop" {
		t.Fatal("stop repeated", string(raw))
	}
	for _, change := range [][2]string{{"TEST_ACTIVITY_FRAGMENT", "/etc/systemd/user/foreign.timer"}, {"TEST_ACTIVITY_NAMES", u.Name + " foreign.timer"}, {"TEST_ACTIVITY_DROPINS", "/etc/systemd/user/override.conf"}, {"TEST_ACTIVITY_SOURCE", "/foreign/source"}} {
		t.Run(change[0], func(t *testing.T) {
			t.Setenv(change[0], change[1])
			if err := stopBackupActivityUnits(ctx, home, []backupActivityUnit{u}); err == nil {
				t.Fatal("foreign unit accepted")
			}
		})
	}
	u = backupActivityUnit{Name: "deploy-backup.service", Source: ".config/containers/systemd/deploy-backup.container", Generated: true}
	t.Setenv("TEST_ACTIVITY_NAMES", u.Name)
	t.Setenv("TEST_ACTIVITY_GENERATED", "yes")
	t.Setenv("TEST_ACTIVITY_SOURCE", filepath.Join(home, u.Source))
	t.Setenv("TEST_ACTIVITY_FRAGMENT", filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "systemd/generator", u.Name))
	if stopped, err := inspectBackupActivityUnits(ctx, home, []backupActivityUnit{u}); err != nil || !stopped {
		t.Fatal(stopped, err)
	}
	for _, change := range [][2]string{{"TEST_ACTIVITY_SOURCE", filepath.Join(home, "foreign.container")}, {"TEST_ACTIVITY_FRAGMENT", "/tmp/generator/deploy-backup.service"}} {
		t.Run(change[0]+"-generated", func(t *testing.T) {
			t.Setenv(change[0], change[1])
			if _, err := inspectBackupActivityUnits(ctx, home, []backupActivityUnit{u}); err == nil {
				t.Fatal("foreign generated service accepted")
			}
		})
	}
}
