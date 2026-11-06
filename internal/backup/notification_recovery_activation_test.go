//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/ui"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type recoveryReportAPI struct {
	s     *worker.Store
	calls int
}

func (a *recoveryReportAPI) Call(ctx context.Context, r notify.Request, out any) (notify.Identity, error) {
	a.calls++
	r.Version = notify.Version
	reply := a.s.Handle(ctx, r)
	if !reply.OK {
		return reply.Identity, fmt.Errorf("fixture API %s: %s", r.Operation, reply.Error)
	}
	if out != nil {
		return reply.Identity, json.Unmarshal(reply.Result, out)
	}
	return reply.Identity, nil
}

type recoveryReportServices struct {
	cutoverFixtureServices
	probeError error
}

func (s *recoveryReportServices) PrepareNextRecovery(context.Context, WorkerUnitPaths, string) error {
	return s.probeError
}

func (s *recoveryReportServices) PrepareRecovery(context.Context, WorkerUnitPaths, string) error {
	return s.probeError
}

type recoveryReportSession struct{ *cutoverFixtureSession }

func (s recoveryReportSession) RequireRecoveryCommit(ctx context.Context, p notify.RecoveryProducerCommit) error {
	return s.store.RequireRecoveredProducer(ctx, p)
}

func backupRecoveryActivationFixture(t *testing.T, canonical ...bool) (backupHandoffFixture, string, backupRecoveryActivationSpec, backupCutoverDependencies, *cutoverFixtureSession) {
	t.Helper()
	return backupRecoveryActivationFixtureMode(t, false, canonical...)
}

