//go:build linux

package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	backupprotocol "github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/notification/led"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

type backupRecoveryLEDRegistry struct {
	states map[string]led.State
	writes int
}

func (f *backupRecoveryLEDRegistry) Set(_ context.Context, s led.State) error {
	f.writes++
	f.states[s.ID] = s
	return nil
}
func (f *backupRecoveryLEDRegistry) Clear(_ context.Context, id string) error {
	f.writes++
	delete(f.states, id)
	return nil
}
func (f *backupRecoveryLEDRegistry) List(context.Context) ([]led.Entry, error) {
	winners := map[string]string{}
	for id, s := range f.states {
		prior, ok := f.states[winners[s.LED]]
		if !ok || s.Priority > prior.Priority || (s.Priority == prior.Priority && id < prior.ID) {
			winners[s.LED] = id
		}
	}
	var entries []led.Entry
	for id, s := range f.states {
		entries = append(entries, led.Entry{State: s, OwnerUID: os.Getuid(), Winner: winners[s.LED] == id})
	}
	return entries, nil
}
func (f *backupRecoveryLEDRegistry) Status(ctx context.Context) (led.DaemonStatus, error) {
	status := led.DaemonStatus{LEDs: map[string]json.RawMessage{}, Patterns: map[string]json.RawMessage{}}
	entries, _ := f.List(ctx)
	for _, name := range []string{"green", "red"} {
		var selected *led.Entry
		for _, e := range entries {
			if e.LED == name && e.Winner {
				copy := e
				selected = &copy
			}
		}
		raw, err := json.Marshal(map[string]any{"enabled": true, "available": true, "default_pattern": "off", "selected_state": selected})
		if err != nil {
			return status, err
		}
		status.LEDs[name] = raw
	}
	for _, pattern := range []string{"off", "heartbeat", "blink-slow", "blink-fast", "blink-1", "blink-2"} {
		status.Patterns[pattern] = json.RawMessage(`[[1,1000]]`)
	}
	return status, nil
}

type backupLEDAdapterFixture struct {
	state         *backupState
	config        WorkerInstallation
	binding       backupNotificationBinding
	gatewayPath   string
	gatewayConfig worker.Config
	summary       notify.RecoverySummary
	inventory     worker.RecoveryLEDReview
	session       notify.RecoveryLEDEvidenceSession
	registry      *backupRecoveryLEDRegistry
	subject       string
}

// Exercise real archive/evaluator, producer ledger and quarantined worker APIs
// without binding a Unix socket or contacting a provider/LED daemon.
func newBackupLEDAdapterFixture(t *testing.T, configure ...func(*worker.Config)) *backupLEDAdapterFixture {
	return newBackupLEDAdapterFixtureAt(t, "/private/staged-led.sock", configure...)
}

