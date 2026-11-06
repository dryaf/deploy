//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/ui"
)

func TestBackupLegacyRollbackResumesAndRevokesWithoutDualSending(t *testing.T) {
	for _, point := range []string{"legacy-rollback-intent", "legacy-rollback-canonical", "legacy-rollback-published"} {
		t.Run(point, func(t *testing.T) {
			home, c, cutover, deps, session := backupCutoverFixture(t)
			ctx := context.Background()
			testutil.Must(t, runBackupCutover(ctx, home, c, cutover, false, deps))
			testutil.Must(t, runBackupCutover(ctx, home, c, cutover, true, deps))
			spec := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-restored", Reason: "reviewed return to native legacy delivery"}
			stop := errors.New("interruption")
			deps.Fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if err := runBackupLegacyRollback(ctx, home, c, spec, false, deps); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			deps.Fault = nil
			testutil.Must(t, runBackupLegacyRollback(ctx, home, c, spec, false, deps))
			if !deps.Services.(*cutoverFixtureServices).legacyStarted {
				t.Fatal("native legacy schedule not started")
			}
			shared, err := session.Status()
			testutil.Must(t, err)
			if shared.Active || shared.State != "revoked" {
				t.Fatal("shared authority revived", shared)
			}
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			calls := 0
			testutil.Must(t, drainNotificationOutbox(ctx, state, c, func(_ context.Context, _ string, _ config.WorkerNotifications, event notification.NotificationEvent) (notification.DeliveryAttempt, error) {
				calls++
				if err := session.store.RevokeLegacy(session.p, spec.Generation); err == nil {
					t.Fatal("canonical generation revoked during send")
				}
				return notification.DeliveryAttempt{Accepted: true}, nil
			}, time.Now().UTC().Add(time.Minute)))
			if calls != 1 {
				t.Fatal("legacy message not delivered once", calls)
			}
			j := legacyAttemptRead(t, state)
			if j.review.Accepted != 1 {
				t.Fatal(j.review)
			}
			var raw string
			testutil.Must(t, state.db.QueryRow("SELECT body FROM backup_legacy_delivery_journal WHERE sequence=1").Scan(&raw))
			var record legacyDeliveryRecord
			testutil.Must(t, notify.Decode([]byte(raw), &record, notify.MaxRequest))
			if record.Intent.Generation != spec.Generation {
				t.Fatal("attempt lacks canonical generation")
			}
			if err = requireLegacyNotificationOwner(state); err == nil {
				t.Fatal("old installation workflow bypassed fence")
			}
			state.Close()
			testutil.Must(t, runBackupLegacyRollback(ctx, home, c, spec, false, deps))
			testutil.Must(t, runBackupLegacyRollback(ctx, home, c, spec, true, deps))
			testutil.Must(t, runBackupLegacyRollback(ctx, home, c, spec, true, deps))
			if err = runBackupLegacyRollback(ctx, home, c, spec, false, deps); err == nil {
				t.Fatal("reused revoked legacy generation")
			}
		})
	}
}

func TestBackupLegacyRollbackDryRun(t *testing.T) {
	previous := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = previous }()
	for _, mode := range []string{"--review", "--apply", "--revoke"} {
		testutil.Must(t, runBackupLegacyRollbackCLI([]string{"--local", mode, "--spec", "/absent/spec.json"}, &bytes.Buffer{}))
	}
	for _, args := range [][]string{{"--apply", "--spec", "/spec"}, {"--local", "--apply", "--revoke", "--spec", "/spec"}, {"--local", "--apply", "--spec", "relative"}} {
		if err := runBackupLegacyRollbackCLI(args, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid rollback scope accepted", args)
		}
	}
}

func TestBackupLegacyRollbackRetainsAcceptedSharedOutcome(t *testing.T) {
	home, c, cutover, deps, session := backupCutoverFixture(t)
	ctx := context.Background()
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, false, deps))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	var owner backupNotificationOwner
	testutil.Must(t, state.get("notification_owner", &owner))
	f := &pipelineFixture{state: state, binding: owner.Binding, now: time.Now().UTC()}
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	m := messages[0]
	_, err = session.store.Enqueue(ctx, m)
	testutil.Must(t, err)
	job, ok, err := session.store.Grant(ctx, m.Producer, m.ID)
	testutil.Must(t, err)
	if !ok {
		t.Fatal(job)
	}
	testutil.Must(t, session.store.Finish(ctx, job, worker.Result{Accepted: true}))
	state.Close()
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, true, deps))
	spec := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-after-acceptance", Reason: "retain shared acceptance"}
	testutil.Must(t, runBackupLegacyRollback(ctx, home, c, spec, false, deps))
	state, err = openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	called := false
	testutil.Must(t, drainNotificationOutbox(ctx, state, c, func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		called = true
		return notification.DeliveryAttempt{Accepted: true}, nil
	}, time.Now().UTC().Add(time.Minute)))
	if called {
		t.Fatal("resent shared acceptance through restored legacy sender")
	}
}