func backupRecoveryActivationFixtureMode(t *testing.T, interactive bool, canonical ...bool) (backupHandoffFixture, string, backupRecoveryActivationSpec, backupCutoverDependencies, *cutoverFixtureSession) {
	t.Helper()
	home := t.TempDir()
	var before func(*backupEvaluationFixture)
	if len(canonical) > 0 && canonical[0] {
		before = func(f *backupEvaluationFixture) {
			var e backupCutoverEvidence
			testutil.Must(t, backupTransferTx(context.Background(), f.state, func(tx *sql.Tx) error { var err error; e, err = readBackupCutoverEvidence(tx); return err }))
			cutover := backupCutoverIntent{Spec: backupCutoverSpec{Version: 1, Managed: install.ManagedProducerRequest{Root: home, ManifestHash: notify.Hash("original-managed-installation"), Producer: f.binding.Producer, Generation: f.binding.Generation, Route: f.binding.Route}, HelperHash: notify.Hash("old-helper"), ConfigHash: notify.Hash(f.config), EvidenceHash: notify.Hash(e)}, Evidence: e}
			d, err := safefs.Open(f.state.directory)
			testutil.Must(t, err)
			testutil.Must(t, d.WriteJSON(backupCutoverFile, cutover, false))
			d.Close()
			testutil.Must(t, f.state.put("notification_cutover", notify.Hash(cutover)))
			var owner backupNotificationOwner
			testutil.Must(t, f.state.get("notification_owner", &owner))
			owner.CutoverHash = notify.Hash(cutover)
			testutil.Must(t, f.state.put("notification_owner", owner))
		}
	}
	f := backupHandoffTestFixtureWith(t, interactive, before)
	ctx := context.Background()
	var out bytes.Buffer
	testutil.Must(t, runBackupRecoveryHandoff(ctx, f.options, f.config, f.gatewayConfig, &out, nil))
	var handoff backupRecoveryHandoffResult
	testutil.Must(t, json.Unmarshal(out.Bytes(), &handoff))
	if interactive {
		completeBackupRecoveryIntake(t, f)
	}
	review, err := worker.ReviewRecovery(ctx, f.gatewayPath, f.gatewayConfig, f.options.Operation)
	testutil.Must(t, err)
	cp, err := worker.CheckpointRecovery(ctx, f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash)
	testutil.Must(t, err)
	proof, err := worker.VerifyRecoveryCheckpoint(ctx, f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash, cp.CheckpointHash)
	testutil.Must(t, err)
	image, err := worker.PrepareRecoveryPublication(ctx, f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
	testutil.Must(t, err)
	publication, err := worker.PublishRecovery(ctx, f.gatewayPath, f.gatewayConfig, f.options.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
	testutil.Must(t, err)
	adoption, err := worker.AdoptPublishedRecoveryVerification(ctx, f.gatewayPath, f.gatewayConfig, publication.Commit.CommitHash)
	testutil.Must(t, err)
	revision, err := safefs.NewID("n")
	testutil.Must(t, err)
	installation, err := safefs.NewID("n")
	testutil.Must(t, err)
	owner := worker.ManagedDeliveryOwner{Version: 1, Root: home, Installation: installation, Identity: adoption.After, ConfigHash: notify.Hash(f.gatewayConfig)}
	recovery := worker.ManagedDeliveryRecovery{Version: 1, Owner: owner, Before: adoption.Before, After: adoption.After, CommitHash: adoption.CommitHash}
	s, err := worker.OpenManagedRecoveryVerification(f.gatewayPath, f.gatewayConfig, adoption.After, revision, adoption.CommitHash)
	testutil.Must(t, err)
	testutil.Must(t, s.EnrollManagedDeliveryRecovery(recovery))
	testutil.Must(t, s.Close())
	s, err = worker.OpenManagedRecoveredDelivery(f.gatewayPath, f.gatewayConfig, revision, recovery, notify.Hash(owner), notify.Hash("fixture-activation"))
	testutil.Must(t, err)
	t.Cleanup(func() { s.Close() })
	paths := backupCutoverPaths(home, f.config)
	testutil.Must(t, os.MkdirAll(filepath.Dir(paths.State), 0700))
	testutil.Must(t, f.state.Close())
	testutil.Must(t, os.Rename(f.state.directory, paths.State))
	f.state, err = openBackupState(paths.State, false)
	testutil.Must(t, err)
	t.Cleanup(func() { f.state.Close() })
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	testutil.Must(t, root.Mkdir(".config/deploy/backup-worker"))
	testutil.Must(t, root.WriteJSON(".config/deploy/backup-worker/runtime.json", f.config, false))
	testutil.Must(t, root.Mkdir(".local/state/deploy/locks"))
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	testutil.Must(t, err)
	lock.Close()
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", WorkerSetupJournal{Stage: "complete", WorkerID: f.config.WorkerID, ArchiveID: f.config.ArchiveID}, false))
	session := &cutoverFixtureSession{m: install.ManagedManifest{Root: home, Revision: revision, Identity: adoption.After, Mode: "delivery", DeliveryRecovery: &recovery, UnitDirectory: filepath.Join(home, ".config/systemd/user"), Paths: install.ServicePaths{State: f.gatewayPath, Socket: f.client.Socket}}, store: s, p: handoff.Delivery.After.Producer, g: handoff.Delivery.After.Generation, config: f.gatewayConfig}
	spec := backupRecoveryActivationSpec{Version: 1, Managed: install.ManagedProducerRequest{Root: home, ManifestHash: notify.Hash(session.m), Producer: session.p, Generation: session.g, Route: handoff.Delivery.After.Route}, HelperHash: notify.Hash("helper"), ConfigHash: notify.Hash(f.config), CommitHash: notify.Hash(handoff.Delivery)}
	services := &recoveryReportServices{}
	deps := backupCutoverDependencies{Services: services, Managed: func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		return fn(recoveryReportSession{session})
	}, Verify: func(context.Context, backupNotificationOwner) error {
		status, err := session.Status()
		if err != nil {
			return err
		}
		if !status.Active {
			return errors.New("producer inactive")
		}
		return nil
	}}
	return f, home, spec, deps, session
}

func TestBackupRecoveryActivationLifecycle(t *testing.T) {
	f, home, spec, deps, session := backupRecoveryActivationFixture(t)
	ctx := context.Background()
	if err := requireNotificationRecoveryInactiveTx(f.state); err == nil {
		t.Fatal("unadmitted recovery reports allowed")
	}
	testutil.Must(t, runBackupRecoveryActivation(ctx, home, f.config, spec, false, deps))
	testutil.Must(t, requireNotificationRecoveryInactiveTx(f.state))
	var owner backupNotificationOwner
	testutil.Must(t, f.state.get("notification_owner", &owner))
	testutil.Must(t, requireBackupCutoverOwner(f.state, owner))
	f.binding = owner.Binding
	f.advance(1)
	f.refreshProbe(t)
	testutil.Must(t, f.state.resolveIncident("capture", f.subject, f.clock.Now, f.config.Policy.Notifications))
	f.advance(1)
	f.observe(t, f.subject, "new fault after recovery", f.clock.Now)
	_, messages := f.evaluate(t)
	if len(messages) == 0 {
		t.Fatal("recovered source produced no current report")
	}
	for _, m := range messages {
		if m.Generation != spec.Managed.Generation {
			t.Fatal("old generation reused")
		}
	}
	api := &recoveryReportAPI{s: session.store}
	testutil.Must(t, transferBackupNotifications(ctx, f.state, api))
	for _, m := range messages {
		job, granted, e := session.store.Grant(ctx, m.Producer, m.ID)
		testutil.Must(t, e)
		if !granted {
			t.Fatal("recovered new report not dispatchable", m.ID)
		}
		binding := interaction.DeliveryBinding{Destination: interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100"}, Native: interaction.NativeReply{ID: fmt.Sprint(m.Sequence), Sender: "123"}}
		testutil.Must(t, session.store.Finish(ctx, job, worker.Result{Accepted: true, Binding: &binding}))
	}
	testutil.Must(t, transferBackupNotifications(ctx, f.state, api))
	testutil.Must(t, transferBackupNotifications(ctx, f.state, api))
	var cursor int64
	testutil.Must(t, f.state.db.QueryRow("SELECT cursor FROM notification_producer").Scan(&cursor))
	if cursor == 0 {
		t.Fatal("new epoch receipts not acknowledged")
	}
	testutil.Must(t, runSharedBackupReportWith(ctx, f.config, f.state, backupReportDependencies{API: api, Clock: func() backupEvaluationClock { return f.clock }, Heartbeat: func(context.Context, string, bool) error { return nil }}))
	testutil.Must(t, runBackupRecoveryActivation(ctx, home, f.config, spec, false, deps))
	testutil.Must(t, runBackupRecoveryActivation(ctx, home, f.config, spec, true, deps))
	testutil.Must(t, runBackupRecoveryActivation(ctx, home, f.config, spec, true, deps))
	status, err := session.Status()
	testutil.Must(t, err)
	if status.State != "revoked" || status.Active {
		t.Fatal(status)
	}
	if requireNotificationRecoveryInactiveTx(f.state) == nil {
		t.Fatal("revoked recovery reports allowed")
	}
	if runBackupRecoveryActivation(ctx, home, f.config, spec, false, deps) == nil {
		t.Fatal("revoked generation revived")
	}
}

func requireNotificationRecoveryInactiveTx(s *backupState) error {
	return backupTransferTx(context.Background(), s, requireNotificationRecoveryInactive)
}

func TestBackupRecoveryActivationInterrupted(t *testing.T) {
	for _, at := range []string{"recovery-activation-intent", "recovery-activation-sql", "recovery-activation-active", "recovery-activation-file", "recovery-activation-shared"} {
		t.Run(at, func(t *testing.T) {
			f, home, spec, deps, _ := backupRecoveryActivationFixture(t)
			stop := errors.New("interrupted")
			deps.Fault = func(p string) error {
				if p == at {
					return stop
				}
				return nil
			}
			if err := runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			deps.Fault = nil
			testutil.Must(t, runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps))
			testutil.Must(t, requireNotificationRecoveryInactiveTx(f.state))
		})
	}
}

func TestBackupRecoveryActivationLostEvidence(t *testing.T) {
	for _, anchor := range []string{"intent", "complete", "sql", "sql-complete", "summary", "record"} {
		t.Run(anchor, func(t *testing.T) {
			f, home, spec, deps, _ := backupRecoveryActivationFixture(t)
			testutil.Must(t, runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps))
			switch anchor {
			case "intent":
				testutil.Must(t, os.Remove(filepath.Join(f.state.directory, recoveryActivationFile)))
			case "complete":
				testutil.Must(t, os.Remove(filepath.Join(f.state.directory, recoveryActivationFile+".complete")))
			case "sql":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key=?", recoveryActivationKey)
				testutil.Must(t, err)
			case "sql-complete":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key=?", recoveryActivationKey+"_complete")
				testutil.Must(t, err)
			case "summary":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key='notification_recovery_summary'")
				testutil.Must(t, err)
			case "record":
				_, err := f.state.db.Exec("DELETE FROM backup_notification_recovery_records")
				testutil.Must(t, err)
			}
			if _, err := requireRecoveryActivationFiles(f.state); err == nil {
				t.Fatal("lost evidence admitted reports")
			}
			if runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps) == nil {
				t.Fatal("lost completed evidence recreated")
			}
		})
	}
}

