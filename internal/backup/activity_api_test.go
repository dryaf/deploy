package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func backupActivityFixture(t *testing.T) (string, *backupState, WorkerSetupJournal, WorkerInstallation, config.DesiredWorkers) {
	t.Helper()
	// This fixture models a separate target home with legacy default roots,
	// independent of the developer account's inherited XDG environment.
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME"} {
		t.Setenv(name, "")
	}
	home, s, j, c := declaredQualificationFixture(t)
	inv, err := config.ParseInventory("server.yaml", []byte("version: 3\nservers:\n  manager:\n    host: pi.invalid\n    user: backup\n    backup_worker:\n      sources: [source]\n  source: {host: source.invalid, user: app}\n"))
	testutil.Must(t, err)
	d := *inv.Servers["manager"].Desired
	j.Declaration.Desired = &d
	j.Declaration.DesiredHash, err = d.Fingerprint()
	testutil.Must(t, err)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	for _, path := range []string{".config/containers/systemd/deploy-backup.container", ".config/systemd/user/deploy-backup.timer", ".config/systemd/user/deploy-backup-health.timer", ".config/systemd/user/deploy-backup-health.service", ".config/systemd/user/deploy-backup-maintenance.timer", ".config/systemd/user/deploy-backup-maintenance.service"} {
		testutil.Must(t, root.Mkdir(filepath.Dir(path)))
		body := []byte("synthetic owned " + path)
		testutil.Must(t, os.WriteFile(filepath.Join(home, path), body, 0600))
		j.OwnedFiles[path] = fmtDigest(body)
	}
	testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
	return home, s, j, c, d
}

func TestBackupActivityResumesEveryBoundaryAndKeepsIdentity(t *testing.T) {
	for _, boundary := range []string{"intent", "fenced", "stopped", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			home, s, j, c, desired := backupActivityFixture(t)
			ctx := context.Background()
			stopped, stops := false, 0
			faulted := false
			deps := backupActivityDependencies{inspect: func(context.Context, string, []backupActivityUnit) (bool, error) { return stopped, nil }, stop: func(_ context.Context, _ string, units []backupActivityUnit) error {
				var mode string
				testutil.Must(t, s.get("mode", &mode))
				if mode != "maintenance" {
					t.Fatal("stop before execution fence")
				}
				if len(units) != 6 || !units[0].Activation {
					t.Fatal("activation units not stopped first")
				}
				stops++
				stopped = true
				return nil
			}, boundary: func(stage string) error {
				if !faulted && stage == boundary {
					faulted = true
					return errors.New("interrupted")
				}
				return nil
			}}
			for _, enabled := range []bool{false, true, false, true} {
				flag := enabled
				b := *desired.Backup
				b.Enabled = &flag
				desired.Backup = &b
				faulted = false
				_, err := reconcileDeclaredActivity(ctx, home, desired, j.Declaration.HelperSHA256, deps)
				if err == nil || !faulted {
					t.Fatal("boundary not reached", boundary, enabled, err)
				}
				observed, err := InspectSetup(ctx, home)
				testutil.Must(t, err)
				if boundary != "complete" && observed.Stage != "activity_pending" {
					t.Fatal("pending activity hidden", observed)
				}
				if boundary != "intent" && !enabled && requireBackupActivityReady(s) == nil {
					t.Fatal("disabled execution was not fenced")
				}
				got, err := reconcileDeclaredActivity(ctx, home, desired, j.Declaration.HelperSHA256, deps)
				testutil.Must(t, err)
				if !got.Handled || got.State != activityInstallation(!enabled) {
					t.Fatal(got)
				}
				before := stops
				got, err = reconcileDeclaredActivity(ctx, home, desired, j.Declaration.HelperSHA256, deps)
				testutil.Must(t, err)
				if !got.Unchanged || stops != before {
					t.Fatal("completed activity repeated stop")
				}
				observed, policy, err := InspectSetupPolicy(ctx, home)
				testutil.Must(t, err)
				hash, _ := desired.Fingerprint()
				if observed.DesiredHash != hash || !reflect.DeepEqual(policy, &desired) || observed.WorkerID != j.WorkerID || observed.ArchiveID != j.ArchiveID || observed.Installation != activityInstallation(!enabled) {
					t.Fatal("activity lost policy or identity", observed)
				}
				actual, err := loadWorkerInstallation(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"))
				testutil.Must(t, err)
				if notify.Hash(actual) != notify.Hash(c) {
					t.Fatal("activity rewrote runtime policy")
				}
				if enabled {
					testutil.Must(t, requireBackupActivityReady(s))
					stopped = false
				} else if requireBackupActivityReady(s) == nil {
					t.Fatal("disabled writer allowed")
				}
			}
			if stops != 2 {
				t.Fatal("enable unexpectedly started/stopped services", stops)
			}
		})
	}
}