type legacyRollbackCrashInput struct {
	Config   WorkerInstallation
	Spec     backupLegacyRollbackSpec
	Manifest install.ManagedManifest
	Runtime  worker.Config
}

func TestBackupLegacyRollbackCrashChild(t *testing.T) {
	home := os.Getenv("DEPLOY_LEGACY_ROLLBACK_HOME")
	if home == "" {
		t.Skip("subprocess fixture")
	}
	d, err := safefs.Open(home)
	testutil.Must(t, err)
	defer d.Close()
	var input legacyRollbackCrashInput
	testutil.Must(t, d.ReadJSON("legacy-crash-input.json", &input))
	gateway, err := worker.Open(input.Manifest.Paths.State, input.Runtime, false)
	testutil.Must(t, err)
	defer gateway.Close()
	session := &cutoverFixtureSession{m: input.Manifest, store: gateway, p: input.Spec.Cutover.Managed.Producer, g: input.Spec.Cutover.Managed.Generation, legacy: input.Spec.Generation}
	deps := backupCutoverDependencies{Services: &cutoverFixtureServices{}, Managed: func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		return fn(session)
	}, Fault: func(at string) error {
		if at == os.Getenv("DEPLOY_LEGACY_ROLLBACK_BOUNDARY") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	err = runBackupLegacyRollback(context.Background(), home, input.Config, input.Spec, false, deps)
	t.Fatal("crash boundary not reached", err)
}

func TestBackupLegacyRollbackSIGKILL(t *testing.T) {
	for _, point := range []string{"legacy-rollback-intent", "legacy-rollback-canonical", "legacy-rollback-published"} {
		t.Run(point, func(t *testing.T) {
			home, c, cutover, deps, session := backupCutoverFixture(t)
			testutil.Must(t, runBackupCutover(context.Background(), home, c, cutover, false, deps))
			testutil.Must(t, runBackupCutover(context.Background(), home, c, cutover, true, deps))
			spec := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-crash", Reason: "reviewed rollback"}
			testutil.Must(t, safefs.WritePrivateJSON(filepath.Join(home, "legacy-crash-input.json"), legacyRollbackCrashInput{c, spec, session.m, session.config}))
			testutil.Must(t, session.store.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupLegacyRollbackCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_LEGACY_ROLLBACK_HOME="+home, "DEPLOY_LEGACY_ROLLBACK_BOUNDARY="+point)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("child did not die at boundary", err)
			}
			session.store, err = worker.Open(session.m.Paths.State, session.config, false)
			testutil.Must(t, err)
			defer session.store.Close()
			testutil.Must(t, runBackupLegacyRollback(context.Background(), home, c, spec, false, deps))
			testutil.Must(t, runBackupLegacyRollback(context.Background(), home, c, spec, true, deps))
		})
	}
}

func TestBackupLegacyRollbackRefusesAttemptedSharedWork(t *testing.T) {
	home, c, cutover, deps, session := backupCutoverFixture(t)
	ctx := context.Background()
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, false, deps))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	var owner backupNotificationOwner
	testutil.Must(t, state.get("notification_owner", &owner))
	f := &pipelineFixture{state: state, binding: owner.Binding, now: time.Now().UTC()}
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	m := messages[0]
	_, err = session.store.Enqueue(ctx, m)
	testutil.Must(t, err)
	job, ok, err := session.store.Grant(ctx, m.Producer, m.ID)
	testutil.Must(t, err)
	if !ok {
		t.Fatal(job)
	}
	testutil.Must(t, session.store.Finish(ctx, job, worker.Result{Ambiguous: true}))
	state.Close()
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, true, deps))
	spec := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-restored", Reason: "cannot override ambiguous shared work"}
	if err = runBackupLegacyRollback(ctx, home, c, spec, false, deps); err == nil {
		t.Fatal("restored over ambiguous shared attempts")
	}
	if deps.Services.(*cutoverFixtureServices).legacyStarted {
		t.Fatal("started blocked rollback")
	}
	state, err = openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	_, err = state.db.Exec("UPDATE notification_exports SET accepted=1,eligible=1")
	testutil.Must(t, err)
	state.Close()
	if err = runBackupLegacyRollback(ctx, home, c, spec, false, deps); err == nil {
		t.Fatal("local acceptance flags bypassed canonical evidence")
	}
}

