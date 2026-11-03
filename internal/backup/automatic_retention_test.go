//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
)

func automaticRetentionFixture(t *testing.T) (*backupArchive, *backupState, WorkerInstallation, SnapshotReceipt, SnapshotReceipt) {
	t.Helper()
	a, state, c, old := healthyWorkerFixture(t)
	data, err := os.ReadFile(filepath.Join(a.dir.File().Name(), receiptPath(old), "database.db"))
	testutil.Must(t, err)
	header := old.Header
	header.Attempt.Generation = header.Generation
	header.Attempt.CaptureSequence++
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.Attempt.RunID = testutil.ID(t, "r")
	header.Attempt.RequestedAt = time.Now().UTC()
	header.SnapshotID = testutil.ID(t, "b")
	header.StartedAt = header.Attempt.RequestedAt
	header.FinishedAt = header.StartedAt
	newest := publishSnapshotFixture(t, a, state, header, data)
	c.Policy.Retention = &config.RetentionConfig{KeepLast: 1}
	c.Policy.RestoreChecks = nil
	key := instanceKey(header.Attempt.SourceID, header.Attempt.Service)
	testutil.Must(t, a.updateControl(state, func(control *ArchiveControl) error {
		control.Retention = c.Policy.Retention
		instance := control.Instances[key]
		instance.LastSuccess = header.Attempt.RequestedAt
		control.Instances[key] = instance
		return nil
	}))
	testutil.Must(t, state.put("run", WorkerRun{ID: header.Attempt.RunID, ConfigHash: c.ConfigHash, StartedAt: header.Attempt.RequestedAt.Add(-time.Minute), FinishedAt: time.Now().UTC(), Captured: 1, Complete: true}))
	// Skip the already-completed integrity work and reserve a deterministic
	// maintenance window, independent of the wall clock/calendar boundary.
	testutil.Must(t, state.put("maintenance/full/"+key+"/"+header.Generation, maintenanceAttempt{At: time.Now().UTC(), Passed: true, BackupID: newest.BackupID}))
	testutil.Must(t, state.put("paused_until", time.Now().UTC().Add(2*time.Hour)))
	return a, state, c, old, newest
}

func requireBundle(t *testing.T, a *backupArchive, receipt SnapshotReceipt, present bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(a.dir.File().Name(), receiptPath(receipt)))
	if present && err != nil || !present && !os.IsNotExist(err) {
		t.Fatalf("bundle %s: want present=%v, stat=%v", receipt.BackupID, present, err)
	}
}

func TestBackupAutomaticRetentionNeedsCurrentSuccessfulCapture(t *testing.T) {
	for _, scenario := range []string{"failed-instance", "failed-source", "stale-capture", "incomplete-run", "previous-run", "changed-config", "changed-binding", "retired", "uncertain-newer-attempt"} {
		t.Run(scenario, func(t *testing.T) {
			a, state, c, old, newest := automaticRetentionFixture(t)
			key := instanceKey(newest.Header.Attempt.SourceID, newest.Header.Attempt.Service)
			var run WorkerRun
			testutil.Must(t, state.get("run", &run))
			run.Failures = map[string]string{}
			switch scenario {
			case "failed-instance":
				run.Failures[key] = "source unreachable"
			case "failed-source":
				run.Failures[newest.Header.Attempt.SourceID] = "discovery incomplete"
			case "incomplete-run":
				run.FinishedAt = time.Time{}
			case "previous-run":
				run.ID = testutil.ID(t, "r")
			case "changed-config":
				run.ConfigHash = "old config"
			case "stale-capture":
				// Keep all receipt/run evidence consistent; freshness alone must
				// prevent deletion when the worker returns after a long outage.
				run.StartedAt = run.StartedAt.Add(-48 * time.Hour)
				run.FinishedAt = run.FinishedAt.Add(-48 * time.Hour)
				for _, receipt := range []*SnapshotReceipt{&old, &newest} {
					receipt.ReceivedAt = receipt.ReceivedAt.Add(-48 * time.Hour)
					receipt.Header.Attempt.RequestedAt = receipt.Header.Attempt.RequestedAt.Add(-48 * time.Hour)
					receipt.Header.StartedAt = receipt.Header.StartedAt.Add(-48 * time.Hour)
					receipt.Header.FinishedAt = receipt.Header.FinishedAt.Add(-48 * time.Hour)
					testutil.Must(t, a.dir.WriteJSON(receiptPath(*receipt)+"/receipt.json", receipt, true))
					body, err := json.Marshal(receipt)
					testutil.Must(t, err)
					_, err = state.db.Exec("UPDATE bundles SET record=? WHERE id=?", string(body), receipt.BackupID)
					testutil.Must(t, err)
				}
			}
			testutil.Must(t, state.put("run", run))
			testutil.Must(t, a.updateControl(state, func(control *ArchiveControl) error {
				instance := control.Instances[key]
				switch scenario {
				case "stale-capture":
					instance.LastSuccess = newest.Header.Attempt.RequestedAt
				case "changed-binding":
					instance.BindingHash = "changed binding"
				case "retired":
					instance.Retired = true
				case "uncertain-newer-attempt":
					instance.CaptureSequence++
				}
				control.Instances[key] = instance
				return nil
			}))
			testutil.Must(t, runScheduledMaintenance(context.Background(), c, state, a))
			requireBundle(t, a, old, true)
			requireBundle(t, a, newest, true)
			control, err := a.checkpoint()
			testutil.Must(t, err)
			if len(control.RetentionPasses) != 0 {
				t.Fatal("ineligible capture authorized automatic retention")
			}
		})
	}
}