func newBackupLEDAdapterFixtureAt(t *testing.T, socket string, configure ...func(*worker.Config)) *backupLEDAdapterFixture {
	t.Helper()
	ctx := context.Background()
	archive, state, attempt, header, data := archiveFixture(t)
	_, err := archive.receive(state, attempt, "fixture", 1<<20, func(file *os.File) (backupprotocol.SnapshotHeader, error) { _, e := file.Write(data); return header, e }, fixtureValidator)
	testutil.Must(t, err)
	subject := instanceKey(attempt.SourceID, attempt.Service)
	testutil.Must(t, archive.updateControl(state, func(c *ArchiveControl) error {
		v := c.Instances[subject]
		v.Generation = header.Generation
		v.LastSuccess = attempt.RequestedAt
		c.Instances[subject] = v
		return nil
	}))
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	config := WorkerInstallation{Schema: 1, WorkerID: control.WriterID, ArchiveID: control.ArchiveID, ConfigHash: control.ConfigHash, Policy: configfixture.WorkerPolicy(t), Qualification: map[string]bool{}}
	for _, gate := range []string{"source_restrictions", "rootless_runtime", "storage_durability", "notification_delivery", "external_monitor"} {
		config.Qualification[gate] = true
	}
	clock := reportHostClock()
	testutil.Must(t, state.put("health_probe", WorkerHealthProbe{Schema: 1, ObservedAt: clock.Now, BootID: clock.Boot, Uptime: clock.Uptime, ConfigHash: config.ConfigHash, Healthy: true, BackupHealthy: true, DeliveryHealthy: true}))
	testutil.Must(t, state.put("run", WorkerRun{ID: "run-one", ConfigHash: config.ConfigHash, StartedAt: attempt.RequestedAt, FinishedAt: clock.Now, Complete: true, Captured: 1}))
	p := notify.Producer{ID: control.WriterID, Epoch: "one"}
	cfg := worker.Config{Producers: []worker.Enrollment{{Producer: p, Required: true, Routes: []string{"ops"}, MaxMessages: 1000, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400}}, Routes: map[string]worker.Route{"ops": {Revision: "one", Primary: "telegram"}}, MaxMessages: 1000, MaxPayloadBytes: 1 << 20, MaxDatabaseBytes: 32 << 20, LEDBindings: []worker.LEDBinding{{ID: "backups", Producer: p, Subjects: []string{"*"}, LED: "green"}}}
	for _, adjust := range configure {
		adjust(&cfg)
	}
	path := t.TempDir()
	testutil.Must(t, os.Chmod(path, 0700))
	gateway, err := worker.Open(path, cfg, true)
	testutil.Must(t, err)
	defer gateway.Close()
	testutil.Must(t, gateway.Stage(p, "one"))
	testutil.Must(t, gateway.Activate(p, "one"))
	binding := backupNotificationBinding{Identity: gateway.Identity(), Producer: p, Archive: control.ArchiveID, Epoch: control.Epoch, Generation: "one", Route: "ops"}
	ledgerTx(t, state, func(tx *sql.Tx) error {
		if err := initializeBackupCommands(tx); err != nil {
			return err
		}
		return installBackupNotificationPipeline(tx, binding)
	})
	testutil.Must(t, state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: "shared", Binding: binding, Socket: "/private/staged-worker.sock"}))
	testutil.Must(t, state.observeIncident("capture", subject, "Fixture backup failure", clock.Now, config.Policy.Notifications))
	var evaluation backupNotificationEvaluation
	ledgerTx(t, state, func(tx *sql.Tx) error {
		var e error
		evaluation, e = evaluateBackupNotificationState(tx, config, reportHostClock())
		return e
	})
	testutil.Must(t, gateway.Replace(ctx, evaluation.Snapshot))
	registry := &backupRecoveryLEDRegistry{states: map[string]led.State{}}
	testutil.Must(t, (&worker.LEDReconciler{Store: gateway, API: registry, Budget: 2 * time.Second}).Step(ctx))
	artifact := filepath.Join(t.TempDir(), "snapshot")
	manifest, err := gateway.Snapshot(ctx, artifact)
	testutil.Must(t, err)
	ledgerTx(t, state, func(tx *sql.Tx) error {
		_, e := resolveBackupIncident(tx, "capture", subject, time.Now().UTC(), config.Policy.Notifications)
		return e
	})
	testutil.Must(t, gateway.Close())
	prepared, err := worker.QuarantineRecovery(ctx, path, artifact, cfg, manifest.ID, manifest.DatabaseHash)
	testutil.Must(t, err)
	if prepared.OriginalSourceHash == "" {
		t.Fatal("original source archive missing")
	}
	recovery, err := worker.OpenRecoverySession(ctx, path, cfg)
	testutil.Must(t, err)
	summary, err := reconcileBackupNotifications(ctx, state, config, recovery)
	testutil.Must(t, err)
	testutil.Must(t, recovery.Close())
	inventory, err := worker.ReviewRecoveryLED(ctx, path, cfg, socket, registry, false)
	testutil.Must(t, err)
	session, err := worker.OpenRecoveryLEDEvidenceSession(ctx, path, cfg)
	testutil.Must(t, err)
	f := &backupLEDAdapterFixture{state, config, binding, path, cfg, summary, inventory, session, registry, subject}
	t.Cleanup(func() { f.session.Close() })
	return f
}
func (f *backupLEDAdapterFixture) review(t *testing.T) worker.RecoveryLEDReview {
	t.Helper()
	testutil.Must(t, f.session.Close())
	v, err := worker.ReviewRecoveryLED(context.Background(), f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", nil, false)
	testutil.Must(t, err)
	f.session, err = worker.OpenRecoveryLEDEvidenceSession(context.Background(), f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	return v
}
func (f *backupLEDAdapterFixture) refresh(t *testing.T) {
	t.Helper()
	testutil.Must(t, f.session.Close())
	v, err := worker.ReviewRecoveryLED(context.Background(), f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", f.registry, true)
	testutil.Must(t, err)
	f.inventory = v
	f.session, err = worker.OpenRecoveryLEDEvidenceSession(context.Background(), f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
}
func backupLEDSequence(t *testing.T, s *backupState) uint64 {
	t.Helper()
	var n uint64
	testutil.Must(t, s.db.QueryRow("SELECT COALESCE(max(sequence),0) FROM backup_notification_snapshots").Scan(&n))
	return n
}
func assertBackupLEDDeliveryFenced(t *testing.T, f *backupLEDAdapterFixture) {
	t.Helper()
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		j, err := readBackupRecoveryAuthority(tx, f.config)
		if err != nil {
			return err
		}
		if notify.Hash(j.Manifest) != notify.Hash(f.summary.Manifest) {
			t.Fatal("LED exchange changed recovery manifest")
		}
		var exports uint64
		if err = tx.QueryRow("SELECT count(*) FROM notification_exports").Scan(&exports); err != nil {
			return err
		}
		if exports != j.Records {
			t.Fatal("LED exchange exported messages")
		}
		return nil
	})
	if err := requireLegacyNotificationOwner(f.state); err == nil {
		t.Fatal("LED evidence enabled legacy delivery")
	}
	testutil.Must(t, f.session.Close())
	if s, err := worker.Open(f.gatewayPath, f.gatewayConfig, false); err == nil {
		s.Close()
		t.Fatal("worker quarantine was removed")
	}
	var err error
	f.session, err = worker.OpenRecoveryLEDEvidenceSession(context.Background(), f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
}
func TestBackupLEDRecoveryRealEvidenceAndReplay(t *testing.T) {
	f := newBackupLEDAdapterFixture(t)
	before := backupLEDSequence(t, f.state)
	writes := f.registry.writes
	proof, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
	testutil.Must(t, err)
	if proof.Report.Snapshot.Sequence != before+1 || proof.Report.Snapshot.Subjects[0].Condition != "ok" || len(proof.Report.Transitions) != 0 || len(proof.Report.Snapshot.Support) != 0 {
		t.Fatal(proof.Report)
	}
	again, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
	testutil.Must(t, err)
	if notify.Hash(proof) != notify.Hash(again) || backupLEDSequence(t, f.state) != before+1 {
		t.Fatal("retry regenerated evidence")
	}
	view := f.review(t)
	if !view.Resolutions["backups"].DomainResolved || len(view.Resolutions["backups"].ProposedFaultIDs) == 0 || view.ClearAllowed || view.ActivationAllowed {
		t.Fatal(view.Resolutions)
	}
	if f.registry.writes != writes {
		t.Fatal("evidence exchange mutated LEDs")
	}
	assertBackupLEDDeliveryFenced(t, f)
}

type interruptedBackupLEDSession struct {
	notify.RecoveryLEDEvidenceSession
	lookupErr   error
	challenge   func(*notify.RecoveryLEDChallenge)
	afterSubmit func(*notify.RecoveryLEDEvidence) error
	submitCalls int
}

func (s *interruptedBackupLEDSession) LEDEvidence(ctx context.Context, id string, p notify.Producer) (notify.RecoveryLEDEvidence, error) {
	if s.lookupErr != nil {
		return notify.RecoveryLEDEvidence{}, s.lookupErr
	}
	return s.RecoveryLEDEvidenceSession.LEDEvidence(ctx, id, p)
}
func (s *interruptedBackupLEDSession) ChallengeLED(ctx context.Context, id string, p notify.Producer) (notify.RecoveryLEDChallenge, error) {
	v, e := s.RecoveryLEDEvidenceSession.ChallengeLED(ctx, id, p)
	if e == nil && s.challenge != nil {
		s.challenge(&v)
	}
	return v, e
}
func (s *interruptedBackupLEDSession) SubmitLED(ctx context.Context, ch notify.RecoveryLEDChallenge, r notify.RecoveryLEDReport) (notify.RecoveryLEDEvidence, error) {
	s.submitCalls++
	v, e := s.RecoveryLEDEvidenceSession.SubmitLED(ctx, ch, r)
	if e == nil && s.afterSubmit != nil {
		e = s.afterSubmit(&v)
	}
	return v, e
}

func TestBackupLEDRecoveryLostAcknowledgementDoesNotReevaluate(t *testing.T) {
	f := newBackupLEDAdapterFixture(t)
	before := backupLEDSequence(t, f.state)
	lost := &interruptedBackupLEDSession{RecoveryLEDEvidenceSession: f.session, afterSubmit: func(*notify.RecoveryLEDEvidence) error { return errors.New("lost acknowledgement") }}
	if _, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, lost, f.inventory.InventoryID); err == nil {
		t.Fatal("lost reply ignored")
	}
	testutil.Must(t, f.state.observeIncident("capture", f.subject, "New failure after frozen evidence", time.Now().UTC(), f.config.Policy.Notifications))
	// Expiry/reboot do not prevent importing a historical acknowledgement, and
	// must not cause a new evaluation of the changed source.
	proof, err := reconcileBackupLEDRecoveryWith(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID, func() backupEvaluationClock {
		return backupEvaluationClock{Now: time.Now().Add(time.Hour), Boot: "different", Uptime: 1}
	}, nil)
	testutil.Must(t, err)
	if backupLEDSequence(t, f.state) != before+1 || proof.Report.Snapshot.Subjects[0].Condition != "ok" {
		t.Fatal("lost response reevaluated source")
	}
	f.refresh(t)
	newer, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
	testutil.Must(t, err)
	if newer.Report.Snapshot.Sequence != before+2 || newer.Report.Snapshot.Subjects[0].Condition != "critical" {
		t.Fatal("new inventory missed new failure", newer.Report)
	}
	if f.review(t).Resolutions["backups"].DomainResolved {
		t.Fatal("new fault proposed for cleanup")
	}
}
func TestBackupLEDRecoveryRejectsMismatchedAuthorityAndTransport(t *testing.T) {
	for _, damage := range []string{"lookup", "operation", "before", "after", "producer", "inventory", "digest", "nonce", "expired", "future", "boot", "uptime", "config"} {
		t.Run(damage, func(t *testing.T) {
			f := newBackupLEDAdapterFixture(t)
			before := backupLEDSequence(t, f.state)
			session := &interruptedBackupLEDSession{RecoveryLEDEvidenceSession: f.session}
			config := f.config
			if damage == "lookup" {
				session.lookupErr = errors.New("worker unavailable")
			}
			if damage == "config" {
				config.ConfigHash = "changed"
			}
			session.challenge = func(ch *notify.RecoveryLEDChallenge) {
				switch damage {
				case "operation":
					ch.Operation = testutil.ID(t, "n")
				case "before":
					ch.Before.Recovery = "different"
				case "after":
					ch.After.Recovery = "different"
				case "producer":
					ch.Producer.Epoch = "different"
				case "inventory":
					ch.InventoryID = testutil.ID(t, "n")
				case "digest":
					ch.DomainHash = "invalid"
				case "nonce":
					ch.Nonce = "invalid"
				case "expired":
					ch.IssuedAt = ch.IssuedAt.Add(-time.Hour)
					ch.ExpiresAt = ch.IssuedAt.Add(10 * time.Minute)
				case "future":
					ch.IssuedAt = ch.IssuedAt.Add(time.Minute)
					ch.ExpiresAt = ch.IssuedAt.Add(10 * time.Minute)
				case "boot":
					ch.BootID = "different"
				case "uptime":
					ch.Uptime += 100
				}
			}
			if _, err := reconcileBackupLEDRecovery(context.Background(), f.state, config, session, f.inventory.InventoryID); err == nil {
				t.Fatal("invalid authority accepted")
			}
			if before != backupLEDSequence(t, f.state) || session.submitCalls != 0 {
				t.Fatal("invalid request evaluated or submitted evidence")
			}
		})
	}
}
func TestBackupLEDRecoveryRechecksSourceAfterWorkerCall(t *testing.T) {
	for _, damage := range []string{"receipt", "owner", "lease"} {
		t.Run(damage, func(t *testing.T) {
			f := newBackupLEDAdapterFixture(t)
			session := &interruptedBackupLEDSession{RecoveryLEDEvidenceSession: f.session, afterSubmit: func(v *notify.RecoveryLEDEvidence) error {
				switch damage {
				case "receipt":
					v.Hash = "wrong"
				case "owner":
					return f.state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: "shared", Binding: f.binding, Socket: "/private/staged-worker.sock"})
				case "lease":
					name := filepath.Join(f.state.directory, "notifier.lock")
					if err := os.Rename(name, name+".old"); err != nil {
						return err
					}
					return os.WriteFile(name, nil, 0600)
				}
				return nil
			}}
			if _, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, session, f.inventory.InventoryID); err == nil {
				t.Fatal("changed authority accepted")
			}
			var receipts int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM backup_led_recovery_reports WHERE receipt<>''").Scan(&receipts))
			if receipts != 0 {
				t.Fatal("unverified receipt persisted")
			}
		})
	}
}
func TestBackupLEDRecoveryFailuresRetainUnknownEvidence(t *testing.T) {
	for _, failure := range []string{"expired-probe", "paused", "qualification", "run-failed", "retired"} {
		t.Run(failure, func(t *testing.T) {
			f := newBackupLEDAdapterFixture(t)
			switch failure {
			case "expired-probe":
				testutil.Must(t, f.state.put("health_probe", WorkerHealthProbe{}))
			case "paused":
				testutil.Must(t, f.state.put("paused_until", time.Now().Add(time.Hour)))
			case "qualification":
				f.config.Qualification["rootless_runtime"] = false
			case "run-failed":
				testutil.Must(t, f.state.put("run", WorkerRun{ID: "failed", ConfigHash: f.config.ConfigHash, StartedAt: time.Now().Add(-time.Minute), FinishedAt: time.Now(), Failures: map[string]string{"fixture": "failure"}}))
			case "retired":
				ledgerTx(t, f.state, func(tx *sql.Tx) error {
					control, err := incidentControl(tx)
					if err != nil {
						return err
					}
					v := control.Instances[f.subject]
					v.Retired = true
					v.RetirementReason = "retired by test"
					control.Instances[f.subject] = v
					return storeBackupEvaluationMeta(tx, "control", control)
				})
			}
			proof, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
			testutil.Must(t, err)
			if proof.Report.Snapshot.Subjects[0].Condition == "ok" {
				t.Fatal("incomplete evidence reported health", failure)
			}
			if f.review(t).Resolutions["backups"].DomainResolved {
				t.Fatal("incomplete source evidence permitted cleanup")
			}
		})
	}
}

