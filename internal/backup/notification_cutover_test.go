//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/ui"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type cutoverFixtureServices struct {
	stopped, started bool
	legacyStarted    bool
	units            map[string]string
}

func (s *cutoverFixtureServices) PrepareLegacy(context.Context, WorkerInstallation, WorkerUnitPaths, string) error {
	s.legacyStarted = false
	return nil
}
func (s *cutoverFixtureServices) StartLegacy(context.Context) error {
	s.legacyStarted = true
	return nil
}

func (s *cutoverFixtureServices) Prepare(context.Context, WorkerInstallation, WorkerUnitPaths, string) error {
	return nil
}
func (s *cutoverFixtureServices) StopLegacy(context.Context) error { s.stopped = true; return nil }
func (s *cutoverFixtureServices) InstallReports(_ context.Context, u map[string]string) error {
	s.units = u
	return nil
}
func (s *cutoverFixtureServices) StartReports(_ context.Context, u map[string]string) error {
	s.started = true
	return nil
}
func (s *cutoverFixtureServices) StopReports(_ context.Context, u map[string]string) error {
	s.started = false
	return nil
}

type cutoverFixtureSession struct {
	m      install.ManagedManifest
	store  *worker.Store
	p      notify.Producer
	g      string
	config worker.Config
	legacy string
}

