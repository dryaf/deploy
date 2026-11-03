package backup

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type ArchiveIdentity struct {
	Schema       int    `json:"schema"`
	ID           string `json:"id"`
	FilesystemID string `json:"filesystem_id"`
	WriterID     string `json:"writer_id"`
	Epoch        uint64 `json:"epoch"`
	Enrolled     bool   `json:"enrolled"`
}

type SnapshotReceipt struct {
	Schema              int                         `json:"schema"`
	PublicationSequence uint64                      `json:"publication_sequence"`
	ArchiveID           string                      `json:"archive_id"`
	BackupID            string                      `json:"backup_id"`
	WriterID            string                      `json:"writer_id"`
	WriterEpoch         uint64                      `json:"writer_epoch"`
	SourceAlias         string                      `json:"source_alias"`
	Header              protocol.SnapshotHeader     `json:"header"`
	ReceivedAt          time.Time                   `json:"received_at"`
	Validation          protocol.SnapshotValidation `json:"validation"`
}

type archiveCheckpoint struct {
	Control ArchiveControl `json:"control"`
	SHA256  string         `json:"sha256"`
}

type backupArchive struct {
	dir         *safefs.Directory
	identity    ArchiveIdentity
	maintenance *safefs.Lock
}

func enrollBackupArchive(directory, archiveID, writerID string) (*backupArchive, error) {
	if !safefs.ValidID(archiveID, "a") || !safefs.ValidID(writerID, "w") {
		return nil, fmt.Errorf("invalid archive/writer identity")
	}
	dir, err := safefs.Open(directory)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if info, e := dir.File().Stat(); e != nil || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("archive directory must not be writable by other users/groups")
	}
	entries, err := dir.File().ReadDir(1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("new archive enrollment requires an empty explicitly selected directory")
	}
	filesystem, err := dir.FilesystemID()
	if err != nil {
		return nil, err
	}
	identity := ArchiveIdentity{1, archiveID, filesystem, writerID, 1, false}
	if err = dir.WriteJSON("archive.json", identity, false); err != nil {
		return nil, err
	}
	for _, p := range []string{".locks", ".incoming", ".trash", "sources", "control/verification"} {
		if err = dir.Mkdir(p); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"maintenance", "index", "control", "writer"} {
		lock, err := dir.Lock(".locks/"+name+".lock", true, true)
		if err != nil {
			return nil, err
		}
		lock.Close()
	}
	identity.Enrolled = true
	if err = dir.WriteJSON("archive.json", identity, true); err != nil {
		return nil, err
	}
	return openBackupArchive(directory)
}

func openBackupArchive(directory string) (*backupArchive, error) {
	dir, err := safefs.Open(directory)
	if err != nil {
		return nil, err
	}
	if info, e := dir.File().Stat(); e != nil || info.Mode().Perm()&0022 != 0 {
		dir.Close()
		return nil, fmt.Errorf("unsafe archive directory permissions")
	}
	good := false
	defer func() {
		if !good {
			dir.Close()
		}
	}()
	var identity ArchiveIdentity
	if err = dir.ReadJSON("archive.json", &identity); err != nil {
		return nil, fmt.Errorf("archive unavailable: %w", err)
	}
	if identity.Schema != 1 || !identity.Enrolled || !safefs.ValidID(identity.ID, "a") || !safefs.ValidID(identity.WriterID, "w") || identity.Epoch == 0 {
		return nil, fmt.Errorf("unsupported or incomplete archive enrollment")
	}
	if _, err = dir.FilesystemID(); err != nil {
		return nil, err
	}
	// Readers may recover a copied archive. Writer admission still requires the enrolled filesystem identity.
	maintenance, err := dir.Lock(".locks/maintenance.lock", false, false)
	if err != nil {
		return nil, err
	}
	good = true
	return &backupArchive{dir, identity, maintenance}, nil
}

func (a *backupArchive) Close() error {
	if a.maintenance != nil {
		a.maintenance.Close()
	}
	return a.dir.Close()
}

