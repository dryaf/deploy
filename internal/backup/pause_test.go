//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupSchedulerWaitsForNotifier(t *testing.T) {
	for _, operation := range []string{"admission", "completion", "pause", "catchup"} {
		t.Run(operation, func(t *testing.T) {
			_, state, c, _ := healthyWorkerFixture(t)
			if operation == "catchup" {
				testutil.Must(t, setWorkerPause(state, time.Now().Add(-time.Minute)))
			}
			// Separate handles model the independent notifier and worker/host
			// processes sharing the same flock and SQLite state.
			other, err := openBackupState(state.directory, false)
			testutil.Must(t, err)
			defer other.Close()
			lease, err := schedulerLock(state)
			testutil.Must(t, err)
			defer lease.Close()
			run := WorkerRun{ID: testutil.ID(t, "r"), ConfigHash: c.ConfigHash, StartedAt: time.Now().UTC(), Complete: operation == "completion", Captured: 1, FinishedAt: time.Now().UTC()}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch operation {
				case "admission":
					_, err := admitWorkerRun(ctx, other, run)
					done <- err
				case "completion":
					done <- finishWorkerRun(other, run, "")
				case "pause":
					done <- setWorkerPause(other, time.Now().Add(time.Hour))
				case "catchup":
					done <- consumeBackupCatchup(other, c, false, func() error { return nil })
				}
			}()
			select {
			case err := <-done:
				t.Fatalf("scheduler operation did not wait for the heartbeat: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			lease.Close()
			select {
			case err := <-done:
				testutil.Must(t, err)
			case <-ctx.Done():
				t.Fatal("scheduler did not resume after the heartbeat")
			}
			if operation == "admission" || operation == "completion" {
				var saved WorkerRun
				testutil.Must(t, state.get("run", &saved))
				if saved.ID != run.ID || saved.Complete != run.Complete || !saved.FinishedAt.Equal(run.FinishedAt) {
					t.Fatalf("run evidence lost after lock contention: %+v", saved)
				}
			}
		})
	}
}

func TestBackupSchedulerCancellationAndPauseOrdering(t *testing.T) {
	_, state, c, _ := healthyWorkerFixture(t)
	lease, err := schedulerLock(state)
	testutil.Must(t, err)
	defer lease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	run := WorkerRun{ID: testutil.ID(t, "r"), ConfigHash: c.ConfigHash, StartedAt: time.Now().UTC()}
	_, err = admitWorkerRun(ctx, state, run)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("admission did not respect its deadline: %v", err)
	}
	var saved WorkerRun
	testutil.Must(t, state.get("run", &saved))
	if saved.ID == run.ID {
		t.Fatal("canceled admission overwrote current run")
	}
	// A regular health tick has no scheduler mutation to perform and should
	// remain healthy while a heartbeat owns the lease.
	testutil.Must(t, consumeBackupCatchup(state, c, false, func() error { t.Fatal("unexpected catch-up"); return nil }))
	// Simulate the pause transaction committing before it releases the lease.
	testutil.Must(t, state.put("paused_until", time.Now().Add(time.Hour)))
	done := make(chan error, 1)
	go func() { _, err := admitWorkerRun(context.Background(), state, run); done <- err }()
	lease.Close()
	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "paused") {
			t.Fatalf("admission ignored the committed pause: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("admission did not finish")
	}
}

func TestBackupSchedulerDoesNotRetryFilesystemErrors(t *testing.T) {
	_, state, _, _ := healthyWorkerFixture(t)
	testutil.Must(t, os.Mkdir(filepath.Join(state.directory, "scheduler.lock"), 0700))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := schedulerLockContext(ctx, state)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("filesystem error retried as lock contention: %v", err)
	}
}

func TestBackupReviewPausedWorkerCannotReportHealthyCoverage(t *testing.T) {
	_, s, c, _ := healthyWorkerFixture(t)
	if err := s.put("paused_until", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	summary, healthy, err := evaluateWorkerHealth(c, s, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if healthy || !strings.Contains(strings.ToLower(summary), "paus") {
		t.Fatalf("pause is absent from health/report: healthy=%v %s", healthy, summary)
	}
}

func TestBackupReviewResumeAdmitsCatchup(t *testing.T) {
	_, s, c, _ := healthyWorkerFixture(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "commands")
	if err = os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$REVIEW_COMMAND_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("REVIEW_COMMAND_LOG", log)
	if err = s.put("paused_until", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = runWorkerOperation([]string{"resume", "--local", "--config", config, "--state-dir", s.directory}); err != nil {
		t.Fatal(err)
	}
	commands, _ := os.ReadFile(log)
	if !strings.Contains(string(commands), "deploy-backup.service") {
		t.Fatal("resume cleared the pause but did not request a catch-up backup; no durable catch-up consumer exists")
	}
}

func TestBackupReviewPauseCatchupDurability(t *testing.T) {
	_, s, c, _ := healthyWorkerFixture(t)
	testutil.Must(t, setWorkerPause(s, time.Now().Add(-time.Minute)))
	failed := errors.New("manager unavailable")
	if err := consumeBackupCatchup(s, c, false, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	var request backupCatchup
	testutil.Must(t, s.get("backup_catchup", &request))
	if request.ID == "" {
		t.Fatal("lost catchup")
	}
	// Reopen as another host tick after a process exit, retrying the same request.
	reopened, err := openBackupState(s.directory, false)
	testutil.Must(t, err)
	defer reopened.Close()
	starts := 0
	testutil.Must(t, consumeBackupCatchup(reopened, c, false, func() error { starts++; return nil }))
	run := WorkerRun{ID: testutil.ID(t, "r"), ConfigHash: c.ConfigHash, StartedAt: time.Now().UTC()}
	id, err := admitWorkerRun(context.Background(), reopened, run)
	testutil.Must(t, err)
	if id != request.ID || starts != 1 {
		t.Fatal("retry changed request")
	}
	testutil.Must(t, finishWorkerRun(reopened, run, id))
	testutil.Must(t, reopened.get("backup_catchup", &request))
	if request.ID != id {
		t.Fatal("incomplete run cleared catchup")
	}
	run.Complete = true
	run.FinishedAt = time.Now().UTC()
	testutil.Must(t, finishWorkerRun(reopened, run, id))
	testutil.Must(t, reopened.get("backup_catchup", &request))
	if request.ID != "" {
		t.Fatal("complete run did not clear catchup")
	}
	testutil.Must(t, setWorkerPause(reopened, time.Now().Add(-time.Minute)))
	testutil.Must(t, reopened.put("clock_suspect", true))
	if err = consumeBackupCatchup(reopened, c, false, func() error { t.Fatal("started with suspect clock"); return nil }); err == nil {
		t.Fatal("uncertain clock resumed")
	}
	testutil.Must(t, reopened.put("clock_suspect", false))
	testutil.Must(t, consumeBackupCatchup(reopened, c, false, func() error { return nil }))
	testutil.Must(t, finishWorkerRun(reopened, run, id))
	testutil.Must(t, reopened.get("backup_catchup", &request))
	if request.ID == "" {
		t.Fatal("old run erased newer request")
	}
}