func (s *cutoverFixtureSession) Manifest() install.ManagedManifest { return s.m }
func (s *cutoverFixtureSession) CredentialPaths() []string {
	return []string{filepath.Join(s.m.Root, "telegram-token")}
}
func (s *cutoverFixtureSession) Status() (notify.OwnershipStatus, error) {
	return s.store.OwnershipStatus(s.p, s.g)
}
func (s *cutoverFixtureSession) Stage() error {
	v, err := s.Status()
	if err != nil {
		return err
	}
	if v.Active || v.State == "staged" {
		return nil
	}
	return s.store.Stage(s.p, s.g)
}
func (s *cutoverFixtureSession) Activate() error { return s.store.Activate(s.p, s.g) }
func (s *cutoverFixtureSession) Revoke(ctx context.Context) error {
	v, err := s.Status()
	if err != nil {
		return err
	}
	if v.State == "unknown" {
		if err = s.store.Stage(s.p, s.g); err != nil {
			return err
		}
	}
	return s.store.Revoke(ctx, s.p, s.g)
}
func (s *cutoverFixtureSession) Receipts(ctx context.Context, c notify.Cursor) (worker.ReceiptPage, error) {
	return s.store.Receipts(ctx, c, 128)
}
func (s *cutoverFixtureSession) RequireLegacyUnsent(ctx context.Context, m notify.Message) error {
	return s.store.RequireLegacyUnsent(ctx, m)
}
func (s *cutoverFixtureSession) RequireLegacyAccepted(ctx context.Context, m notify.Message) error {
	return s.store.RequireLegacyAccepted(ctx, m)
}
func (s *cutoverFixtureSession) RestoreLegacy() error {
	return s.store.RestoreLegacy(s.p, s.g, s.legacy)
}
func (s *cutoverFixtureSession) RevokeLegacy() error { return s.store.RevokeLegacy(s.p, s.legacy) }
func backupCutoverFixture(t *testing.T) (string, WorkerInstallation, backupCutoverSpec, backupCutoverDependencies, *cutoverFixtureSession) {
	return backupCutoverFixtureUsing(t, "", nil)
}
func backupCutoverFixtureUsing(t *testing.T, home string, change func(*WorkerInstallation, *worker.Config)) (string, WorkerInstallation, backupCutoverSpec, backupCutoverDependencies, *cutoverFixtureSession) {
	t.Helper()
	state, c, _ := legacyAttemptFixture(t)
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	c.Schema = 1
	c.WorkerID = control.WriterID
	c.ArchiveID = control.ArchiveID
	c.ConfigHash = control.ConfigHash
	c.Policy.Notifications.Signal = nil
	c.Policy.Notifications.Primary = "telegram"
	if home == "" {
		home = t.TempDir()
	}
	c.Policy.Notifications.Telegram = &config.WorkerTelegram{}
	c.Policy.Notifications.Telegram.TokenFile = filepath.Join(home, "telegram-token")
	c.Policy.Notifications.Telegram.ChatIDFile = filepath.Join(home, "telegram-chat")
	testutil.Must(t, os.WriteFile(c.Policy.Notifications.Telegram.TokenFile, []byte("123:fixture-token"), 0600))
	testutil.Must(t, os.WriteFile(c.Policy.Notifications.Telegram.ChatIDFile, []byte("456"), 0600))
	paths := backupCutoverPaths(home, c)
	testutil.Must(t, os.MkdirAll(filepath.Dir(paths.State), 0700))
	testutil.Must(t, os.MkdirAll(filepath.Join(home, ".local/state/deploy/locks"), 0700))
	testutil.Must(t, state.Close())
	testutil.Must(t, os.Rename(state.directory, paths.State))
	testutil.Must(t, os.MkdirAll(filepath.Dir(paths.Config), 0700))
	testutil.Must(t, safefs.WritePrivateJSON(paths.Config, c))
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	testutil.Must(t, err)
	lock.Close()
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", WorkerSetupJournal{Stage: "complete", WorkerID: c.WorkerID, ArchiveID: c.ArchiveID}, false))
	root.Close()
	state, err = openBackupState(paths.State, false)
	testutil.Must(t, err)
	defer state.Close()
	var evidence backupCutoverEvidence
	testutil.Must(t, backupTransferTx(context.Background(), state, func(tx *sql.Tx) error { var e error; evidence, e = readBackupCutoverEvidence(tx); return e }))
	p := notify.Producer{ID: c.WorkerID, Epoch: "one"}
	cfg := worker.Config{Producers: []worker.Enrollment{{Producer: p, Routes: []string{"ops"}, MaxMessages: 100, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400}}, Routes: map[string]worker.Route{"ops": {Revision: "one", Primary: "telegram"}}, MaxMessages: 100, MaxPayloadBytes: 1 << 20, MaxDatabaseBytes: 32 << 20}
	if change != nil {
		change(&c, &cfg)
		testutil.Must(t, safefs.WritePrivateJSON(paths.Config, c))
	}
	gatewayPath := t.TempDir()
	testutil.Must(t, os.Chmod(gatewayPath, 0700))
	gateway, err := worker.Open(gatewayPath, cfg, true)
	testutil.Must(t, err)
	t.Cleanup(func() { gateway.Close() })
	session := &cutoverFixtureSession{m: install.ManagedManifest{Root: gatewayPath, Identity: gateway.Identity(), UnitDirectory: filepath.Join(home, ".config/systemd/user"), Paths: install.ServicePaths{State: gatewayPath, Socket: filepath.Join(home, "api.sock")}}, store: gateway, p: p, g: "cutover-one", config: cfg}
	spec := backupCutoverSpec{Version: 1, Managed: install.ManagedProducerRequest{Root: gatewayPath, ManifestHash: notify.Hash("manifest"), Producer: p, Generation: session.g, Route: "ops"}, HelperHash: strings.Repeat("a", 64), ConfigHash: notify.Hash(c), EvidenceHash: notify.Hash(evidence), Decisions: map[string]backupCutoverDecision{}}
	for _, row := range evidence.Rows {
		spec.Decisions[row.ID] = backupCutoverDecision{notify.Hash(row), "unsent", "fixture independent old-history evidence"}
	}
	services := &cutoverFixtureServices{}
	deps := backupCutoverDependencies{Services: services, Managed: func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		session.legacy = r.LegacyGeneration
		if !services.stopped {
			return errors.New("legacy sender not stopped")
		}
		return fn(session)
	}, Verify: func(context.Context, backupNotificationOwner) error {
		v, e := session.Status()
		if e == nil && !v.Active {
			e = errors.New("not active")
		}
		return e
	}}
	return home, c, spec, deps, session
}
func TestBackupCutoverResumesEveryBoundary(t *testing.T) {
	for _, point := range []string{"cutover-intent", "cutover-worker-leased", "cutover-fenced", "cutover-staged", "cutover-units", "cutover-active", "cutover-shared", "cutover-verified"} {
		t.Run(point, func(t *testing.T) {
			home, c, spec, deps, session := backupCutoverFixture(t)
			ctx := context.Background()
			stop := errors.New("interrupted")
			deps.Fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if err := runBackupCutover(ctx, home, c, spec, false, deps); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			if err = requireLegacyNotificationOwner(state); err == nil {
				t.Fatal("legacy fence lost")
			}
			state.Close()
			deps.Fault = nil
			testutil.Must(t, runBackupCutover(ctx, home, c, spec, false, deps))
			status, err := session.Status()
			testutil.Must(t, err)
			if !status.Active {
				t.Fatal(status)
			}
			testutil.Must(t, runBackupCutover(ctx, home, c, spec, true, deps))
			status, err = session.Status()
			testutil.Must(t, err)
			if status.State != "revoked" {
				t.Fatal(status)
			}
			if err = runBackupCutover(ctx, home, c, spec, false, deps); err == nil {
				t.Fatal("reactivated revoked generation")
			}
		})
	}
}
func TestBackupCutoverRejectsChangedEvidenceAndPreservesHeldRows(t *testing.T) {
	home, c, spec, deps, _ := backupCutoverFixture(t)
	ctx := context.Background()
	bad := spec
	bad.EvidenceHash = notify.Hash("changed")
	if err := runBackupCutover(ctx, home, c, bad, false, deps); err == nil {
		t.Fatal("accepted changed evidence")
	}
	for id, d := range spec.Decisions {
		d.Disposition = "held"
		spec.Decisions[id] = d
	}
	testutil.Must(t, runBackupCutover(ctx, home, c, spec, false, deps))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	var before, after string
	testutil.Must(t, state.db.QueryRow("SELECT body FROM backup_notification_legacy WHERE source='legacy-one'").Scan(&before))
	testutil.Must(t, state.db.QueryRow("SELECT record FROM outbox WHERE id='legacy-one'").Scan(&after))
	if !strings.Contains(before, "original envelope") || !strings.Contains(after, "original envelope") {
		t.Fatal("evidence changed")
	}
	var owner backupNotificationOwner
	testutil.Must(t, state.get("notification_owner", &owner))
	testutil.Must(t, requireBackupCutoverOwner(state, owner))
	testutil.Must(t, os.Remove(filepath.Join(state.directory, backupCutoverFile)))
	if err = requireBackupCutoverOwner(state, owner); err == nil {
		t.Fatal("report accepted missing intent")
	}
	if err = requireLegacyNotificationOwner(state); err == nil {
		t.Fatal("SQL fence lost")
	}
}