func TestBackupActivityRejectsDriftBeforeIntent(t *testing.T) {
	for _, kind := range []string{"helper", "policy", "pending-activation", "mode", "unit", "snapshot", "foreign-unit"} {
		t.Run(kind, func(t *testing.T) {
			home, s, j, _, d := backupActivityFixture(t)
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			flag := false
			b := *d.Backup
			b.Enabled = &flag
			d.Backup = &b
			helper := j.Declaration.HelperSHA256
			deps := backupActivityDependencies{inspect: func(context.Context, string, []backupActivityUnit) (bool, error) { return false, nil }, stop: func(context.Context, string, []backupActivityUnit) error { t.Fatal("drift reached stop"); return nil }}
			switch kind {
			case "helper":
				helper = fmtDigest([]byte("changed"))
			case "policy":
				d.Backup.Schedule = "weekly"
			case "pending-activation":
				testutil.Must(t, s.put("declared_activation", declaredActivation{Version: 1}))
			case "mode":
				testutil.Must(t, s.put("mode", "maintenance"))
			case "unit":
				testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/systemd/user/deploy-backup.timer"), []byte("foreign"), 0600))
			case "snapshot":
				j.Declaration.Desired = nil
				testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
			case "foreign-unit":
				deps.inspect = func(context.Context, string, []backupActivityUnit) (bool, error) {
					return false, errors.New("foreign fragment")
				}
			}
			before, err := os.ReadFile(filepath.Join(home, backupSetupFile))
			testutil.Must(t, err)
			if _, err = reconcileDeclaredActivity(context.Background(), home, d, helper, deps); err == nil {
				t.Fatal("drift accepted")
			}
			after, err := os.ReadFile(filepath.Join(home, backupSetupFile))
			testutil.Must(t, err)
			if string(before) != string(after) {
				t.Fatal("rejected activity changed setup")
			}
		})
	}
}

func TestBackupActivityRetainsLostStopAndRefusesMissingCompletion(t *testing.T) {
	home, s, j, _, d := backupActivityFixture(t)
	flag := false
	b := *d.Backup
	b.Enabled = &flag
	d.Backup = &b
	stopped, stops := false, 0
	deps := backupActivityDependencies{inspect: func(context.Context, string, []backupActivityUnit) (bool, error) { return stopped, nil }, stop: func(context.Context, string, []backupActivityUnit) error {
		stops++
		stopped = true
		return errors.New("lost response")
	}}
	ctx := context.Background()
	if _, err := reconcileDeclaredActivity(ctx, home, d, j.Declaration.HelperSHA256, deps); err == nil {
		t.Fatal("lost reply hidden")
	}
	_, err := reconcileDeclaredActivity(ctx, home, d, j.Declaration.HelperSHA256, deps)
	testutil.Must(t, err)
	if stops != 1 {
		t.Fatal("lost stop repeated")
	}
	stopped = false
	if _, err = reconcileDeclaredActivity(ctx, home, d, j.Declaration.HelperSHA256, deps); err == nil || stops != 1 {
		t.Fatal("completed disable silently repaired drift")
	}
	_, err = s.db.Exec("DELETE FROM meta WHERE key='declared_activity'")
	testutil.Must(t, err)
	if _, err = InspectSetup(ctx, home); err == nil {
		t.Fatal("lost activity completion ignored")
	}
	if _, err = reconcileDeclaredActivity(ctx, home, d, j.Declaration.HelperSHA256, deps); err == nil {
		t.Fatal("lost activity receipt recreated")
	}
	var a declaredBackupActivity
	if err = s.get("declared_activity", &a); err != sql.ErrNoRows {
		t.Fatal("missing evidence rewritten", err)
	}
}

