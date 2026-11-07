//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func freshSharedReportingFixture(t *testing.T) (string, WorkerInstallation, string, string, backupCutoverDependencies, *cutoverFixtureSession) {
	t.Helper()
	epoch, err := safefs.NewID("n")
	testutil.Must(t, err)
	home, c, old, deps, session := backupCutoverFixtureUsing(t, "", func(c *WorkerInstallation, cfg *worker.Config) {
		c.Policy.Notifications = config.WorkerNotifications{Mode: "shared"}
		cfg.Producers[0].Producer.Epoch = epoch
	})
	session.p = session.config.Producers[0].Producer
	session.g, err = safefs.NewID("g")
	testutil.Must(t, err)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	d, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer d.Close()
	setupID, err := safefs.NewID("n")
	testutil.Must(t, err)
	desired := notify.Hash("desired-shared-policy")
	r := ProducerReservation{Version: 1, Producer: session.p, Generation: session.g}
	j := WorkerSetupJournal{Schema: 1, ID: setupID, Stage: "complete", WorkerID: c.WorkerID, ArchiveID: c.ArchiveID, RequestHash: notify.Hash("setup-request"), Producer: &r, Declaration: &SetupDeclaration{Version: 1, DesiredHash: desired, Home: home, UID: os.Getuid(), HelperSHA256: old.HelperHash, NotificationRoot: session.m.Root, NotificationRoute: old.Managed.Route}}
	raw, err := safefs.ReadFile(root, ".config/deploy/backup-worker/runtime.json", 8<<20)
	testutil.Must(t, err)
	j.OwnedFiles = map[string]string{".config/deploy/backup-worker/runtime.json": sharedSetupDigest(raw)}
	testutil.Must(t, d.WriteJSON("setup.json", j, true))
	testutil.Must(t, d.WriteJSON("producer-reservation.json", producerReservationIntent{Version: 1, SetupID: j.ID, RequestHash: j.RequestHash, WorkerID: j.WorkerID, ArchiveID: j.ArchiveID, Reservation: r}, false))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	control.SharedRequired = true
	testutil.Must(t, state.put("control", control))
	deps.Completed = func(ctx context.Context, _ string, _ WorkerInstallation, _ backupCutoverSpec, owner backupNotificationOwner) error {
		return deps.Verify(ctx, owner)
	}
	return home, c, desired, old.Managed.ManifestHash, deps, session
}

func TestFreshSharedReportingResumesBoundaries(t *testing.T) {
	for _, point := range []string{"cutover-intent", "cutover-worker-leased", "cutover-fenced", "cutover-staged", "cutover-units", "cutover-active", "cutover-shared", "cutover-verified"} {
		t.Run(point, func(t *testing.T) {
			home, c, desired, manifest, deps, session := freshSharedReportingFixture(t)
			stop := errors.New("interrupted")
			deps.Fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			ctx := context.Background()
			if err := connectSharedReporting(ctx, home, desired, manifest, deps); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			if requireLegacyNotificationOwner(state) == nil || requireBackupReporting(state, c) == nil {
				t.Fatal("lost direct sender fence")
			}
			state.Close()
			deps.Fault = nil
			testutil.Must(t, connectSharedReporting(ctx, home, desired, manifest, deps))
			status, err := session.Status()
			testutil.Must(t, err)
			if !status.Active {
				t.Fatal(status)
			}
			_, spec, err := prepareFreshSharedReporting(ctx, home, desired, manifest)
			testutil.Must(t, err)
			if spec.Version != 5 || spec.Fresh == nil {
				t.Fatal("fabricated historical migration")
			}
			if err = (backupLegacyRollbackSpec{Cutover: spec}).validate(c); err == nil {
				t.Fatal("fresh shared setup restored legacy sender")
			}
			testutil.Must(t, runBackupCutover(ctx, home, c, spec, true, deps))
			if err = connectSharedReporting(ctx, home, desired, manifest, deps); err == nil {
				t.Fatal("reactivated revoked generation")
			}
		})
	}
}

func TestFreshSharedReportingRejectsContradictionsBeforeActivation(t *testing.T) {
	for _, change := range []string{"desired", "reservation", "fence", "attempts", "accepted", "gateway", "helper"} {
		t.Run(change, func(t *testing.T) {
			home, c, desired, manifest, deps, session := freshSharedReportingFixture(t)
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			switch change {
			case "desired":
				desired = notify.Hash("different")
			case "reservation":
				testutil.Must(t, os.Remove(filepath.Join(home, ".local/state/deploy/backup-worker/producer-reservation.json")))
			case "gateway", "helper":
				_, spec, e := prepareFreshSharedReporting(context.Background(), home, desired, manifest)
				testutil.Must(t, e)
				if change == "gateway" {
					spec.Managed.Root = t.TempDir()
				} else {
					spec.HelperHash = strings.Repeat("b", 64)
				}
				if e = runBackupCutover(context.Background(), home, c, spec, false, deps); e == nil {
					t.Fatal("changed pinned input accepted")
				}
				return
			default:
				state, e := openBackupState(backupCutoverPaths(home, c).State, false)
				testutil.Must(t, e)
				if change == "fence" {
					var control ArchiveControl
					testutil.Must(t, state.get("control", &control))
					control.SharedRequired = false
					testutil.Must(t, state.put("control", control))
				} else {
					err = backupTransferTx(context.Background(), state, func(tx *sql.Tx) error {
						query := "UPDATE outbox SET attempts=1"
						if change == "accepted" {
							query = "UPDATE outbox SET accepted='2026-11-07T00:00:00Z'"
						}
						_, e := tx.Exec(query)
						return e
					})
					testutil.Must(t, err)
				}
				state.Close()
			}
			if err = connectSharedReporting(context.Background(), home, desired, manifest, deps); err == nil {
				t.Fatal("contradiction accepted")
			}
			status, err := session.Status()
			testutil.Must(t, err)
			if status.Active {
				t.Fatal("activated contradictory installation")
			}
			if _, err = os.Stat(filepath.Join(home, ".local/state/deploy/backup-worker/state", backupCutoverFile)); !os.IsNotExist(err) {
				t.Fatal("published invalid cutover", err)
			}
		})
	}
}