type cutoverCrashInput struct {
	Home     string
	Config   WorkerInstallation
	Spec     backupCutoverSpec
	Manifest install.ManagedManifest
	Worker   worker.Config
	Boundary string
}

func TestBackupCutoverKilledHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_CUTOVER_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess helper")
	}
	dir, err := safefs.Open(filepath.Dir(path))
	testutil.Must(t, err)
	defer dir.Close()
	var input cutoverCrashInput
	testutil.Must(t, dir.ReadJSON(filepath.Base(path), &input))
	store, err := worker.Open(input.Manifest.Root, input.Worker, false)
	testutil.Must(t, err)
	s := &cutoverFixtureSession{m: input.Manifest, store: store, p: input.Spec.Managed.Producer, g: input.Spec.Managed.Generation}
	deps := backupCutoverDependencies{Services: &cutoverFixtureServices{}, Managed: func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		return fn(s)
	}, Verify: func(context.Context, backupNotificationOwner) error { return nil }, Fault: func(at string) error {
		if at == input.Boundary {
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	if err = runBackupCutover(context.Background(), input.Home, input.Config, input.Spec, false, deps); err != nil {
		t.Fatal(err)
	}
	t.Fatal("kill boundary not reached")
}
func TestBackupCutoverSurvivesSIGKILL(t *testing.T) {
	for _, point := range []string{"cutover-intent", "cutover-fenced", "cutover-active", "cutover-shared"} {
		t.Run(point, func(t *testing.T) {
			home, c, spec, deps, session := backupCutoverFixture(t)
			input := cutoverCrashInput{home, c, spec, session.m, session.config, point}
			path := filepath.Join(t.TempDir(), "input.json")
			testutil.Must(t, safefs.WritePrivateJSON(path, input))
			testutil.Must(t, session.store.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupCutoverKilledHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "DEPLOY_CUTOVER_CRASH_INPUT="+path)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("child did not reach kill: %v %s", err, output)
			}
			session.store, err = worker.Open(session.m.Root, session.config, false)
			testutil.Must(t, err)
			defer session.store.Close()
			testutil.Must(t, runBackupCutover(context.Background(), home, c, spec, false, deps))
			status, err := session.Status()
			testutil.Must(t, err)
			if !status.Active {
				t.Fatal(status)
			}
		})
	}
}

