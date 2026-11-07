package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func manualRunFixture(t *testing.T) (string, *backupState, WorkerSetupJournal) {
	t.Helper()
	home, state, j, _ := declaredQualificationFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	testutil.Must(t, root.Mkdir(".config/containers/systemd"))
	const file = ".config/containers/systemd/deploy-backup.container"
	raw := []byte("fixture owned Quadlet\n")
	testutil.Must(t, os.WriteFile(filepath.Join(home, file), raw, 0600))
	j.OwnedFiles[file] = sharedSetupDigest(raw)
	testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
	return home, state, j
}

func TestManualDeclaredBackupDoesNotActivateOrClaimSuccess(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(strconv.FormatBool(lost), func(t *testing.T) {
			home, _, j := manualRunFixture(t)
			stopped, starts := true, 0
			deps := manualBackupDependencies{
				stopped: func(_ context.Context, actual string, unit backupActivityUnit) (bool, error) {
					if actual != home || unit.Name != "deploy-backup.service" || !unit.Generated || unit.Activation || unit.Source != ".config/containers/systemd/deploy-backup.container" {
						t.Fatal("manual run targeted a different service", unit)
					}
					return stopped, nil
				},
				start: func(context.Context) error {
					starts++
					stopped = false
					if lost {
						return errors.New("lost start acknowledgement")
					}
					return nil
				},
			}
			got, err := requestDeclaredBackup(context.Background(), home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, deps)
			if (err != nil) != lost || starts != 1 || !lost && got.State != "start_requested" || lost && got.State != "start_unconfirmed" {
				t.Fatal(got, starts, err)
			}
			got, err = requestDeclaredBackup(context.Background(), home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, deps)
			if err != nil || got.State != "running_or_busy" || starts != 1 {
				t.Fatal("busy service restarted", got, starts, err)
			}
			observed, err := InspectSetup(context.Background(), home)
			testutil.Must(t, err)
			if observed.Installation != "staged-awaiting-qualification" || observed.RecentSuccessfulBackup || len(observed.PendingQualification) == 0 {
				t.Fatal("queued request invented qualification or success", observed)
			}
		})
	}
}

func TestManualDeclaredBackupRefusesChangedOrPendingAuthority(t *testing.T) {
	for _, damage := range []string{"desired", "helper", "pending-setup", "pending-activation", "unit-receipt", "unit-file", "disabled", "maintenance", "manager-owner"} {
		t.Run(damage, func(t *testing.T) {
			home, state, j := manualRunFixture(t)
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			desired, helper := j.Declaration.DesiredHash, j.Declaration.HelperSHA256
			switch damage {
			case "desired":
				desired = strings.Repeat("e", 64)
			case "helper":
				helper = strings.Repeat("e", 64)
			case "pending-setup":
				j.Updating = true
			case "pending-activation":
				testutil.Must(t, state.put("declared_activation", declaredActivation{Version: 1}))
			case "unit-receipt":
				delete(j.OwnedFiles, ".config/containers/systemd/deploy-backup.container")
			case "unit-file":
				testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/containers/systemd/deploy-backup.container"), []byte("foreign"), 0600))
			case "disabled":
				testutil.Must(t, state.put("installation", "disabled"))
			case "maintenance":
				testutil.Must(t, state.put("mode", "maintenance"))
			}
			testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
			starts := 0
			_, err = requestDeclaredBackup(context.Background(), home, desired, helper, manualBackupDependencies{
				stopped: func(context.Context, string, backupActivityUnit) (bool, error) {
					if damage == "manager-owner" {
						return false, errors.New("foreign loaded unit")
					}
					return true, nil
				},
				start: func(context.Context) error { starts++; return nil },
			})
			if err == nil || starts != 0 {
				t.Fatal("manual run ignored authority", starts, err)
			}
		})
	}
}

func TestManualDeclaredBackupChecksNativeManagerOwner(t *testing.T) {
	home, _, j := manualRunFixture(t)
	bin := t.TempDir()
	manager := `#!/bin/sh
if [ "$2" = show ]; then
 printf 'LoadState=loaded\nActiveState=inactive\nUnitFileState=generated\nFragmentPath=%s\nSourcePath=%s\nDropInPaths=%s\nNames=deploy-backup.service\n' "$TEST_RUN_FRAGMENT" "$TEST_RUN_SOURCE" "$TEST_RUN_DROPINS"
 exit
fi
[ "$2" = start ] && [ "$3" = --no-block ] && [ "$4" = deploy-backup.service ] || exit 9
printf start >> "$TEST_RUN_CALLS"
`
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(manager), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TEST_RUN_FRAGMENT", filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "systemd/generator/deploy-backup.service"))
	t.Setenv("TEST_RUN_SOURCE", filepath.Join(home, ".config/containers/systemd/deploy-backup.container"))
	t.Setenv("TEST_RUN_DROPINS", "")
	t.Setenv("TEST_RUN_CALLS", filepath.Join(bin, "calls"))
	got, err := RequestDeclaredBackup(context.Background(), home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256)
	if err != nil || got.State != "start_requested" {
		t.Fatal(got, err)
	}
	t.Setenv("TEST_RUN_DROPINS", "/foreign/override.conf")
	if _, err = RequestDeclaredBackup(context.Background(), home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256); err == nil {
		t.Fatal("foreign unit started")
	}
	calls, err := os.ReadFile(filepath.Join(bin, "calls"))
	testutil.Must(t, err)
	if string(calls) != "start" {
		t.Fatal("unexpected manager mutation", string(calls))
	}
}