func (a *backupArchive) checkWriter(control ArchiveControl) error {
	if control.Schema != 1 || control.ArchiveID != a.identity.ID || control.WriterID != a.identity.WriterID || control.Epoch != a.identity.Epoch || control.Revision == 0 {
		return fmt.Errorf("writer is not enrolled for this archive/epoch")
	}
	var current ArchiveIdentity
	if err := a.dir.ReadJSON("archive.json", &current); err != nil {
		return err
	}
	if current != a.identity {
		return fmt.Errorf("archive writer epoch changed; operation fenced")
	}
	filesystem, err := a.dir.FilesystemID()
	if err != nil || filesystem != a.identity.FilesystemID {
		return fmt.Errorf("archive filesystem changed")
	}
	return nil
}

func (a *backupArchive) checkpoint() (ArchiveControl, error) {
	var record archiveCheckpoint
	if err := a.dir.ReadJSON("control/recovery.json", &record); err != nil {
		return record.Control, err
	}
	body, _ := json.Marshal(record.Control)
	if fmt.Sprintf("%x", sha256.Sum256(body)) != record.SHA256 {
		return record.Control, fmt.Errorf("archive recovery checkpoint checksum mismatch")
	}
	if record.Control.Schema != 1 || record.Control.Revision == 0 || !safefs.DigestPattern.MatchString(record.Control.ConfigHash) || record.Control.Instances == nil || record.Control.RetiredSources == nil || record.Control.Pins == nil {
		return record.Control, fmt.Errorf("unsupported or incomplete archive control schema")
	}
	if record.Control.ArchiveID != a.identity.ID || record.Control.Epoch != a.identity.Epoch || record.Control.WriterID != a.identity.WriterID {
		return record.Control, fmt.Errorf("archive recovery checkpoint identity mismatch")
	}
	return record.Control, nil
}

func (a *backupArchive) commitControlLocked(s *backupState) error {
	var mode string
	if err := s.get("mode", &mode); err != nil || mode != "writer" {
		return fmt.Errorf("archive mutation requires enrolled writer mode")
	}
	var control ArchiveControl
	if err := s.get("control", &control); err != nil {
		return err
	}
	if err := a.checkWriter(control); err != nil {
		return err
	}
	previous, err := a.checkpoint()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if previous.Revision > control.Revision {
			return fmt.Errorf("local control is stale; reconcile newer archive evidence")
		}
		if previous.Revision == control.Revision && !reflect.DeepEqual(previous, control) {
			return fmt.Errorf("different control payloads claim the same revision")
		}
	}
	body, _ := json.Marshal(control)
	record := archiveCheckpoint{control, fmt.Sprintf("%x", sha256.Sum256(body))}
	if err = a.dir.WriteJSON("control/recovery.json", record, err == nil); err != nil {
		return err
	}
	// Acknowledgment comes after archive data and its directory entry are durable.
	return s.put("control_ack", control.Revision)
}