func TestBackupCutoverReviewCannotOverrideAttemptEvidence(t *testing.T) {
	state, c, now := legacyAttemptFixture(t)
	testutil.Must(t, drainNotificationOutboxWith(context.Background(), state, c, func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		return notification.DeliveryAttempt{Ambiguous: true}, errors.New("lost response")
	}, now, nil))
	ledgerTx(t, state, func(tx *sql.Tx) error {
		evidence, err := readBackupCutoverEvidence(tx)
		if err != nil {
			return err
		}
		decisions := map[string]backupCutoverDecision{}
		for _, row := range evidence.Rows {
			decisions[row.ID] = backupCutoverDecision{notify.Hash(row), "unsent", "operator assertion"}
		}
		if err = validateBackupCutoverDecisions(tx, evidence, decisions); err == nil {
			t.Fatal("assertion overrode attempted delivery")
		}
		for id, d := range decisions {
			d.Disposition = "held"
			decisions[id] = d
		}
		return validateBackupCutoverDecisions(tx, evidence, decisions)
	})
}
func TestSharedReportHeldRowsDoNotStarveFreshReports(t *testing.T) {
	f, api, deps, _ := setupReportFixture(t)
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		for n := 0; n < 40; n++ {
			event := notification.NotificationEvent{ID: fmt.Sprintf("held-%02d", n), Kind: "test", Message: "possibly already sent"}
			if err := enqueueNotification(tx, event, f.clock.Now.Add(-time.Hour)); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO backup_notification_legacy VALUES(?,?,?,?,?)", event.ID, "retained", notify.Hash(event), "held", "old evidence incomplete"); err != nil {
				return err
			}
		}
		return nil
	})
	testutil.Must(t, runSharedBackupReportWith(context.Background(), f.config, f.state, deps))
	if len(api.accepted) != 1 || api.accepted[0].ReportType != "daily" {
		t.Fatal("held rows starved fresh daily evaluation", api.accepted)
	}
	var health backupReportHealth
	testutil.Must(t, f.state.get("shared_report_health", &health))
	if health.Available {
		t.Fatal("held legacy work incorrectly reported complete")
	}
	var exported int
	testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM backup_notification_sources WHERE source LIKE 'held-%'").Scan(&exported))
	if exported != 0 {
		t.Fatal("held row resent")
	}
}

func TestBackupCutoverCLIDryRunAndModes(t *testing.T) {
	previous := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = previous }()
	for _, args := range [][]string{{"--local", "--review"}, {"--local", "--apply", "--spec", "/does-not-exist/spec.json"}, {"--local", "--revoke", "--spec", "/does-not-exist/spec.json"}} {
		var out bytes.Buffer
		testutil.Must(t, runBackupCutoverCLI(args, &out))
		if !strings.Contains(out.String(), "[DRY]") {
			t.Fatal(out.String())
		}
	}
	for _, args := range [][]string{{"--review"}, {"--local", "--apply"}, {"--local", "--review", "--apply"}, {"--local", "--review", "--spec", "/spec.json"}, {"--local", "--apply", "--spec", "relative.json"}} {
		if err := runBackupCutoverCLI(args, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid flags accepted", args)
		}
	}
}

func TestBackupCutoverAllowsCaptureAfterIntent(t *testing.T) {
	home, c, spec, deps, _ := backupCutoverFixture(t)
	ctx := context.Background()
	stop := errors.New("interrupted while capture continues")
	deps.Fault = func(at string) error {
		if at != "cutover-intent" {
			return nil
		}
		state, err := openBackupState(backupCutoverPaths(home, c).State, false)
		if err != nil {
			return err
		}
		defer state.Close()
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			control, err := incidentControl(tx)
			if err != nil {
				return err
			}
			control.Revision++
			if err = storeBackupEvaluationMeta(tx, "control", control); err != nil {
				return err
			}
			return enqueueNotification(tx, notification.NotificationEvent{ID: "captured-after-fence", Kind: "test", Message: "new capture outcome"}, time.Now().UTC())
		})
		if err != nil {
			return err
		}
		return stop
	}
	if err := runBackupCutover(ctx, home, c, spec, false, deps); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	deps.Fault = nil
	testutil.Must(t, runBackupCutover(ctx, home, c, spec, false, deps))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	var count int
	testutil.Must(t, state.db.QueryRow("SELECT count(*) FROM outbox WHERE id='captured-after-fence' AND accepted IS NULL").Scan(&count))
	if count != 1 {
		t.Fatal("capture work lost")
	}
}