func TestBackupRecoveryActivationRejectsChangedProofAndHelper(t *testing.T) {
	for _, damage := range []string{"helper", "generation", "source-commit", "checkpoint"} {
		t.Run(damage, func(t *testing.T) {
			f, home, spec, deps, session := backupRecoveryActivationFixture(t)
			switch damage {
			case "helper":
				deps.Services.(*recoveryReportServices).probeError = errors.New("unsupported helper")
			case "generation":
				spec.Managed.Generation = "foreign-generation"
			case "source-commit":
				spec.CommitHash = notify.Hash("another-source")
			case "checkpoint":
				paths, err := filepath.Glob(filepath.Join(f.gatewayPath, "recovery", f.options.Operation, "checkpoints", "*", "projection.db"))
				testutil.Must(t, err)
				if len(paths) != 1 {
					t.Fatal(paths)
				}
				testutil.Must(t, os.WriteFile(paths[0], nil, 0600))
			}
			if runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps) == nil {
				t.Fatal("changed pin admitted")
			}
			status, err := session.Status()
			testutil.Must(t, err)
			if status.Active {
				t.Fatal("failed preflight activated producer")
			}
		})
	}
}

func TestBackupRecoveryActivationPreservesOriginalCutover(t *testing.T) {
	f, home, spec, deps, _ := backupRecoveryActivationFixture(t, true)
	testutil.Must(t, runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps))
	var owner backupNotificationOwner
	testutil.Must(t, f.state.get("notification_owner", &owner))
	testutil.Must(t, requireBackupCutoverOwner(f.state, owner))
	testutil.Must(t, os.Remove(filepath.Join(f.state.directory, backupCutoverFile)))
	if requireBackupCutoverOwner(f.state, owner) == nil {
		t.Fatal("lost cutover accepted")
	}
	if runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps) == nil {
		t.Fatal("lost original cutover recreated")
	}
}

func TestBackupRecoveryActivationCLIDryRun(t *testing.T) {
	prior := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = prior }()
	file := filepath.Join(t.TempDir(), "absent-spec.json")
	for _, args := range [][]string{{"--local", "--review"}, {"--local", "--spec", file}, {"--local", "--spec", file, "--revoke"}} {
		testutil.Must(t, runBackupRecoveryActivationCLI(args, io.Discard))
	}
	for _, args := range [][]string{{"--review"}, {"--local"}, {"--local", "--review", "--revoke"}, {"--local", "--review", "--spec", file}, {"--local", "--spec", "relative"}, {"--local", "--spec", file, "extra"}} {
		if runBackupRecoveryActivationCLI(args, io.Discard) == nil {
			t.Fatal("bad flags accepted", args)
		}
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("dry-run touched specification", err)
	}
}
