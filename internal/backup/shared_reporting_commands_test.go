//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestFreshReportingCommandContinuationKeepsOriginalCutover(t *testing.T) {
	home, c, desired, original, deps, session := freshSharedReportingFixture(t)
	ctx := context.Background()
	testutil.Must(t, connectSharedReporting(ctx, home, desired, original, deps))
	path := filepath.Join(backupCutoverPaths(home, c).State, backupCutoverFile)
	before, err := os.ReadFile(path)
	testutil.Must(t, err)
	current := session.m
	current.Mode = "delivery"
	currentHash := notify.Hash(current)
	calls := 0
	deps.CommandContinuation = func(_ context.Context, root, now, old string) (install.ManagedManifest, error) {
		calls++
		if root != session.m.Root || now != currentHash || old != original {
			return install.ManagedManifest{}, errors.New("wrong command continuation pins")
		}
		return current, nil
	}
	deps.Managed = nil
	services := deps.Services.(*cutoverFixtureServices)
	services.started = false
	for range 2 {
		testutil.Must(t, connectSharedReporting(ctx, home, desired, currentHash, deps))
	}
	if services.started || calls != 2 {
		t.Fatal("continued reporting replayed service or lost proof", calls)
	}
	after, err := os.ReadFile(path)
	testutil.Must(t, err)
	if string(before) != string(after) {
		t.Fatal("command admission rewrote original cutover")
	}
	_, spec, err := prepareFreshSharedReportingUsing(ctx, home, desired, currentHash, deps.CommandContinuation)
	testutil.Must(t, err)
	if spec.Managed.ManifestHash != original || spec.Version != 5 {
		t.Fatal("continuation replaced original admission")
	}
}

func TestFreshReportingCommandContinuationRefusesIncompleteOrChangedState(t *testing.T) {
	for _, damage := range []string{"completion", "owner", "disabled", "policy", "proof", "identity", "lost-after-review"} {
		t.Run(damage, func(t *testing.T) {
			home, c, desired, original, deps, session := freshSharedReportingFixture(t)
			ctx := context.Background()
			testutil.Must(t, connectSharedReporting(ctx, home, desired, original, deps))
			current := session.m
			current.Mode = "delivery"
			currentHash := notify.Hash(current)
			deps.CommandContinuation = func(context.Context, string, string, string) (install.ManagedManifest, error) {
				if damage == "proof" {
					return install.ManagedManifest{}, errors.New("missing command history")
				}
				if damage == "identity" {
					current.Identity = notify.Identity{}
					return current, nil
				}
				if damage == "lost-after-review" {
					state, e := openBackupState(backupCutoverPaths(home, c).State, false)
					testutil.Must(t, e)
					_, e = state.db.Exec("DELETE FROM meta WHERE key='notification_cutover_complete'")
					testutil.Must(t, e)
					testutil.Must(t, state.Close())
				}
				return current, nil
			}
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			switch damage {
			case "completion":
				_, err = state.db.Exec("DELETE FROM meta WHERE key='notification_cutover_complete'")
			case "owner":
				err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
					owner, e := readBackupNotificationOwner(tx)
					if e != nil {
						return e
					}
					owner.Mode = "fenced"
					return storeBackupEvaluationMeta(tx, "notification_owner", owner)
				})
			case "disabled":
				var control ArchiveControl
				testutil.Must(t, state.get("control", &control))
				control.ReportingDisabled = true
				err = state.put("control", control)
			case "policy":
				root, e := safefs.Open(home)
				testutil.Must(t, e)
				var j WorkerSetupJournal
				testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
				desired = notify.Hash("changed backup policy")
				j.Declaration.DesiredHash = desired
				testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
				root.Close()
			}
			testutil.Must(t, err)
			testutil.Must(t, state.Close())
			deps.Managed = nil
			services := deps.Services.(*cutoverFixtureServices)
			services.started = false
			if err = connectSharedReporting(ctx, home, desired, currentHash, deps); err == nil {
				t.Fatal("accepted invalid command continuation")
			}
			if services.started {
				t.Fatal("invalid continuation restarted reports")
			}
		})
	}
}