func TestBackupAutomaticRetentionIsScopedToSuccessfulInstance(t *testing.T) {
	a, state, c, old, newest := automaticRetentionFixture(t)
	data, err := os.ReadFile(filepath.Join(a.dir.File().Name(), receiptPath(newest), "database.db"))
	testutil.Must(t, err)
	header := newest.Header
	header.Attempt.SourceID = testutil.ID(t, "s")
	header.Attempt.CaptureSequence = 1
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.Attempt.RunID = testutil.ID(t, "r")
	header.SnapshotID = testutil.ID(t, "b")
	key := instanceKey(header.Attempt.SourceID, header.Attempt.Service)
	testutil.Must(t, a.updateControl(state, func(control *ArchiveControl) error {
		control.Instances[key] = ExpectedInstance{SourceID: header.Attempt.SourceID, Alias: "offline", Service: header.Attempt.Service, Generation: header.Generation, BindingHash: header.Attempt.BindingHash, LastSuccess: header.Attempt.RequestedAt}
		return nil
	}))
	offlineOld := publishSnapshotFixture(t, a, state, header, data)
	header.Attempt.CaptureSequence++
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.SnapshotID = testutil.ID(t, "b")
	offlineLatest := publishSnapshotFixture(t, a, state, header, data)
	testutil.Must(t, state.put("maintenance/full/"+key+"/"+header.Generation, maintenanceAttempt{At: time.Now().UTC(), Passed: true, BackupID: offlineLatest.BackupID}))
	var run WorkerRun
	testutil.Must(t, state.get("run", &run))
	run.Complete = false
	run.Failures = map[string]string{key: "source unreachable"}
	testutil.Must(t, state.put("run", run))
	testutil.Must(t, runScheduledMaintenance(context.Background(), c, state, a))
	requireBundle(t, a, old, false)
	requireBundle(t, a, newest, true)
	requireBundle(t, a, offlineOld, true)
	requireBundle(t, a, offlineLatest, true)
	// Explicit, scoped operator recovery remains possible during an outage.
	_, err = a.prune(state, ArchiveFilter{Source: header.Attempt.SourceID, Instance: header.Attempt.Service}, true, time.Now().UTC())
	testutil.Must(t, err)
	requireBundle(t, a, offlineOld, false)
	requireBundle(t, a, offlineLatest, true)
}