func (a *backupArchive) commitControl(s *backupState) error {
	lock, err := a.dir.Lock(".locks/control.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	return a.commitControlLocked(s)
}

func (a *backupArchive) updateControl(s *backupState, change func(*ArchiveControl) error) error {
	lock, err := a.dir.Lock(".locks/control.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	var control ArchiveControl
	if err = s.get("control", &control); err != nil {
		return err
	}
	if err = a.checkWriter(control); err != nil {
		return err
	}
	// Repair a pending acknowledgment before accepting another policy mutation.
	if err = a.commitControlLocked(s); err != nil {
		return err
	}
	if err = change(&control); err != nil {
		return err
	}
	control.Revision++
	if err = s.put("control", control); err != nil {
		return err
	}
	return a.commitControlLocked(s)
}

func receiptPath(r SnapshotReceipt) string {
	return filepath.Join("sources", r.Header.Attempt.SourceID, safefs.LockName(r.Header.Attempt.Service), r.Header.Generation, r.BackupID)
}

func validateReceipt(r SnapshotReceipt, archiveID string) error {
	if r.Schema != 1 || r.PublicationSequence == 0 || r.ArchiveID != archiveID || !safefs.ValidID(r.BackupID, "b") || !safefs.ValidID(r.WriterID, "w") || r.WriterEpoch == 0 || r.ReceivedAt.IsZero() || r.Validation.Result != "ok" || r.Validation.Check != "quick_check" || r.Validation.CheckedAt.IsZero() {
		return fmt.Errorf("invalid snapshot receipt")
	}
	return protocol.ValidateSnapshotHeader(r.Header, r.Header.Attempt, 1<<40)
}

// fetch must return only after a complete framed transfer AND successful remote
// exit. Validators run with no source credentials and no writable archive mount.
func (a *backupArchive) receive(s *backupState, attempt protocol.SnapshotAttempt, alias string, limit int64, fetch func(*os.File) (protocol.SnapshotHeader, error), validate func(*os.File) (protocol.SnapshotValidation, error)) (SnapshotReceipt, error) {
	var receipt SnapshotReceipt
	if err := attempt.Validate(); err != nil {
		return receipt, err
	}
	var control ArchiveControl
	if err := s.get("control", &control); err != nil {
		return receipt, err
	}
	if err := a.checkWriter(control); err != nil {
		return receipt, err
	}
	reserve := int64(1 << 30)
	var configured int64
	if s.get("min_free_bytes", &configured) == nil && configured > 0 {
		reserve = configured
	}
	if err := a.dir.Capacity(limit + reserve); err != nil {
		return receipt, err
	}
	id, err := safefs.NewID("b")
	if err != nil {
		return receipt, err
	}
	receipt = SnapshotReceipt{Schema: 1, ArchiveID: a.identity.ID, BackupID: id, WriterID: control.WriterID, WriterEpoch: control.Epoch, SourceAlias: alias}
	key := instanceKey(attempt.SourceID, attempt.Service)
	if err = s.recordAttempt(id, key, "receiving", attempt); err != nil {
		return receipt, err
	}
	bundleLock, err := a.dir.Lock(".locks/"+id+".lock", true, true)
	if err != nil {
		return receipt, err
	}
	defer bundleLock.Close()
	incoming, err := a.dir.Sub(".incoming")
	if err != nil {
		return receipt, err
	}
	defer incoming.Close()
	if err = unix.Mkdirat(int(incoming.File().Fd()), id, 0700); err != nil {
		return receipt, err
	}
	if err = incoming.File().Sync(); err != nil {
		return receipt, err
	}
	staging, err := incoming.Sub(id)
	if err != nil {
		return receipt, err
	}
	defer staging.Close()
	publicationAttempted := false
	defer func() {
		if !publicationAttempted {
			_ = s.recordAttempt(id, key, "failed", attempt)
			_ = unix.Unlinkat(int(staging.File().Fd()), "database.db", 0)
			_ = unix.Unlinkat(int(staging.File().Fd()), "receipt.json", 0)
			_ = staging.File().Sync()
			_ = unix.Unlinkat(int(incoming.File().Fd()), id, unix.AT_REMOVEDIR)
			_ = incoming.File().Sync()
		}
	}()
	file, err := staging.Open("database.db", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return receipt, err
	}
	defer file.Close()
	receipt.Header, err = fetch(file)
	if err != nil {
		return receipt, err
	}
	if err = protocol.ValidateSnapshotHeader(receipt.Header, attempt, limit); err != nil {
		return receipt, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != receipt.Header.Size {
		return receipt, fmt.Errorf("received snapshot size mismatch")
	}
	digest, err := safefs.DigestFile(file)
	if err != nil || digest != receipt.Header.SHA256 {
		return receipt, fmt.Errorf("received snapshot checksum mismatch")
	}
	receipt.Validation, err = validate(file)
	if err != nil {
		return receipt, err
	}
	receipt.ReceivedAt = time.Now().UTC()
	if err = file.Sync(); err != nil {
		return receipt, err
	}
	parentPath := filepath.Dir(receiptPath(receipt))
	if err = a.dir.Mkdir(parentPath); err != nil {
		return receipt, err
	}
	parent, err := a.dir.Sub(parentPath)
	if err != nil {
		return receipt, err
	}
	defer parent.Close()
	controlLease, err := a.dir.Lock(".locks/control.lock", true, false)
	if err != nil {
		return receipt, err
	}
	defer controlLease.Close()
	if err = a.commitControlLocked(s); err != nil {
		return receipt, err
	}
	var latest ArchiveControl
	if err = s.get("control", &latest); err != nil {
		return receipt, err
	}
	instance, known := latest.Instances[key]
	if !known || instance.Retired || latest.RetiredSources[attempt.SourceID] != "" || latest.ConfigHash != attempt.ConfigHash || instance.BindingHash != attempt.BindingHash || instance.CaptureSequence != attempt.CaptureSequence || (instance.Generation != "unenrolled" && instance.Generation != receipt.Header.Generation) {
		return receipt, fmt.Errorf("instance/configuration changed before publication")
	}
	latest.Revision++
	receipt.PublicationSequence = latest.Revision
	if err = validateReceipt(receipt, a.identity.ID); err != nil {
		return receipt, err
	}
	if err = rememberLatestSnapshot(&latest, receipt); err != nil {
		return receipt, err
	}
	if err = s.put("control", latest); err != nil {
		return receipt, err
	}
	if err = a.commitControlLocked(s); err != nil {
		return receipt, err
	}
	if err = staging.WriteJSON("receipt.json", receipt, false); err != nil {
		return receipt, err
	}
	if err = staging.File().Sync(); err != nil {
		return receipt, err
	}
	index, err := a.dir.Lock(".locks/index.lock", true, false)
	if err != nil {
		return receipt, err
	}
	if err = a.checkWriter(control); err == nil {
		publicationAttempted = true
		err = incoming.Rename(id, parent, id)
	}
	index.Close()
	controlLease.Close()
	if err != nil {
		return receipt, err
	}
	if err = s.recordAttempt(id, key, "published-unindexed", receipt); err != nil {
		return receipt, fmt.Errorf("bundle published but catalog reconciliation required: %w", err)
	}
	body, _ := json.Marshal(receipt)
	tx, err := s.db.Begin()
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO bundles(id,path,record) VALUES(?,?,?)", id, receiptPath(receipt), string(body)); err != nil {
		return receipt, err
	}
	if _, err = tx.Exec("UPDATE attempts SET status='committed' WHERE id=?", id); err != nil {
		return receipt, err
	}
	if err = tx.Commit(); err != nil {
		return receipt, err
	}
	return receipt, nil
}

type ArchiveListing struct {
	Verification   map[string]BackupVerificationStatus `json:"verification,omitempty"`
	Schema         int                                 `json:"schema"`
	ArchiveID      string                              `json:"archive_id"`
	Complete       bool                                `json:"complete"`
	Entries        []SnapshotReceipt                   `json:"entries"`
	Diagnostics    []string                            `json:"diagnostics"`
	Incoming       int                                 `json:"incoming"`
	Trash          int                                 `json:"trash"`
	UnavailableIDs []string                            `json:"unavailable_ids,omitempty"`
	NextCursor     string                              `json:"next_cursor,omitempty"`
}

// Receipt scans do not touch the state store, source hosts, or messenger. They
// use existing read-only-open locks and remain usable on full/read-only media.
func (a *backupArchive) scan(maxEntries int) (ArchiveListing, error) {
	list := ArchiveListing{Schema: 1, ArchiveID: a.identity.ID, Complete: true, Entries: []SnapshotReceipt{}, Diagnostics: []string{}}
	index, err := a.dir.Lock(".locks/index.lock", false, false)
	if err != nil {
		return list, err
	}
	defer index.Close()
	seen := map[string]bool{}
	unavailable := map[string]bool{}
	count := 0
	var walk func(*safefs.Directory, string, int)
	walk = func(dir *safefs.Directory, prefix string, depth int) {
		entries, err := dir.File().ReadDir(maxEntries + 1)
		if err != nil && err != io.EOF {
			list.Complete = false
			list.Diagnostics = append(list.Diagnostics, "unreadable scope: "+prefix)
			return
		}
		for _, entry := range entries {
			count++
			if count > maxEntries {
				list.Complete = false
				list.Diagnostics = append(list.Diagnostics, "archive scan limit reached")
				return
			}
			name := entry.Name()
			relative := filepath.Join(prefix, name)
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				list.Complete = false
				list.Diagnostics = append(list.Diagnostics, "foreign/non-directory archive entry: "+relative)
				continue
			}
			child, err := dir.Sub(name)
			if err != nil {
				list.Complete = false
				list.Diagnostics = append(list.Diagnostics, "unreadable directory: "+relative)
				continue
			}
			if depth < 3 {
				walk(child, relative, depth+1)
				child.Close()
				continue
			}
			if seen[name] {
				unavailable[name] = true
			}
			seen[name] = true
			var receipt SnapshotReceipt
			err = child.ReadJSON("receipt.json", &receipt)
			if err == nil {
				err = validateReceipt(receipt, a.identity.ID)
			}
			if err == nil && (receiptPath(receipt) != relative || unavailable[receipt.BackupID]) {
				err = fmt.Errorf("receipt identity/path conflict")
			}
			if err == nil {
				file, e := child.Open("database.db", unix.O_RDONLY|unix.O_NONBLOCK, 0)
				if e == nil {
					info, e := file.Stat()
					file.Close()
					if e != nil || !info.Mode().IsRegular() || info.Size() != receipt.Header.Size {
						err = fmt.Errorf("missing or wrong-size database")
					}
				} else {
					err = e
				}
			}
			child.Close()
			if err != nil {
				list.Complete = false
				list.Diagnostics = append(list.Diagnostics, "invalid bundle: "+relative)
				if safefs.ValidID(name, "b") {
					unavailable[name] = true
				}
				if safefs.ValidID(receipt.BackupID, "b") {
					unavailable[receipt.BackupID] = true
				}
				continue
			}
			seen[receipt.BackupID] = true
			list.Entries = append(list.Entries, receipt)
		}
	}
	sources, err := a.dir.Sub("sources")
	if err != nil {
		return list, err
	}
	walk(sources, "sources", 0)
	sources.Close()
	for name, countDest := range map[string]*int{".incoming": &list.Incoming, ".trash": &list.Trash} {
		dir, err := a.dir.Sub(name)
		if err != nil {
			list.Complete = false
			list.Diagnostics = append(list.Diagnostics, name+" unavailable")
			continue
		}
		entries, err := dir.File().ReadDir(maxEntries + 1)
		dir.Close()
		if err != nil && err != io.EOF {
			list.Complete = false
		}
		*countDest = len(entries)
	}
	filtered := list.Entries[:0]
	for _, entry := range list.Entries {
		if !unavailable[entry.BackupID] {
			filtered = append(filtered, entry)
		}
	}
	list.Entries = filtered
	for id := range unavailable {
		list.UnavailableIDs = append(list.UnavailableIDs, id)
	}
	sort.Strings(list.UnavailableIDs)
	sort.Slice(list.Entries, func(i, j int) bool {
		a, b := list.Entries[i], list.Entries[j]
		if !a.Header.Attempt.RequestedAt.Equal(b.Header.Attempt.RequestedAt) {
			return a.Header.Attempt.RequestedAt.After(b.Header.Attempt.RequestedAt)
		}
		if a.Header.Attempt.CaptureSequence != b.Header.Attempt.CaptureSequence {
			return a.Header.Attempt.CaptureSequence > b.Header.Attempt.CaptureSequence
		}
		return a.BackupID < b.BackupID
	})
	list.Verification, err = a.verificationStatuses(maxEntries)
	if err != nil {
		list.Complete = false
		list.Diagnostics = append(list.Diagnostics, "verification history unavailable: "+err.Error())
	}
	return list, nil
}

func (a *backupArchive) openBundle(receipt SnapshotReceipt) (*safefs.Directory, *safefs.Lock, error) {
	return a.openBundleLease(receipt, false)
}

func (a *backupArchive) openBundleLease(receipt SnapshotReceipt, exclusive bool) (*safefs.Directory, *safefs.Lock, error) {
	if err := validateReceipt(receipt, a.identity.ID); err != nil {
		return nil, nil, err
	}
	index, err := a.dir.Lock(".locks/index.lock", false, false)
	if err != nil {
		return nil, nil, err
	}
	defer index.Close()
	lock, err := a.dir.Lock(".locks/"+receipt.BackupID+".lock", exclusive, false)
	if err != nil {
		return nil, nil, err
	}
	dir, err := a.openBundleDirectory(receipt)
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	return dir, lock, nil
}

// Caller holds the index lock while opening and checking the published identity.
func (a *backupArchive) openBundleDirectory(receipt SnapshotReceipt) (*safefs.Directory, error) {
	if err := validateReceipt(receipt, a.identity.ID); err != nil {
		return nil, err
	}
	dir, err := a.dir.Sub(receiptPath(receipt))
	if err != nil {
		return nil, fmt.Errorf("selected backup unavailable: %w", err)
	}
	var current SnapshotReceipt
	if err = dir.ReadJSON("receipt.json", &current); err != nil || !reflect.DeepEqual(current, receipt) {
		dir.Close()
		return nil, fmt.Errorf("selected backup receipt changed")
	}
	return dir, nil
}
