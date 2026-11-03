package backup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/sqlite"
	"golang.org/x/sys/unix"
)

type ArchiveVerification struct {
	ControlRevision     uint64                      `json:"control_revision,omitempty"`
	StartedAt           time.Time                   `json:"started_at"`
	FinishedAt          time.Time                   `json:"finished_at"`
	ErrorKind           string                      `json:"error_kind,omitempty"`
	Schema              int                         `json:"schema"`
	ID                  string                      `json:"id"`
	ArchiveID           string                      `json:"archive_id"`
	BackupID            string                      `json:"backup_id"`
	SHA256              string                      `json:"sha256"`
	Validation          protocol.SnapshotValidation `json:"validation"`
	NetworkIsolated     bool                        `json:"network_isolated"`
	ApplicationRestored bool                        `json:"application_restored"`
}

func (a *backupArchive) verify(state *backupState, receipt SnapshotReceipt, full bool) (record ArchiveVerification, resultErr error) {
	var dir *safefs.Directory
	var lease *safefs.Lock
	// Failure persistence below must run before the bundle lease is released.
	defer func() {
		if dir != nil {
			dir.Close()
		}
		if lease != nil {
			lease.Close()
		}
	}()
	id, err := safefs.NewID("n")
	if err != nil {
		return record, err
	}
	record = ArchiveVerification{Schema: 1, ID: id, ArchiveID: a.identity.ID, BackupID: receipt.BackupID, SHA256: receipt.Header.SHA256, StartedAt: time.Now().UTC()}
	stored := false
	defer func() {
		if !stored && resultErr != nil {
			record.FinishedAt = time.Now().UTC()
			record.ErrorKind = "execution"
			var classified *sqlite.ValidationError
			if errors.As(resultErr, &classified) {
				record.ErrorKind = classified.Kind
			}
			if strings.Contains(resultErr.Error(), "checksum") {
				record.ErrorKind = "checksum"
			}
			// A busy/unwritable archive must not keep a known-bad snapshot green
			// in the notifier's separate state volume while checkpointing fails.
			var current ArchiveControl
			if state.get("control", &current) == nil {
				record.ControlRevision = current.Revision + 1
			}
			if e := state.rememberVerification(record); e != nil {
				resultErr = fmt.Errorf("%w; local verification status failed: %v", resultErr, e)
			}
			if e := a.updateControl(state, func(control *ArchiveControl) error {
				return a.writeVerification(state, control, &record)
			}); e != nil {
				resultErr = fmt.Errorf("%w; verification result persistence failed: %v", resultErr, e)
			}
		}
	}()

	// Readers share the bundle lease; verification changes its current evidence
	// and therefore excludes downloads until that evidence has been persisted.
	dir, lease, err = a.openBundleLease(receipt, true)
	if err != nil {
		return record, err
	}
	file, err := dir.Open("database.db", unix.O_RDONLY, 0)
	if err != nil {
		return record, err
	}
	defer file.Close()
	digest, err := safefs.DigestFile(file)
	if err != nil || digest != receipt.Header.SHA256 {
		return record, fmt.Errorf("archive checksum failed; snapshot cannot be promoted")
	}
	validation, err := isolatedSnapshotValidation(file, full, 5*time.Minute)
	if err != nil {
		return record, err
	}
	record.SHA256 = digest
	record.Validation = validation
	record.NetworkIsolated = true
	record.FinishedAt = time.Now().UTC()
	// Pin/control updates serialize against deletion and are acknowledged only
	// after their recovery checkpoint reaches the archive.
	err = a.updateControl(state, func(control *ArchiveControl) error {
		if err := a.writeVerification(state, control, &record); err != nil {
			return err
		}
		stored = true
		if full {
			return advanceRecoveryPin(state, control, receipt, "full-integrity/"+instanceKey(receipt.Header.Attempt.SourceID, receipt.Header.Attempt.Service)+"/"+receipt.Header.Generation)
		}
		return nil
	})
	return record, err
}

type RetentionCandidate struct {
	Receipt SnapshotReceipt `json:"receipt"`
	Delete  bool            `json:"eligible"`
	Reason  string          `json:"reason"`
	Deleted bool            `json:"deleted"`
}

type RetentionPreview struct {
	Schema           int                  `json:"schema"`
	ArchiveID        string               `json:"archive_id"`
	ControlRevision  uint64               `json:"control_revision"`
	ReclaimableBytes int64                `json:"reclaimable_bytes"`
	Candidates       []RetentionCandidate `json:"candidates"`
}

