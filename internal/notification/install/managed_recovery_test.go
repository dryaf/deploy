package install

import (
	"bytes"
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/process"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"encoding/json"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func publishManagedRecovery(t *testing.T, spec ManagedSpec) worker.RecoveryPublicationResult {
	return publishManagedRecoveryFrom(t, spec, "", nil)
}
func publishManagedRecoveryFrom(t *testing.T, spec ManagedSpec, restored string, restoreManifest *worker.SnapshotManifest) worker.RecoveryPublicationResult {
	return publishManagedRecoveryFromUsing(t, spec, restored, restoreManifest, nil)
}
func publishManagedRecoveryFromUsing(t *testing.T, spec ManagedSpec, restored string, restoreManifest *worker.SnapshotManifest, reconcile func(context.Context, string, worker.Config)) worker.RecoveryPublicationResult {
	t.Helper()
	ctx := context.Background()
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var s *worker.Store
	if owner, e := worker.InspectManagedDeliveryOwner(spec.State); e == nil {
		revision, e := safefs.NewID("n")
		if e != nil {
			t.Fatal(e)
		}
		s, err = worker.OpenManagedRuntime(spec.State, c.Worker, owner.Identity, revision)
	} else {
		s, err = worker.Open(spec.State, c.Worker, false)
	}
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	m, err := s.Snapshot(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	history := snapshot
	historyManifest := m
	if restoreManifest != nil {
		snapshot, m = restored, *restoreManifest
	}
	var q worker.RecoveryQuarantineView
	if owner, e := worker.InspectManagedDeliveryOwner(spec.State); e == nil {
		revision, e := safefs.NewID("n")
		if e != nil {
			t.Fatal(e)
		}
		q, err = worker.QuarantineManagedDeliveryRecovery(ctx, spec.State, snapshot, c.Worker, m.ID, m.DatabaseHash, revision, owner)
	} else {
		q, err = worker.QuarantineRecovery(ctx, spec.State, snapshot, c.Worker, m.ID, m.DatabaseHash)
	}
	if err != nil {
		t.Fatal(err)
	}
	intake := func(operation string, extra ...string) {
		t.Helper()
		args := append([]string{operation, "--local", "--config", spec.RuntimeFile, "--state-dir", spec.State, "--operation", q.Operation}, extra...)
		var output bytes.Buffer
		if err := runRecoveryIntakeCLIWith(args, &output, telegramCheckTransport{}); err != nil {
			t.Fatal(operation, err)
		}
	}
	if c.Telegram != nil {
		intake("recovery-telegram-history", "--from", history, "--snapshot-id", historyManifest.ID, "--database-hash", historyManifest.DatabaseHash)
		intake("recovery-telegram-drain", "--receivers-stopped")
	}

	for _, p := range c.Worker.Producers {
		if _, err = worker.FenceRecoveryOwnership(ctx, spec.State, c.Worker, p.Producer); err != nil {
			t.Fatal(err)
		}
	}
	if reconcile != nil {
		reconcile(ctx, spec.State, c.Worker)
	}
	if c.Telegram != nil {
		intake("recovery-requests-seal", "--reconciliation-complete")
		intake("recovery-telegram-cursor")
	}

	review, err := worker.ReviewRecovery(ctx, spec.State, c.Worker, q.Operation)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := worker.CheckpointRecovery(ctx, spec.State, c.Worker, q.Operation, review.ReviewHash)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := worker.VerifyRecoveryCheckpoint(ctx, spec.State, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash)
	if err != nil {
		t.Fatal(err)
	}
	image, err := worker.PrepareRecoveryPublication(ctx, spec.State, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.PublishRecovery(ctx, spec.State, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func managedRecoveryFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeManagedServices, ManagedManifest) {
	t.Helper()
	spec, i, f, _ := managedFixture(t)
	before, err := i.prepare(context.Background(), "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := publishManagedRecovery(t, spec)
	spec.RecoveryCommitHash = result.Commit.CommitHash
	i.recoveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	return spec, i, f, before
}

func TestManagedRecoveryLifecycle(t *testing.T) {
	spec, i, _, before := managedRecoveryFixture(t)
	ctx := context.Background()
	m, err := i.prepare(ctx, "adopt-recovery", spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.Identity.Worker != before.Identity.Worker || m.Identity.Recovery == before.Identity.Recovery || m.RecoveryCommitHash != spec.RecoveryCommitHash || m.HelperRevision != m.Revision {
		t.Fatal(m)
	}
	replay, err := i.resume(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(m) != protocol.Hash(replay) {
		t.Fatal("replay changed manifest")
	}
	spec.RecoveryCommitHash = ""
	updated, err := i.prepare(ctx, "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Identity != m.Identity || updated.RecoveryCommitHash != m.RecoveryCommitHash {
		t.Fatal(updated)
	}
	if _, _, _, err = reviewManagedRollback(spec.Root, before.Revision); err == nil {
		t.Fatal("rollback crossed recovery identity")
	}
	if _, _, _, err = reviewManagedRollback(spec.Root, m.Revision); err != nil {
		t.Fatal(err)
	}
	removed, err := i.uninstall(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if removed.RecoveryCommitHash != m.RecoveryCommitHash || removed.Mode != "uninstalled" {
		t.Fatal(removed)
	}
	if _, err = os.Stat(filepath.Join(spec.State, "recovery.json")); err != nil {
		t.Fatal("quarantine was removed", err)
	}
}

func TestManagedRecoveryInterrupted(t *testing.T) {
	for _, point := range []string{"prepared", "stop-returned", "recovery-adopted", "stopped", "files", "unit-published", "manifest-published", "published", "reload-returned", "reloaded", "start-returned", "started", "complete"} {
		t.Run(point, func(t *testing.T) {
			spec, i, _, _ := managedRecoveryFixture(t)
			stop := errors.New("interrupted")
			i.fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			if _, err := i.prepare(context.Background(), "adopt-recovery", spec); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			if m.RecoveryCommitHash != spec.RecoveryCommitHash {
				t.Fatal(m)
			}
		})
	}
}

func TestManagedRecoveryRefusesWrongPinsAndCapabilities(t *testing.T) {
	for _, kind := range []string{"pin", "candidate", "helper", "update", "setup"} {
		t.Run(kind, func(t *testing.T) {
			spec, i, _, before := managedRecoveryFixture(t)
			operation := "adopt-recovery"
			switch kind {
			case "pin":
				spec.RecoveryCommitHash = protocol.Hash("wrong")
			case "candidate", "helper":
				i.recoveryProbe = func(_ context.Context, _ *safefs.Directory, name, _ string) error {
					if (kind == "helper" && filepath.Base(name) == "recovery-helper") || (kind == "candidate" && filepath.Base(name) == "deploy") {
						return errors.New("unsupported binary")
					}
					return nil
				}
			case "update":
				operation = "update"
			case "setup":
				operation = "setup"
			}
			if _, err := i.prepare(context.Background(), operation, spec); err == nil {
				t.Fatal("accepted " + kind)
			}
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			j, err := readManagedJournal(d)
			if err != nil {
				t.Fatal(err)
			}
			if j.Phase != "complete" || protocol.Hash(j.Intent.After) != protocol.Hash(before) {
				t.Fatal("failed preflight changed installation")
			}
			if _, err = os.Stat(filepath.Join(spec.State, "recovery-verification.json")); !os.IsNotExist(err) {
				t.Fatal("failed preflight adopted recovery", err)
			}
		})
	}
}

func TestManagedRecoverySIGKILL(t *testing.T) {
	for _, point := range []string{"prepared", "stop-returned", "recovery-adopted", "stopped", "files", "unit-published", "manifest-published", "published", "reload-returned", "reloaded", "start-returned", "started", "complete"} {
		t.Run(point, func(t *testing.T) {
			spec, i, f, _ := managedRecoveryFixture(t)
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = d.WriteJSON("input.json", managedCrashInput{Spec: spec, Operation: "adopt-recovery", Boundary: point, Service: f.state}, false); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestManagedCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DEPLOY_MANAGED_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(cmd, 50*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", string(out))
			}
			if err = d.ReadJSON("manager.json", &f.state); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(spec.BinaryFile); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(spec.RuntimeFile); err != nil {
				t.Fatal(err)
			}
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			if m.RecoveryCommitHash != spec.RecoveryCommitHash {
				t.Fatal(m)
			}
			again, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			if protocol.Hash(m) != protocol.Hash(again) {
				t.Fatal("replay changed installation")
			}
		})
	}
}

func TestManagedRecoveryCLIDryRun(t *testing.T) {
	spec, _, _, _ := managedFixture(t)
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	spec.UnitDirectory = filepath.Join(home, "systemd", "user")
	spec.RecoveryCommitHash = protocol.Hash("explicit pin")
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "spec.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	saved := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = saved }()
	if err = RunCLI([]string{"adopt-recovery", "--local", "--spec", path}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"adopt-recovery", "--spec", path}, {"adopt-recovery", "--local", "--spec", path, "--activate"}, {"update", "--local", "--spec", path}} {
		if err = RunCLI(args); err == nil {
			t.Fatal("invalid adoption accepted", args)
		}
	}
	if _, err = os.Stat(spec.Root); !os.IsNotExist(err) {
		t.Fatal("dry run created installation", err)
	}
	if _, err = os.Stat(filepath.Join(spec.State, "recovery-verification.json")); !os.IsNotExist(err) {
		t.Fatal("dry run adopted state", err)
	}
}