func TestFreshSharedReportingRequiresAbsentDirectServices(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{".config/systemd/user", ".config/containers/systemd"} {
		testutil.Must(t, os.MkdirAll(filepath.Join(home, dir), 0700))
	}
	changed, container := "", false
	u := &backupCutoverManager{home: home, freshShared: true, run: func(_ context.Context, program string, args ...string) ([]byte, error) {
		if program == "podman" {
			if container {
				return []byte("surviving-sender"), nil
			}
			return nil, nil
		}
		if program != "systemctl" || len(args) < 4 || args[2] != "show" {
			t.Fatal("absence inspection mutated a service", program, args)
		}
		values := map[string]string{"LoadState": "not-found", "ActiveState": "inactive", "FragmentPath": "", "SourcePath": "", "DropInPaths": "", "Names": "", "MainPID": "0", "ControlPID": "0"}
		if strings.HasSuffix(args[3], ".timer") || strings.HasSuffix(args[3], ".path") {
			delete(values, "MainPID")
			delete(values, "ControlPID")
		}
		if changed != "" {
			values[changed] = "foreign"
		}
		var out strings.Builder
		for k, v := range values {
			fmt.Fprintf(&out, "%s=%s\n", k, v)
		}
		return []byte(out.String()), errors.New("systemctl not-found status")
	}}
	testutil.Must(t, u.StopLegacy(context.Background()))
	for _, field := range []string{"LoadState", "ActiveState", "FragmentPath", "SourcePath", "DropInPaths", "Names", "MainPID", "ControlPID", "extra"} {
		changed = field
		if err := u.StopLegacy(context.Background()); err == nil {
			t.Fatal("accepted unproven absence", field)
		}
	}
	changed, container = "", true
	if err := u.StopLegacy(context.Background()); err == nil {
		t.Fatal("accepted surviving container")
	}
	container = false
	testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/containers/systemd/deploy-backup-notifier.container"), []byte("foreign"), 0600))
	if err := u.StopLegacy(context.Background()); err == nil {
		t.Fatal("adopted foreign file")
	}
}

func TestFreshSharedReportingCompletedRetryDoesNotRestart(t *testing.T) {
	home, _, desired, manifest, deps, _ := freshSharedReportingFixture(t)
	ctx := context.Background()
	testutil.Must(t, connectSharedReporting(ctx, home, desired, manifest, deps))
	services := deps.Services.(*cutoverFixtureServices)
	services.started = false
	deps.Managed = nil // A completed retry must not enter the gateway lifecycle.
	testutil.Must(t, connectSharedReporting(ctx, home, desired, manifest, deps))
	if services.started {
		t.Fatal("completed retry restarted reporting")
	}
	if err := connectSharedReporting(ctx, home, desired, notify.Hash("changed admission"), deps); err == nil {
		t.Fatal("adopted changed admission")
	}
}

func TestFreshSharedReportingSurvivesSIGKILL(t *testing.T) {
	for _, point := range []string{"cutover-intent", "cutover-fenced", "cutover-active", "cutover-shared"} {
		t.Run(point, func(t *testing.T) {
			home, c, desired, manifest, deps, session := freshSharedReportingFixture(t)
			_, spec, err := prepareFreshSharedReporting(context.Background(), home, desired, manifest)
			testutil.Must(t, err)
			path := filepath.Join(t.TempDir(), "input.json")
			testutil.Must(t, safefs.WritePrivateJSON(path, cutoverCrashInput{home, c, spec, session.m, session.config, point}))
			testutil.Must(t, session.store.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupCutoverKilledHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "DEPLOY_CUTOVER_CRASH_INPUT="+path)
			output, err := cmd.CombinedOutput()
			var status *exec.ExitError
			if !errors.As(err, &status) || !status.ProcessState.Sys().(syscall.WaitStatus).Signaled() || status.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("did not reach kill boundary: %v %s", err, output)
			}
			session.store, err = worker.Open(session.m.Root, session.config, false)
			testutil.Must(t, err)
			defer session.store.Close()
			testutil.Must(t, connectSharedReporting(context.Background(), home, desired, manifest, deps))
			active, err := session.Status()
			testutil.Must(t, err)
			if !active.Active {
				t.Fatal(active)
			}
		})
	}
}