func retentionPreview(control ArchiveControl, receipts []SnapshotReceipt, filter ArchiveFilter, now time.Time) (RetentionPreview, error) {
	result := RetentionPreview{Schema: 1, ArchiveID: control.ArchiveID, ControlRevision: control.Revision, Candidates: []RetentionCandidate{}}
	groups := map[string][]SnapshotReceipt{}
	sourceIDs := map[string]bool{}
	for _, r := range receipts {
		if filter.Source == "" || r.SourceAlias == filter.Source || r.Header.Attempt.SourceID == filter.Source {
			sourceIDs[r.Header.Attempt.SourceID] = true
		}
	}
	for _, instance := range control.Instances {
		if filter.Source == instance.Alias || filter.Source == instance.SourceID {
			sourceIDs[instance.SourceID] = true
		}
	}
	if filter.Source != "" && len(sourceIDs) > 1 {
		return result, fmt.Errorf("ambiguous source alias; select its UUID")
	}
	for _, r := range receipts {
		if filter.Source != "" && !sourceIDs[r.Header.Attempt.SourceID] || filter.Instance != "" && filter.Instance != r.Header.Attempt.Service || filter.Generation != "" && filter.Generation != r.Header.Generation {
			continue
		}
		key := instanceKey(r.Header.Attempt.SourceID, r.Header.Attempt.Service) + "/" + r.Header.Generation
		groups[key] = append(groups[key], r)
	}
	for _, receipts := range groups {
		sort.Slice(receipts, func(i, j int) bool {
			if receipts[i].WriterEpoch != receipts[j].WriterEpoch {
				return receipts[i].WriterEpoch > receipts[j].WriterEpoch
			}
			if receipts[i].Header.Attempt.CaptureSequence != receipts[j].Header.Attempt.CaptureSequence {
				return receipts[i].Header.Attempt.CaptureSequence > receipts[j].Header.Attempt.CaptureSequence
			}
			return receipts[i].BackupID < receipts[j].BackupID
		})
		for i, r := range receipts {
			row := RetentionCandidate{Receipt: r}
			instance, known := control.Instances[instanceKey(r.Header.Attempt.SourceID, r.Header.Attempt.Service)]
			switch {
			case control.Retention == nil:
				row.Reason = "automatic deletion disabled: no retention policy"
			case control.Retention.KeepLast <= 0:
				return result, fmt.Errorf("invalid explicit retention policy")
			case !known:
				row.Reason = "instance policy is unknown"
			case instance.Retired || control.RetiredSources[instance.SourceID] != "":
				row.Reason = "retired history requires explicit reconciliation"
			case instance.Generation != r.Header.Generation:
				row.Reason = "historical generation requires explicit reconciliation"
			case control.Pins[r.BackupID] != "":
				row.Reason = "pinned: " + control.Pins[r.BackupID]
			case i < control.Retention.KeepLast:
				row.Reason = "protected by newest-count minimum"
			case r.ReceivedAt.After(now):
				row.Reason = "future observation requires clock reconciliation"
			default:
				if control.Retention.MaxAge != "" {
					age, err := config.PositiveWorkerDuration(control.Retention.MaxAge, 10*365*24*time.Hour)
					if err != nil {
						return result, err
					}
					if now.Sub(r.ReceivedAt) <= age {
						row.Reason = "protected by maximum-age window"
					}
				}
				if row.Reason == "" {
					row.Delete = true
					row.Reason = "outside count and age protection"
					result.ReclaimableBytes += r.Header.Size
				}
			}
			result.Candidates = append(result.Candidates, row)
		}
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		return result.Candidates[i].Receipt.BackupID < result.Candidates[j].Receipt.BackupID
	})
	return result, nil
}

type archiveDeletion struct {
	Schema          int             `json:"schema"`
	ArchiveID       string          `json:"archive_id"`
	ControlRevision uint64          `json:"control_revision"`
	Receipt         SnapshotReceipt `json:"receipt"`
	RequestedAt     time.Time       `json:"requested_at"`
}