func TestBackupLEDRecoveryRejectsLostOrCorruptHistory(t *testing.T) {
	for _, damage := range []string{"row", "head", "schema", "snapshot", "receipt", "gateway-loss", "local-loss"} {
		t.Run(damage, func(t *testing.T) {
			f := newBackupLEDAdapterFixture(t)
			proof, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
			testutil.Must(t, err)
			var session notify.RecoveryLEDEvidenceSession = f.session
			switch damage {
			case "row":
				_, err = f.state.db.Exec("UPDATE backup_led_recovery_reports SET hash='bad'")
			case "head":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_led_recovery_head'")
			case "schema":
				_, err = f.state.db.Exec("DELETE FROM meta WHERE key='notification_led_recovery_schema'")
			case "snapshot":
				_, err = f.state.db.Exec("DELETE FROM backup_notification_snapshots WHERE sequence=?", proof.Report.Snapshot.Sequence)
			case "receipt":
				_, err = f.state.db.Exec("UPDATE backup_led_recovery_reports SET receipt='{}'")
			case "gateway-loss":
				session = &interruptedBackupLEDSession{RecoveryLEDEvidenceSession: f.session, lookupErr: notify.ErrRecoveryIncomplete}
			case "local-loss":
				_, err = f.state.db.Exec("DROP TABLE backup_led_recovery_reports; DELETE FROM meta WHERE key IN ('notification_led_recovery_schema','notification_led_recovery_head')")
			}
			testutil.Must(t, err)
			before := backupLEDSequence(t, f.state)
			if _, err = reconcileBackupLEDRecovery(context.Background(), f.state, f.config, session, f.inventory.InventoryID); err == nil {
				t.Fatal("damaged or lost evidence accepted")
			}
			if backupLEDSequence(t, f.state) != before {
				t.Fatal("failure regenerated evidence")
			}
		})
	}
}

