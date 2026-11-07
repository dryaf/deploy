package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/sqlite"
)

type WorkerInstallation struct {
	Paths         *InstallationPaths          `json:"paths,omitempty"`
	Image         string                      `json:"image"`
	RelayImage    string                      `json:"relay_image,omitempty"`
	Schema        int                         `json:"schema"`
	WorkerID      string                      `json:"worker_id"`
	ArchiveID     string                      `json:"archive_id"`
	ConfigHash    string                      `json:"config_hash"`
	Policy        config.BackupWorkerConfig   `json:"policy"`
	Sources       []deploysource.WorkerSource `json:"sources"`
	Qualification map[string]bool             `json:"qualification"`
}

type WorkerRun struct {
	ID         string            `json:"id"`
	ConfigHash string            `json:"config_hash"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	Complete   bool              `json:"complete"`
	Captured   int               `json:"captured"`
	Failures   map[string]string `json:"failures"`
}

func loadWorkerInstallation(file string) (WorkerInstallation, error) {
	var c WorkerInstallation
	f, err := os.Open(file)
	if err != nil {
		return c, err
	}
	defer f.Close()
	if err = safefs.ReadJSON(f, &c, safefs.MaxRecordBytes); err != nil {
		return c, err
	}
	return c, validateWorkerInstallation(c)
}

func validateWorkerInstallation(c WorkerInstallation) error {
	if c.Schema != 1 || !safefs.ValidID(c.WorkerID, "w") || !safefs.ValidID(c.ArchiveID, "a") || !safefs.DigestPattern.MatchString(c.ConfigHash) {
		return fmt.Errorf("unsupported worker installation")
	}
	if c.Paths != nil {
		if err := c.Paths.Validate(); err != nil {
			return err
		}
		if _, err := c.Paths.UnitPaths(c.Policy.Storage.Path); err != nil {
			return err
		}
	}
	if _, err := config.NormalizeWorker(c.Policy); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, s := range c.Sources {
		if seen[s.Alias] || !config.ValidInventoryName(s.Alias) || !safefs.ValidID(s.SourceID, "s") || s.Host == "" || strings.HasPrefix(s.Host, "-") || strings.ContainsAny(s.Host, " \r\n\t") || s.User == "root" || !config.ValidInventoryName(s.User) || s.Port < 1 || s.Port > 65535 || !config.WorkerAbsolutePath(s.KeyFile) || !config.WorkerAbsolutePath(s.KnownHosts) {
			return fmt.Errorf("invalid restricted source configuration")
		}
		seen[s.Alias] = true
	}
	if len(seen) != len(c.Policy.Sources) {
		return fmt.Errorf("source inventory is incomplete")
	}
	for _, name := range c.Policy.Sources {
		if !seen[name] {
			return fmt.Errorf("source policy mismatch")
		}
	}
	return nil
}

func runBackupPass(ctx context.Context, c WorkerInstallation, state *backupState, archive *backupArchive) error {
	if err := requireBackupActivityReady(state); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, config.Budget(c.Policy.MaxRunTime, "40m"))
	defer cancel()
	stateDir, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer stateDir.Close()
	lease, err := stateDir.Lock("run.lock", true, true)
	if err != nil {
		return err
	}
	defer lease.Close()
	writer, err := archive.dir.Lock(".locks/writer.lock", true, false)
	if err != nil {
		return err
	}
	defer writer.Close()
	var mode string
	if err = state.get("mode", &mode); err != nil || mode != "writer" {
		return fmt.Errorf("worker is not in writer mode; explicit recovery reconciliation required")
	}
	var pausedUntil time.Time
	_ = state.get("paused_until", &pausedUntil)
	if time.Now().Before(pausedUntil) {
		return fmt.Errorf("worker is paused")
	}
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	if control.ReportingDisabled != (c.Policy.Notifications.Mode == "disabled") || control.SharedRequired != (c.Policy.Notifications.Mode == "shared") {
		return fmt.Errorf("reporting policy differs from retained archive control")
	}
	if err = archive.checkWriter(control); err != nil {
		return err
	}
	if control.ConfigHash != c.ConfigHash || control.WriterID != c.WorkerID || control.ArchiveID != c.ArchiveID {
		return fmt.Errorf("installed configuration/writer does not match control checkpoint")
	}
	if err = archive.commitControl(state); err != nil {
		return err
	}
	if err = archive.reconcileWork(state); err != nil {
		return err
	}
	var previous WorkerRun
	if state.get("run", &previous) == nil && previous.FinishedAt.IsZero() {
		if err = state.observeIncident("worker-crash", "worker", "previous run ended without a committed outcome", time.Now().UTC(), c.Policy.Notifications); err != nil {
			return err
		}
	}
	runID, err := safefs.NewID("r")
	if err != nil {
		return err
	}
	run := WorkerRun{ID: runID, ConfigHash: c.ConfigHash, StartedAt: time.Now().UTC(), Failures: map[string]string{}}
	catchupID, err := admitWorkerRun(ctx, state, run)
	if err != nil {
		return err
	}
	fail := func(key, class, message string) error {
		run.Failures[key] = message
		return state.observeIncident(class, key, message, time.Now().UTC(), c.Policy.Notifications)
	}
	type job struct {
		source   deploysource.WorkerSource
		instance ExpectedInstance
		attempts int
		due      time.Time
	}
	var jobs []job
	for _, source := range c.Sources {
		if control.RetiredSources[source.SourceID] != "" {
			continue
		}
		var found deploysource.SourceInventory
		if err = deploysource.QueryRestrictedSource(ctx, source, c.Policy.Timeouts, "inventory", &found); err != nil || found.Protocol != 1 || found.SourceID != source.SourceID || !found.Complete {
			if err = fail(source.SourceID, "discovery", "source inventory unavailable, incomplete or identity changed"); err != nil {
				return err
			}
			continue
		}
		seen := map[string]bool{}
		seenServices := map[string]bool{}
		for _, item := range found.Instances {
			m := item.Metadata
			key := instanceKey(source.SourceID, m.Service)
			if quadlet.ValidateMetadata(m) != nil || m.Role != "application" || m.Backup == nil || !m.Backup.Enabled || seenServices[m.Service] || protocol.MetadataBindingHash(m) != item.BindingHash {
				if err = fail(source.SourceID, "discovery", "ambiguous or malformed opted-in source metadata"); err != nil {
					return err
				}
				continue
			}
			seenServices[m.Service] = true
			seen[key] = true
			existing, known := control.Instances[key]
			if known && existing.Retired {
				continue
			}
			if !item.Eligible || (known && (existing.BindingHash != item.BindingHash || (existing.Generation != "unenrolled" && existing.Generation != item.Generation))) || (!known && item.Generation != "unenrolled" && !safefs.ValidID(item.Generation, "g")) {
				if err = fail(key, "identity", "source eligibility or database generation requires reconciliation"); err != nil {
					return err
				}
				continue
			}
			if !known {
				existing = ExpectedInstance{SourceID: source.SourceID, Alias: source.Alias, Service: m.Service, Generation: item.Generation, BindingHash: item.BindingHash}
				if err = archive.updateControl(state, func(next *ArchiveControl) error {
					if next.Instances == nil {
						next.Instances = map[string]ExpectedInstance{}
					}
					next.Instances[key] = existing
					return nil
				}); err != nil {
					return err
				}
				control.Instances[key] = existing
			}
			jobs = append(jobs, job{source: source, instance: existing})
		}
		for key, instance := range control.Instances {
			if instance.SourceID == source.SourceID && !instance.Retired && !seen[key] {
				if err = fail(key, "discovery", "previously enrolled instance is absent or no longer opted in; explicit retirement required"); err != nil {
					return err
				}
			}
		}
		if _, failed := run.Failures[source.SourceID]; !failed {
			if err = state.resolveIncident("discovery", source.SourceID, time.Now().UTC(), c.Policy.Notifications); err != nil {
				return err
			}
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].instance.LastSuccess.Equal(jobs[j].instance.LastSuccess) {
			return jobs[i].instance.LastSuccess.Before(jobs[j].instance.LastSuccess)
		}
		return instanceKey(jobs[i].source.SourceID, jobs[i].instance.Service) < instanceKey(jobs[j].source.SourceID, jobs[j].instance.Service)
	})
	retryOffsets := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	for len(jobs) > 0 && ctx.Err() == nil {
		next := 0
		for i := range jobs {
			if jobs[i].due.Before(jobs[next].due) {
				next = i
			}
		}
		current := jobs[next]
		jobs = append(jobs[:next], jobs[next+1:]...)
		if delay := time.Until(current.due); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				continue
			case <-timer.C:
			}
		}
		key := instanceKey(current.source.SourceID, current.instance.Service)
		nonce, err := safefs.NewID("n")
		if err != nil {
			return err
		}
		if err = archive.updateControl(state, func(control *ArchiveControl) error {
			instance := control.Instances[key]
			if instance.Retired || control.ConfigHash != c.ConfigHash {
				return fmt.Errorf("instance or configuration changed")
			}
			instance.CaptureSequence++
			current.instance = instance
			control.Instances[key] = instance
			return nil
		}); err != nil {
			return err
		}
		attempt := protocol.SnapshotAttempt{Protocol: 1, Operation: "snapshot", Nonce: nonce, RunID: runID, ConfigHash: c.ConfigHash, CaptureSequence: current.instance.CaptureSequence, SourceID: current.source.SourceID, Service: current.instance.Service, Generation: current.instance.Generation, BindingHash: current.instance.BindingHash, RequestedAt: time.Now().UTC()}
		receipt, captureErr := archive.receive(state, attempt, current.source.Alias, c.Policy.MaxSnapshotBytes, func(file *os.File) (protocol.SnapshotHeader, error) {
			return deploysource.FetchRestrictedSnapshot(ctx, current.source, c.Policy, attempt, file)
		}, func(file *os.File) (protocol.SnapshotValidation, error) {
			remaining := config.Budget(c.Policy.Timeouts.Verify, "5m")
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < remaining {
				remaining = time.Until(deadline)
			}
			if remaining <= 0 {
				return protocol.SnapshotValidation{}, ctx.Err()
			}
			return isolatedSnapshotValidation(file, false, remaining)
		})
		if captureErr != nil {
			message := "snapshot capture, validation or durable publication failed; inspect source ownership/profile, pending recovery, quotas, and archive capacity"
			var classified *sqlite.ValidationError
			if errors.As(captureErr, &classified) {
				message = "snapshot validation failed: " + classified.Kind
			}
			if err = fail(key, "capture", message); err != nil {
				return err
			}
			if current.attempts < len(retryOffsets) {
				current.due = time.Now().Add(retryOffsets[current.attempts])
				current.attempts++
				jobs = append(jobs, current)
			}
			continue
		}
		if err = archive.updateControl(state, func(control *ArchiveControl) error {
			instance := control.Instances[key]
			if instance.Retired || instance.CaptureSequence != attempt.CaptureSequence || instance.BindingHash != attempt.BindingHash {
				return fmt.Errorf("instance changed during capture")
			}
			instance.Generation = receipt.Header.Generation
			instance.LastSuccess = attempt.RequestedAt
			control.Instances[key] = instance
			return nil
		}); err != nil {
			return err
		}
		run.Captured++
		delete(run.Failures, key)
		for _, class := range []string{"capture", "identity", "discovery"} {
			if err = state.resolveIncident(class, key, time.Now().UTC(), c.Policy.Notifications); err != nil {
				return err
			}
		}
	}
	if ctx.Err() != nil {
		if err = fail("worker", "deadline", "run deadline exhausted; some instances may not have been captured"); err != nil {
			return err
		}
	}
	var finalControl ArchiveControl
	if err = state.get("control", &finalControl); err != nil {
		return err
	}
	for key, instance := range finalControl.Instances {
		if instance.Retired || finalControl.RetiredSources[instance.SourceID] != "" {
			continue
		}
		if instance.LastSuccess.Before(run.StartedAt) {
			if err = fail(key, "coverage", "this expected database has no successful capture in the current pass"); err != nil {
				return err
			}
		} else {
			if err = state.resolveIncident("coverage", key, time.Now().UTC(), c.Policy.Notifications); err != nil {
				return err
			}
		}
	}
	run.FinishedAt = time.Now().UTC()
	run.Complete = len(run.Failures) == 0 && run.Captured > 0
	if run.Captured == 0 && len(run.Failures) == 0 {
		if err = fail("worker", "coverage", "no active database snapshots completed"); err != nil {
			return err
		}
	}
	if err = finishWorkerRun(state, run, catchupID); err != nil {
		return err
	}
	if run.Complete {
		for _, class := range []string{"deadline", "coverage", "worker-crash"} {
			if err = state.resolveIncident(class, "worker", run.FinishedAt, c.Policy.Notifications); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("backup run incomplete: %d captures, %d failed scopes", run.Captured, len(run.Failures))
}