func TestBackupAutomaticRetentionConsumesCaptureBeforeDeletion(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "interrupted"}[interrupted], func(t *testing.T) {
			a, state, c, old, newest := automaticRetentionFixture(t)
			// Keep both until a later age boundary. Even if no deletion happens
			// now, that same capture must not authorize aging them out tomorrow.
			c.Policy.Retention.MaxAge = "1h"
			c.Policy.Freshness = "72h"
			testutil.Must(t, a.updateControl(state, func(control *ArchiveControl) error { control.Retention = c.Policy.Retention; return nil }))
			if interrupted {
				// Force pruning to stop after the durable claim, before deletion.
				c.Policy.Retention.MaxAge = ""
				testutil.Must(t, a.updateControl(state, func(control *ArchiveControl) error { control.Retention = c.Policy.Retention; return nil }))
				lease, err := a.dir.Lock(".locks/"+old.BackupID+".lock", true, true)
				testutil.Must(t, err)
				err = runAutomaticRetention(context.Background(), c, state, a, time.Now().UTC())
				lease.Close()
				if err == nil {
					t.Fatal("prune ignored the live bundle lease")
				}
			} else {
				testutil.Must(t, runScheduledMaintenance(context.Background(), c, state, a))
			}
			control, err := a.checkpoint()
			testutil.Must(t, err)
			if control.RetentionPasses[latestSnapshotKey(newest)].BackupID != newest.BackupID {
				t.Fatal("automatic retention authorization was not checkpointed")
			}
			// Rebuild all local evidence used by retention from the archive,
			// with the old successful run restored independently.
			var run WorkerRun
			testutil.Must(t, state.get("run", &run))
			rebuilt, err := openBackupState(filepath.Join(t.TempDir(), "state"), true)
			testutil.Must(t, err)
			defer rebuilt.Close()
			testutil.Must(t, rebuilt.put("control", control))
			testutil.Must(t, rebuilt.put("mode", "writer"))
			testutil.Must(t, rebuilt.put("run", run))
			for _, receipt := range []SnapshotReceipt{old, newest} {
				body, err := json.Marshal(receipt)
				testutil.Must(t, err)
				_, err = rebuilt.db.Exec("INSERT INTO bundles(id,path,record) VALUES(?,?,?)", receipt.BackupID, receiptPath(receipt), string(body))
				testutil.Must(t, err)
			}
			testutil.Must(t, runAutomaticRetention(context.Background(), c, rebuilt, a, time.Now().UTC().Add(25*time.Hour)))
			requireBundle(t, a, old, true)
			requireBundle(t, a, newest, true)
		})
	}
}

func TestBackupAutomaticRetentionResumesAfterNewCapture(t *testing.T) {
	a, state, c, old, newest := automaticRetentionFixture(t)
	testutil.Must(t, runScheduledMaintenance(context.Background(), c, state, a))
	requireBundle(t, a, old, false)
	data, err := os.ReadFile(filepath.Join(a.dir.File().Name(), receiptPath(newest), "database.db"))
	testutil.Must(t, err)
	header := newest.Header
	header.Attempt.CaptureSequence++
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.Attempt.RunID = testutil.ID(t, "r")
	header.Attempt.RequestedAt = time.Now().UTC()
	header.SnapshotID = testutil.ID(t, "b")
	header.StartedAt = header.Attempt.RequestedAt
	header.FinishedAt = header.StartedAt
	latest := publishSnapshotFixture(t, a, state, header, data)
	testutil.Must(t, a.updateControl(state, func(control *ArchiveControl) error {
		key := instanceKey(header.Attempt.SourceID, header.Attempt.Service)
		instance := control.Instances[key]
		instance.LastSuccess = header.Attempt.RequestedAt
		control.Instances[key] = instance
		return nil
	}))
	testutil.Must(t, state.put("run", WorkerRun{ID: header.Attempt.RunID, ConfigHash: c.ConfigHash, StartedAt: header.Attempt.RequestedAt.Add(-time.Minute), FinishedAt: time.Now().UTC(), Complete: true, Captured: 1}))
	testutil.Must(t, runScheduledMaintenance(context.Background(), c, state, a))
	requireBundle(t, a, newest, true) // the daily per-instance limit still applies
	testutil.Must(t, a.updateControl(state, func(control *ArchiveControl) error {
		pass := control.RetentionPasses[latestSnapshotKey(latest)]
		pass.At = pass.At.Add(-25 * time.Hour)
		control.RetentionPasses[latestSnapshotKey(latest)] = pass
		return nil
	}))
	testutil.Must(t, runScheduledMaintenance(context.Background(), c, state, a))
	requireBundle(t, a, newest, false)
	requireBundle(t, a, latest, true)
}

func TestBackupAutomaticRetentionRefusesMissingNewest(t *testing.T) {
	a, state, c, old, newest := automaticRetentionFixture(t)
	testutil.Must(t, os.RemoveAll(filepath.Join(a.dir.File().Name(), receiptPath(newest))))
	if err := runScheduledMaintenance(context.Background(), c, state, a); err == nil {
		t.Fatal("automatic retention accepted missing latest publication")
	}
	requireBundle(t, a, old, true)
}