func TestBackupLegacyRollbackRefusesUnresolvedAndMissingAuthority(t *testing.T) {
	home, c, cutover, deps, _ := backupCutoverFixture(t)
	for id, d := range cutover.Decisions {
		d.Disposition = "held"
		cutover.Decisions[id] = d
	}
	ctx := context.Background()
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, false, deps))
	spec := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-restored", Reason: "reviewed rollback"}
	if err := runBackupLegacyRollback(ctx, home, c, spec, false, deps); err == nil {
		t.Fatal("rollback without revocation")
	}
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, true, deps))
	if err := runBackupLegacyRollback(ctx, home, c, spec, false, deps); err == nil {
		t.Fatal("rollback silently released held legacy row")
	}
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	var r backupLegacyReview
	ledgerTx(t, state, func(tx *sql.Tx) error {
		var err error
		r, err = readBackupLegacyReview(tx, c, "legacy-one", time.Now().UTC())
		return err
	})
	decision := backupLegacyResolution{Version: 1, Review: r, ReviewHash: notify.Hash(r), Action: "unsent", Reason: "independent unsent evidence"}
	testutil.Must(t, resolveBackupLegacy(ctx, state, c, decision, r.ReviewedAt, nil))
	state.Close()
	testutil.Must(t, runBackupLegacyRollback(ctx, home, c, spec, false, deps))
	state, err = openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	testutil.Must(t, requireLegacyDeliveryOwner(state))
	testutil.Must(t, os.Remove(filepath.Join(state.directory, legacyRollbackFile)))
	if err = requireLegacyDeliveryOwner(state); err == nil {
		t.Fatal("restored sender ignored lost intent")
	}
}

func legacyCredentialPin(t *testing.T, c WorkerInstallation) string {
	t.Helper()
	hash, err := notification.TelegramCredentialHash(c.Policy.Notifications)
	testutil.Must(t, err)
	return hash
}

func TestBackupLegacyRollbackRefusesHiddenCanonicalStateBeforeIntent(t *testing.T) {
	home, c, cutover, deps, session := backupCutoverFixture(t)
	ctx := context.Background()
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, false, deps))
	testutil.Must(t, runBackupCutover(ctx, home, c, cutover, true, deps))
	paths := backupCutoverPaths(home, c)
	session.m.Paths.State = filepath.Join(paths.Keys, "gateway")
	spec := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-hidden", Reason: "reviewed rollback"}
	if err := runBackupLegacyRollback(ctx, home, c, spec, false, deps); err == nil {
		t.Fatal("restored an owner whose canonical state is hidden by the service sandbox")
	}
	if _, err := os.Stat(filepath.Join(paths.State, legacyRollbackFile)); !os.IsNotExist(err) {
		t.Fatal("inaccessible canonical state reached durable intent", err)
	}
	status, err := session.store.OwnershipStatus(session.p, spec.Generation)
	testutil.Must(t, err)
	if status.State != "unknown" || status.Active || deps.Services.(*cutoverFixtureServices).legacyStarted {
		t.Fatal("inaccessible canonical state gained delivery authority", status)
	}
}

func TestBackupLegacyRollbackCredentialsStayBoundAndRevokeWithoutSecrets(t *testing.T) {
	for _, phase := range []string{"before", "canonical", "restored", "runtime"} {
		t.Run(phase, func(t *testing.T) {
			home, c, cutover, deps, _ := backupCutoverFixture(t)
			canonical := c
			ctx := context.Background()
			testutil.Must(t, runBackupCutover(ctx, home, c, cutover, false, deps))
			testutil.Must(t, runBackupCutover(ctx, home, c, cutover, true, deps))
			spec := backupLegacyRollbackSpec{Version: 1, Cutover: cutover, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-pinned", Reason: "reviewed destination"}
			change := func() {
				testutil.Must(t, os.WriteFile(c.Policy.Notifications.Telegram.ChatIDFile, []byte("different-chat"), 0600))
			}
			if phase == "before" {
				change()
			}
			if phase == "canonical" {
				deps.Fault = func(at string) error {
					if at == "legacy-rollback-canonical" {
						change()
					}
					return nil
				}
			}
			err := runBackupLegacyRollback(ctx, home, c, spec, false, deps)
			if phase == "before" || phase == "canonical" {
				if err == nil {
					t.Fatal("changed credentials activated")
				}
				if deps.Services.(*cutoverFixtureServices).legacyStarted {
					t.Fatal("sender started with changed credentials")
				}
			} else {
				testutil.Must(t, err)
				if phase == "restored" {
					change()
				} else {
					p := *c.Policy.Notifications.Telegram
					p.ChatIDFile = filepath.Join(home, "other-chat")
					testutil.Must(t, os.WriteFile(p.ChatIDFile, []byte("other-chat"), 0600))
					c.Policy.Notifications.Telegram = &p
				}
				state, err := openBackupState(backupCutoverPaths(home, c).State, false)
				testutil.Must(t, err)
				called := false
				err = drainNotificationOutbox(ctx, state, c, func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
					called = true
					return notification.DeliveryAttempt{Accepted: true}, nil
				}, time.Now().UTC().Add(time.Minute))
				if err == nil || called {
					t.Fatal("changed restored credentials reached sender", err)
				}
				state.Close()
			}
			if phase != "before" {
				// Revocation does not require readable credentials, even after an
				// interruption between canonical activation and local publication.
				testutil.Must(t, os.Remove(canonical.Policy.Notifications.Telegram.TokenFile))
				deps.Fault = nil
				testutil.Must(t, runBackupLegacyRollback(ctx, home, canonical, spec, true, deps))
			}
		})
	}
}