func TestBackupLEDRecoveryHoldsBothLeasesAcrossCalls(t *testing.T) {
	f := newBackupLEDAdapterFixture(t)
	blocked := &interruptedBackupLEDSession{RecoveryLEDEvidenceSession: f.session, challenge: func(*notify.RecoveryLEDChallenge) {
		dir, err := safefs.Open(f.state.directory)
		testutil.Must(t, err)
		defer dir.Close()
		if lease, err := dir.Lock("notification-recovery.lock", true, false); err == nil {
			lease.Close()
			t.Error("coordinator lease released during worker call")
		}
		if lease, err := backupNotifierLease(f.state); err == nil {
			lease.Close()
			t.Error("notifier lease released during worker call")
		}
		// A local transaction remains available: external calls do not hold SQLite.
		testutil.Must(t, backupTransferTx(context.Background(), f.state, func(tx *sql.Tx) error { var count int; return tx.QueryRow("SELECT count(*) FROM meta").Scan(&count) }))
	}}
	_, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, blocked, f.inventory.InventoryID)
	testutil.Must(t, err)
}

type overBudgetBackupLEDSession struct {
	notify.RecoveryLEDEvidenceSession
	next      notify.RecoveryLEDChallenge
	submitted bool
}

func (s *overBudgetBackupLEDSession) LEDEvidence(context.Context, string, notify.Producer) (notify.RecoveryLEDEvidence, error) {
	return notify.RecoveryLEDEvidence{}, notify.ErrRecoveryIncomplete
}
func (s *overBudgetBackupLEDSession) ChallengeLED(context.Context, string, notify.Producer) (notify.RecoveryLEDChallenge, error) {
	return s.next, nil
}
func (s *overBudgetBackupLEDSession) SubmitLED(context.Context, notify.RecoveryLEDChallenge, notify.RecoveryLEDReport) (notify.RecoveryLEDEvidence, error) {
	s.submitted = true
	return notify.RecoveryLEDEvidence{}, errors.New("unexpected submission")
}
func TestBackupLEDRecoveryBoundsNewEvaluationsAndHonorsCancellation(t *testing.T) {
	f := newBackupLEDAdapterFixture(t)
	ctx := context.Background()
	var proof notify.RecoveryLEDEvidence
	for i := 0; i < maxBackupLEDRecoveryReports; i++ {
		if i > 0 {
			f.refresh(t)
		}
		var err error
		proof, err = reconcileBackupLEDRecovery(ctx, f.state, f.config, f.session, f.inventory.InventoryID)
		testutil.Must(t, err)
	}
	ch := proof.Challenge
	ch.InventoryID = testutil.ID(t, "n")
	ch.Nonce = testutil.ID(t, "n")
	clock := reportHostClock()
	ch.IssuedAt = clock.Now
	ch.ExpiresAt = clock.Now.Add(10 * time.Minute)
	ch.BootID = clock.Boot
	ch.Uptime = clock.Uptime
	session := &overBudgetBackupLEDSession{RecoveryLEDEvidenceSession: f.session, next: ch}
	before := backupLEDSequence(t, f.state)
	if _, err := reconcileBackupLEDRecovery(ctx, f.state, f.config, session, ch.InventoryID); err == nil {
		t.Fatal("source history limit bypassed")
	}
	if before != backupLEDSequence(t, f.state) || session.submitted {
		t.Fatal("over-budget request evaluated or sent")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reconcileBackupLEDRecovery(cancelled, f.state, f.config, f.session, f.inventory.InventoryID); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}
func TestBackupLEDRecoveryCancellationAfterAcceptancePreservesRetry(t *testing.T) {
	f := newBackupLEDAdapterFixture(t)
	before := backupLEDSequence(t, f.state)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &interruptedBackupLEDSession{RecoveryLEDEvidenceSession: f.session, afterSubmit: func(*notify.RecoveryLEDEvidence) error { cancel(); return nil }}
	if _, err := reconcileBackupLEDRecovery(ctx, f.state, f.config, session, f.inventory.InventoryID); err == nil {
		t.Fatal("cancelled acknowledgement committed locally")
	}
	proof, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
	testutil.Must(t, err)
	if before+1 != backupLEDSequence(t, f.state) || proof.Report.Snapshot.Sequence != before+1 {
		t.Fatal("cancellation reevaluated the report")
	}
}

func TestBackupLEDRecoverySurvivesDeliveryEpochCommit(t *testing.T) {
	f := newBackupLEDAdapterFixture(t)
	ctx := context.Background()
	original, err := reconcileBackupLEDRecovery(ctx, f.state, f.config, f.session, f.inventory.InventoryID)
	testutil.Must(t, err)
	testutil.Must(t, f.session.Close())
	session, err := worker.OpenRecoveryProducerSession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	defer session.Close()
	_, err = prepareBackupRecoveryOwnership(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	commit, err := commitBackupNotificationRecovery(ctx, f.state, f.config, session)
	testutil.Must(t, err)
	if commit.After.Identity != original.Challenge.After {
		t.Fatal("delivery commit used another recovery")
	}
	testutil.Must(t, session.Close())
	f.session, err = worker.OpenRecoveryLEDEvidenceSession(ctx, f.gatewayPath, f.gatewayConfig)
	testutil.Must(t, err)
	replay, err := reconcileBackupLEDRecovery(ctx, f.state, f.config, f.session, f.inventory.InventoryID)
	testutil.Must(t, err)
	if notify.Hash(original) != notify.Hash(replay) {
		t.Fatal("delivery commit rewrote LED proof")
	}
	f.refresh(t)
	next, err := reconcileBackupLEDRecovery(ctx, f.state, f.config, f.session, f.inventory.InventoryID)
	testutil.Must(t, err)
	if next.Challenge.Before != f.binding.Identity || next.Challenge.After != commit.After.Identity || next.Report.Snapshot.Sequence != original.Report.Snapshot.Sequence+1 {
		t.Fatal("recovery provenance changed")
	}
	assertBackupLEDDeliveryFenced(t, f)
}