func (a *backupArchive) prune(state *backupState, filter ArchiveFilter, apply bool, now time.Time) (RetentionPreview, error) {
	if apply {
		if err := state.retentionClockSafe(); err != nil {
			return RetentionPreview{}, err
		}
	}
	var control ArchiveControl
	if err := state.get("control", &control); err != nil {
		return RetentionPreview{}, err
	}
	if err := a.checkWriter(control); err != nil {
		return RetentionPreview{}, err
	}
	checkpoint, err := a.checkpoint()
	if err != nil || !reflect.DeepEqual(control, checkpoint) {
		return RetentionPreview{}, fmt.Errorf("retention requires matching complete control/checkpoint evidence")
	}
	listing, err := a.scan(100000)
	if err != nil {
		return RetentionPreview{}, err
	}
	if !listing.Complete || listing.Incoming > 0 || listing.Trash > 0 {
		return RetentionPreview{}, fmt.Errorf("retention blocked by incomplete or unreconciled archive state")
	}
	preview, err := retentionPreview(control, listing.Entries, filter, now)
	if err != nil || !apply {
		return preview, err
	}
	if err = retentionVerificationSafe(state, preview, listing.Verification); err != nil {
		return preview, err
	}
	// Read every relevant retained file before deleting any history. Corruption
	// stops this scope instead of leaving an unverified replacement recovery point.
	for _, row := range preview.Candidates {
		directory, lease, err := a.openBundle(row.Receipt)
		if err != nil {
			return preview, err
		}
		file, err := directory.Open("database.db", unix.O_RDONLY, 0)
		if err == nil {
			var digest string
			digest, err = safefs.DigestFile(file)
			file.Close()
			if err == nil && digest != row.Receipt.Header.SHA256 {
				err = fmt.Errorf("checksum failure blocks retention")
			}
		}
		directory.Close()
		lease.Close()
		if err != nil {
			return preview, err
		}
	}
	for i := range preview.Candidates {
		row := &preview.Candidates[i]
		if !row.Delete {
			continue
		}
		if err = a.deleteRetainedBundle(state, row.Receipt, filter, now); err != nil {
			return preview, err
		}
		row.Deleted = true
	}
	return preview, nil
}

func (a *backupArchive) deleteRetainedBundle(state *backupState, receipt SnapshotReceipt, filter ArchiveFilter, now time.Time) error {
	index, err := a.dir.Lock(".locks/index.lock", true, false)
	if err != nil {
		return err
	}
	lease, err := a.dir.Lock(".locks/"+receipt.BackupID+".lock", true, false)
	index.Close()
	if err != nil {
		return err
	}
	defer lease.Close()
	// Listing uses index only briefly; no data-file I/O occurs under control.
	listing, err := a.scan(100000)
	if err != nil || !listing.Complete {
		return fmt.Errorf("archive changed before retention decision")
	}
	controlLock, err := a.dir.Lock(".locks/control.lock", true, false)
	if err != nil {
		return err
	}
	defer controlLock.Close()
	if err = a.commitControlLocked(state); err != nil {
		return err
	}
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	current, err := retentionPreview(control, listing.Entries, filter, now)
	if err != nil {
		return err
	}
	eligible := false
	for _, row := range current.Candidates {
		if row.Receipt.BackupID == receipt.BackupID && reflect.DeepEqual(row.Receipt, receipt) {
			eligible = row.Delete
		}
	}
	if !eligible {
		return fmt.Errorf("backup is no longer eligible for retention")
	}
	// Recheck before each irreversible mutation, including after lengthy hashing.
	if err = state.retentionClockSafe(); err != nil {
		return err
	}
	if err = retentionVerificationSafe(state, current, listing.Verification); err != nil {
		return err
	}
	directory, err := a.dir.Sub(receiptPath(receipt))
	if err != nil {
		return err
	}
	defer directory.Close()
	var actual SnapshotReceipt
	if err = directory.ReadJSON("receipt.json", &actual); err != nil || !reflect.DeepEqual(actual, receipt) {
		return fmt.Errorf("bundle changed before deletion")
	}
	entries, err := directory.File().ReadDir(3)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) != 2 {
		return fmt.Errorf("foreign files prevent managed deletion")
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || (entry.Name() != "receipt.json" && entry.Name() != "database.db") {
			return fmt.Errorf("unsafe bundle contents prevent deletion")
		}
	}
	if err = a.dir.Mkdir("control/deletions"); err != nil {
		return err
	}
	intent := archiveDeletion{1, a.identity.ID, control.Revision, receipt, now}
	if err = a.dir.WriteJSON("control/deletions/"+receipt.BackupID+".json", intent, false); err != nil {
		var prior archiveDeletion
		if e := a.dir.ReadJSON("control/deletions/"+receipt.BackupID+".json", &prior); e != nil || prior.Schema != 1 || prior.ArchiveID != a.identity.ID || !reflect.DeepEqual(prior.Receipt, receipt) {
			return fmt.Errorf("conflicting deletion intent: %w", err)
		}
	}
	parent, err := a.dir.Sub(filepath.Dir(receiptPath(receipt)))
	if err != nil {
		return err
	}
	defer parent.Close()
	trash, err := a.dir.Sub(".trash")
	if err != nil {
		return err
	}
	defer trash.Close()
	index, err = a.dir.Lock(".locks/index.lock", true, false)
	if err != nil {
		return err
	}
	err = parent.Rename(receipt.BackupID, trash, receipt.BackupID)
	index.Close()
	if err != nil {
		return err
	}
	if _, err = state.db.Exec("DELETE FROM bundles WHERE id=?", receipt.BackupID); err != nil {
		return err
	}
	controlLock.Close()
	if err = unix.Unlinkat(int(directory.File().Fd()), "database.db", 0); err != nil {
		return err
	}
	if err = unix.Unlinkat(int(directory.File().Fd()), "receipt.json", 0); err != nil {
		return err
	}
	if err = directory.File().Sync(); err != nil {
		return err
	}
	if err = unix.Unlinkat(int(trash.File().Fd()), receipt.BackupID, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return trash.File().Sync()
}

