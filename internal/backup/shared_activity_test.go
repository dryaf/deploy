//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func sharedBackupActivityFixture(t *testing.T) (string, WorkerSetupJournal, config.DesiredWorkers) {
	t.Helper()
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME"} {
		t.Setenv(name, "")
	}
	home, _, j, _, _, _ := deliveryReportRouteFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	testutil.Must(t, root.Mkdir(".local/state/deploy/locks"))
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	testutil.Must(t, err)
	lock.Close()
	layout, e := declarationPaths(home, j.Declaration)
	testutil.Must(t, e)
	stateDir, e := safefs.Open(layout.State)
	testutil.Must(t, e)
	schema, e := stateDir.Lock("schema.lock", true, true)
	testutil.Must(t, e)
	schema.Close()
	stateDir.Close()
	for _, path := range []string{".config/containers/systemd/deploy-backup.container", ".config/systemd/user/deploy-backup.timer", ".config/systemd/user/deploy-backup-health.timer", ".config/systemd/user/deploy-backup-health.service", ".config/systemd/user/deploy-backup-maintenance.timer", ".config/systemd/user/deploy-backup-maintenance.service"} {
		if j.OwnedFiles[path] != "" {
			continue
		}
		testutil.Must(t, root.Mkdir(filepath.Dir(path)))
		body := []byte("owned activity fixture " + path)
		testutil.Must(t, os.WriteFile(filepath.Join(home, path), body, 0600))
		j.OwnedFiles[path] = fmtDigest(body)
	}
	testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
	return home, j, *j.Declaration.Desired
}
func TestSharedBackupActivityFencesBeforeGatewayAndResumesEveryBoundary(t *testing.T) {
	for _, boundary := range []string{"intent", "fenced", "stopped", "gateway", "committed", "complete"} {
		for _, field := range []string{"gateway", "backup", "reporting"} {
			t.Run(boundary+"/"+field, func(t *testing.T) {
				ctx := context.Background()
				home, j, desired := sharedBackupActivityFixture(t)
				root, err := safefs.Open(home)
				testutil.Must(t, err)
				defer root.Close()
				state, err := openInstalledBackupState(home, false)
				testutil.Must(t, err)
				defer state.Close()
				beforeOwner, err := currentSharedBackupOwner(state)
				testutil.Must(t, err)
				layout, err := declarationPaths(home, j.Declaration)
				testutil.Must(t, err)
				handoff, err := os.ReadFile(filepath.Join(layout.State, backupCutoverFile))
				testutil.Must(t, err)
				stopped := false
				for _, disabled := range []bool{true, false} {
					flag := !disabled
					n, b := *desired.Notification, *desired.Backup
					desired.Notification, desired.Backup = &n, &b
					switch field {
					case "gateway":
						n.Enabled = &flag
					case "backup":
						b.Enabled = &flag
					case "reporting":
						b.Notifications.Enabled = &flag
					}
					op, err := safefs.NewID("n")
					testutil.Must(t, err)
					request := SharedActivityRequest{op, j.Declaration.NotificationRoot, notify.Hash("gateway before activity")}
					reached := false
					lost := errors.New("lost shared activity reply")
					deps := backupActivityDependencies{setup: func(ctx context.Context, root *safefs.Directory, home string) (SetupObservation, error) {
						var current WorkerSetupJournal
						e := root.ReadJSON(backupSetupFile, &current)
						if e != nil {
							return SetupObservation{}, e
						}
						c, e := loadInstalledWorker(home)
						if e != nil {
							return SetupObservation{}, e
						}
						observed := SetupObservation{Stage: "complete", DesiredHash: current.Declaration.DesiredHash}
						e = state.get("installation", &observed.Installation)
						if e != nil {
							return observed, e
						}
						tx, e := state.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
						if e != nil {
							return observed, e
						}
						defer tx.Rollback()
						e = inspectBackupActivity(tx, current, c, &observed)
						return observed, e
					}, shared: &request, inspect: func(context.Context, string, []backupActivityUnit) (bool, error) { return stopped, nil }, stop: func(context.Context, string, []backupActivityUnit) error { stopped = true; return nil }, gateway: func() error {
						var mode string
						testutil.Must(t, state.get("mode", &mode))
						if mode != "maintenance" || !stopped || requireBackupActivityReady(state) == nil {
							t.Fatal("gateway called without dependency fence")
						}
						return nil
					}, boundary: func(at string) error {
						if at == boundary && !reached {
							reached = true
							return lost
						}
						return nil
					}}
					_, err = reconcileDeclaredActivity(ctx, home, desired, j.Declaration.HelperSHA256, deps)
					if !errors.Is(err, lost) || !reached {
						t.Fatal("missed shared boundary", boundary, disabled, err)
					}
					deps.boundary = nil
					for range 2 {
						v, e := reconcileDeclaredActivity(ctx, home, desired, j.Declaration.HelperSHA256, deps)
						testutil.Must(t, e)
						if !v.Handled {
							t.Fatal("shared operation escaped activity")
						}
					}
					observed, err := deps.setup(ctx, root, home)
					testutil.Must(t, err)
					if observed.Installation != activityInstallation(disabled) {
						t.Fatal("activity installation", observed)
					}
					if !disabled {
						if requireSharedActivityRun(state, WorkerRun{StartedAt: time.Unix(1, 0)}) == nil {
							t.Fatal("old backup run authorized reactivation")
						}
						testutil.Must(t, requireSharedActivityRun(state, WorkerRun{StartedAt: time.Now().UTC()}))
					}
					if disabled == (requireBackupActivityReady(state) == nil) {
						t.Fatal("wrong activity fence")
					}
					var after WorkerSetupJournal
					testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
					owner, e := currentSharedBackupOwner(state)
					testutil.Must(t, e)
					current, e := os.ReadFile(filepath.Join(layout.State, backupCutoverFile))
					testutil.Must(t, e)
					if !reflect.DeepEqual(owner, beforeOwner) || string(current) != string(handoff) || !reflect.DeepEqual(after.SourceKeys, j.SourceKeys) || after.RequestHash != j.RequestHash || *after.Producer != *j.Producer {
						t.Fatal("activity replaced original authority")
					}
				}
				_, err = state.db.Exec("DELETE FROM meta WHERE key='declared_activity'")
				testutil.Must(t, err)
				if requireBackupActivityReady(state) == nil {
					t.Fatal("missing shared execution fence fell back to enabled")
				}
			})
		}
	}
}