func TestBackupActivityProcessDeath(t *testing.T) {
	if home := os.Getenv("DEPLOY_TEST_ACTIVITY_HOME"); home != "" {
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var j WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
		d := *j.Declaration.Desired
		b := *d.Backup
		flag := false
		b.Enabled = &flag
		d.Backup = &b
		deps := durableActivityTestManager(t, home)
		deps.boundary = func(stage string) error {
			if stage == os.Getenv("DEPLOY_TEST_ACTIVITY_BOUNDARY") {
				return syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			return nil
		}
		_, err = reconcileDeclaredActivity(context.Background(), home, d, j.Declaration.HelperSHA256, deps)
		t.Fatal("child did not die", err)
	}
	for _, boundary := range []string{"intent", "fenced", "stopped", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			home, _, j, _, d := backupActivityFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupActivityProcessDeath$")
			cmd.Env = append(os.Environ(), "DEPLOY_TEST_ACTIVITY_HOME="+home, "DEPLOY_TEST_ACTIVITY_BOUNDARY="+boundary)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("wrong child exit", err, string(out))
			}
			flag := false
			b := *d.Backup
			b.Enabled = &flag
			d.Backup = &b
			deps := durableActivityTestManager(t, home)
			for range 2 {
				got, err := reconcileDeclaredActivity(context.Background(), home, d, j.Declaration.HelperSHA256, deps)
				testutil.Must(t, err)
				if got.State != "disabled" {
					t.Fatal(got)
				}
			}
			raw, err := os.ReadFile(filepath.Join(home, "activity-stops"))
			testutil.Must(t, err)
			if string(raw) != "stop" {
				t.Fatal("process death repeated stop", string(raw))
			}
		})
	}
}

func durableActivityTestManager(t *testing.T, home string) backupActivityDependencies {
	t.Helper()
	return backupActivityDependencies{inspect: func(context.Context, string, []backupActivityUnit) (bool, error) {
		_, err := os.Stat(filepath.Join(home, "activity-stopped"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}, stop: func(context.Context, string, []backupActivityUnit) error {
		f, err := os.OpenFile(filepath.Join(home, "activity-stops"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.WriteString("stop")
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(home, "activity-stopped"), []byte("stopped"), 0600)
	}}
}

func TestBackupActivityEnableArchivesOldActivationAndDoesNotActivate(t *testing.T) {
	home, s, j, _, d := backupActivityFixture(t)
	old := declaredActivation{Version: 1, Desired: j.Declaration.DesiredHash, Runtime: fmtDigest([]byte("runtime")), Proof: fmtDigest([]byte("proof")), Complete: true}
	testutil.Must(t, s.put("declared_activation", old))
	testutil.Must(t, s.put("installation", "active"))
	deps := durableActivityTestManager(t, home)
	for _, enabled := range []bool{false, true} {
		b := *d.Backup
		b.Enabled = &enabled
		d.Backup = &b
		_, err := reconcileDeclaredActivity(context.Background(), home, d, j.Declaration.HelperSHA256, deps)
		testutil.Must(t, err)
	}
	var current declaredActivation
	if err := s.get("declared_activation", &current); err != sql.ErrNoRows {
		t.Fatal("old activation reused", err)
	}
	var a declaredBackupActivity
	testutil.Must(t, s.get("declared_activity", &a))
	testutil.Must(t, s.get("declared_activation_history/"+a.Operation, &current))
	if notify.Hash(current) != notify.Hash(old) {
		t.Fatal("activation history changed")
	}
	observed, err := InspectSetup(context.Background(), home)
	testutil.Must(t, err)
	if observed.Installation != "staged-awaiting-qualification" {
		t.Fatal("enable activated timers", observed)
	}
}
