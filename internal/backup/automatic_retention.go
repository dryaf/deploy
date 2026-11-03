package backup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/dryaf/deploy/internal/config"
)

// A capture authorizes at most one automatic pass. Checkpoint the claim before
// deleting anything: interruption or loss of local state cannot reuse that
// capture to expire more history later. Explicit operator pruning is separate.
type AutomaticRetentionPass struct {
	BackupID string    `json:"backup_id"`
	At       time.Time `json:"at"`
}

// Caller holds the archive writer lease, excluding new captures and maintenance.
func runAutomaticRetention(ctx context.Context, c WorkerInstallation, state *backupState, a *backupArchive, now time.Time) error {
	var run WorkerRun
	if err := state.get("run", &run); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	if run.ID == "" || run.ConfigHash != c.ConfigHash || run.FinishedAt.IsZero() || run.FinishedAt.After(now) {
		return nil
	}
	var control ArchiveControl
	if err := state.get("control", &control); err != nil {
		return err
	}
	if control.ConfigHash != c.ConfigHash {
		return fmt.Errorf("automatic retention configuration differs from archive control")
	}
	keys := make([]string, 0, len(control.Instances))
	for key := range control.Instances {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		instance := control.Instances[key]
		_, instanceFailed := run.Failures[key]
		_, sourceFailed := run.Failures[instance.SourceID]
		if instance.Retired || control.RetiredSources[instance.SourceID] != "" || instanceFailed || sourceFailed || instance.LastSuccess.IsZero() || instance.LastSuccess.Before(run.StartedAt) || instance.LastSuccess.After(run.FinishedAt) || now.Sub(instance.LastSuccess) > config.Budget(c.Policy.Freshness, "2h") {
			continue
		}
		receipt, err := state.latestCatalogReceipt(instance)
		if err != nil {
			return err
		}
		if err = validateReceipt(receipt, a.identity.ID); err != nil {
			return err
		}
		attempt := receipt.Header.Attempt
		if attempt.RunID != run.ID || attempt.ConfigHash != c.ConfigHash || attempt.CaptureSequence != instance.CaptureSequence || attempt.BindingHash != instance.BindingHash || !attempt.RequestedAt.Equal(instance.LastSuccess) {
			continue
		}
		if err = requireLatestSnapshot(control, receipt); err != nil {
			return err
		}
		token := latestSnapshotKey(receipt)
		previous := control.RetentionPasses[token]
		if previous.BackupID == receipt.BackupID || !previous.At.IsZero() && now.Sub(previous.At) < 24*time.Hour {
			continue
		}
		filter := ArchiveFilter{Source: instance.SourceID, Instance: instance.Service, Generation: instance.Generation}
		// Catalog success alone is insufficient if the newly published bundle
		// has since disappeared. Never substitute older surviving history.
		if _, err = a.selectReceipt("", true, filter); err != nil {
			return err
		}
		if err = a.updateControl(state, func(next *ArchiveControl) error {
			if next.RetentionPasses == nil {
				next.RetentionPasses = map[string]AutomaticRetentionPass{}
			}
			next.RetentionPasses[token] = AutomaticRetentionPass{BackupID: receipt.BackupID, At: now}
			return nil
		}); err != nil {
			return err
		}
		if _, err = a.prune(state, filter, true, now); err != nil {
			_ = state.observeIncident("retention", key, "automatic retention stopped; a new successful capture or explicit operator recovery is required", now, c.Policy.Notifications)
			return err
		}
		if err = state.resolveIncident("retention", key, now, c.Policy.Notifications); err != nil {
			return err
		}
	}
	return nil
}