// Published receipts are authoritative history. Re-indexing never grants fresh
// success to an abandoned run or silently creates missing source generations.
func (a *backupArchive) reconcilePublished(state *backupState) error {
	listing, err := a.scan(100000)
	if err != nil {
		return err
	}
	if !listing.Complete {
		return fmt.Errorf("incomplete archive blocks writer reconciliation")
	}
	for _, receipt := range listing.Entries {
		var existing string
		err = state.db.QueryRow("SELECT record FROM bundles WHERE id=?", receipt.BackupID).Scan(&existing)
		if err == nil {
			var indexed SnapshotReceipt
			if safefs.DecodeJSON([]byte(existing), &indexed, safefs.MaxRecordBytes) != nil || !reflect.DeepEqual(indexed, receipt) {
				return fmt.Errorf("catalog/receipt identity conflict")
			}
		}
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == sql.ErrNoRows {
			// Unknown bundles are indexed only after a current checksum and sandbox check.
			directory, lease, err := a.openBundle(receipt)
			if err != nil {
				return err
			}
			file, err := directory.Open("database.db", unix.O_RDONLY, 0)
			if err == nil {
				var digest string
				digest, err = safefs.DigestFile(file)
				if err == nil && digest != receipt.Header.SHA256 {
					err = fmt.Errorf("orphan bundle checksum mismatch")
				}
				if err == nil {
					_, err = isolatedSnapshotValidation(file, false, 5*time.Minute)
				}
				file.Close()
			}
			directory.Close()
			lease.Close()
			if err != nil {
				return err
			}
			body, _ := json.Marshal(receipt)
			if _, err = state.db.Exec("INSERT INTO bundles(id,path,record) VALUES(?,?,?)", receipt.BackupID, receiptPath(receipt), string(body)); err != nil {
				return err
			}
		}
		var before ArchiveControl
		if err = state.get("control", &before); err != nil {
			return err
		}
		expected, known := before.Instances[instanceKey(receipt.Header.Attempt.SourceID, receipt.Header.Attempt.Service)]
		if !known || expected.Retired || expected.Generation != "unenrolled" || expected.BindingHash != receipt.Header.Attempt.BindingHash {
			continue
		}
		if err = a.updateControl(state, func(control *ArchiveControl) error {
			key := instanceKey(receipt.Header.Attempt.SourceID, receipt.Header.Attempt.Service)
			instance, known := control.Instances[key]
			if known && !instance.Retired && instance.Generation == "unenrolled" && instance.BindingHash == receipt.Header.Attempt.BindingHash {
				instance.Generation = receipt.Header.Generation
				control.Instances[key] = instance
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err = a.reconcileLatestSnapshots(state); err != nil {
		return err
	}
	return a.reconcileBundleAvailability(state, listing)
}
