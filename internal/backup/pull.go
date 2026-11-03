package backup

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type ArchiveFilter struct {
	Source     string `json:"source"`
	Instance   string `json:"instance"`
	Generation string `json:"generation"`
}

type archiveCursor struct {
	ArchiveID string        `json:"archive_id"`
	Filter    ArchiveFilter `json:"filter"`
	Watermark uint64        `json:"watermark"`
	After     string        `json:"after"`
}

func (a *backupArchive) filter(list ArchiveListing, filter ArchiveFilter) ([]SnapshotReceipt, error) {
	sourceID := filter.Source
	if sourceID != "" && !safefs.ValidID(sourceID, "s") {
		matches := map[string]bool{}
		for _, r := range list.Entries {
			if r.SourceAlias == sourceID {
				matches[r.Header.Attempt.SourceID] = true
			}
		}
		if control, err := a.checkpoint(); err == nil {
			for _, instance := range control.Instances {
				if instance.Alias == sourceID {
					matches[instance.SourceID] = true
				}
			}
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("source alias is ambiguous; use a source UUID")
		}
		if len(matches) == 0 {
			return []SnapshotReceipt{}, nil
		}
		for id := range matches {
			sourceID = id
		}
	}
	filtered := []SnapshotReceipt{}
	for _, r := range list.Entries {
		if sourceID != "" && r.Header.Attempt.SourceID != sourceID {
			continue
		}
		if filter.Instance != "" && r.Header.Attempt.Service != filter.Instance {
			continue
		}
		if filter.Generation != "" && r.Header.Generation != filter.Generation {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered, nil
}

func (a *backupArchive) list(filter ArchiveFilter, limit int, cursor string) (ArchiveListing, error) {
	if limit < 1 || limit > 500 {
		return ArchiveListing{}, fmt.Errorf("list limit must be 1 to 500")
	}
	list, err := a.scan(100000)
	if err != nil {
		return list, err
	}
	entries, err := a.filter(list, filter)
	if err != nil {
		return list, err
	}
	page := archiveCursor{ArchiveID: a.identity.ID, Filter: filter}
	for _, entry := range entries {
		if entry.PublicationSequence > page.Watermark {
			page.Watermark = entry.PublicationSequence
		}
	}
	if cursor != "" {
		if len(cursor) > 4096 {
			return list, fmt.Errorf("cursor exceeds limit")
		}
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || protocol.StrictBackupJSON(data, &page) != nil || page.ArchiveID != a.identity.ID || page.Filter != filter || page.Watermark == 0 {
			return list, fmt.Errorf("cursor does not match archive and filters")
		}
	}
	selected := []SnapshotReceipt{}
	after := page.After == ""
	for _, entry := range entries {
		if entry.PublicationSequence > page.Watermark {
			continue
		}
		if !after {
			if entry.BackupID == page.After {
				after = true
			}
			continue
		}
		selected = append(selected, entry)
	}
	if !after {
		return list, fmt.Errorf("cursor boundary is unavailable; restart listing")
	}
	list.Entries = selected
	if len(selected) > limit {
		list.Entries = selected[:limit]
		page.After = selected[limit-1].BackupID
		data, _ := json.Marshal(page)
		list.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	verification := map[string]BackupVerificationStatus{}
	for _, receipt := range list.Entries {
		if status, ok := list.Verification[receipt.BackupID]; ok {
			verification[receipt.BackupID] = status
		}
	}
	list.Verification = verification
	return list, nil
}

func (a *backupArchive) selectReceipt(id string, latest bool, filter ArchiveFilter) (SnapshotReceipt, error) {
	var zero SnapshotReceipt
	if (id == "") == !latest {
		return zero, fmt.Errorf("select exactly one of --backup and --latest")
	}
	if id != "" && !safefs.ValidID(id, "b") {
		return zero, fmt.Errorf("backup ID must be complete, not a path or prefix")
	}
	if latest && (filter.Source == "" || filter.Instance == "") {
		return zero, fmt.Errorf("--latest requires --source and --instance")
	}
	list, err := a.scan(100000)
	if err != nil {
		return zero, err
	}
	entries, err := a.filter(list, filter)
	if err != nil {
		return zero, err
	}
	if id != "" {
		for _, r := range entries {
			if r.BackupID == id {
				return r, nil
			}
		}
		return zero, fmt.Errorf("selected backup unavailable")
	}
	if !list.Complete {
		return zero, fmt.Errorf("latest cannot be established from an incomplete archive; select an explicit readable backup ID")
	}
	if len(entries) == 0 {
		return zero, fmt.Errorf("no matching stored backups")
	}
	control, err := a.checkpoint()
	if err != nil {
		return zero, fmt.Errorf("latest capture evidence unavailable; select an explicit backup ID")
	}
	generation := filter.Generation
	if generation == "" {
		instance, ok := control.Instances[instanceKey(entries[0].Header.Attempt.SourceID, filter.Instance)]
		if !ok || instance.Retired || !safefs.ValidID(instance.Generation, "g") {
			return zero, fmt.Errorf("active generation is ambiguous/retired; select an explicit generation")
		}
		generation = instance.Generation
	}
	var selected *SnapshotReceipt
	for _, r := range entries {
		if r.Header.Generation != generation {
			continue
		}
		if selected == nil || r.WriterEpoch > selected.WriterEpoch || (r.WriterEpoch == selected.WriterEpoch && r.Header.Attempt.CaptureSequence > selected.Header.Attempt.CaptureSequence) {
			copy := r
			selected = &copy
		}
	}
	if selected == nil {
		return zero, fmt.Errorf("no backup of selected generation")
	}
	if err = requireLatestSnapshot(control, *selected); err != nil {
		return zero, err
	}
	return *selected, nil
}

type DownloadRecord struct {
	Schema       int                         `json:"schema"`
	ArchiveID    string                      `json:"archive_id"`
	BackupID     string                      `json:"backup_id"`
	SHA256       string                      `json:"sha256"`
	DownloadedAt time.Time                   `json:"downloaded_at"`
	Validation   protocol.SnapshotValidation `json:"validation"`
}

func publishLocalPull(receipt SnapshotReceipt, rawReceipt []byte, output string, copyDatabase func(*os.File) error, validate func(*os.File) (protocol.SnapshotValidation, error)) (string, error) {
	if err := validateReceipt(receipt, receipt.ArchiveID); err != nil {
		return "", err
	}
	if output == "" {
		if err := os.Mkdir("deploy-backups", 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
		output = filepath.Join("deploy-backups", receipt.BackupID)
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return "", err
	}
	if absolute == "/" {
		return "", fmt.Errorf("output must be a new directory")
	}
	parent, err := safefs.Open(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	defer parent.Close()
	name := filepath.Base(absolute)
	var stat unix.Stat_t
	if err = unix.Fstatat(int(parent.File().Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return "", fmt.Errorf("output already exists; choose a new directory")
	}
	if err != unix.ENOENT {
		return "", err
	}
	if err = parent.Capacity(receipt.Header.Size + 64<<20); err != nil {
		return "", err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return "", err
	}
	tempName := ".deploy-pull-" + id
	if err = unix.Mkdirat(int(parent.File().Fd()), tempName, 0700); err != nil {
		return "", err
	}
	temp, err := parent.Sub(tempName)
	if err != nil {
		return "", err
	}
	defer temp.Close()
	publicationAttempted := false
	defer func() {
		if !publicationAttempted {
			for _, n := range []string{"database.db", "receipt.json", "download.json"} {
				_ = unix.Unlinkat(int(temp.File().Fd()), n, 0)
			}
			_ = temp.File().Sync()
			current, e := parent.Sub(tempName)
			if e == nil {
				a, _ := current.File().Stat()
				b, _ := temp.File().Stat()
				current.Close()
				if a != nil && b != nil && os.SameFile(a, b) {
					_ = unix.Unlinkat(int(parent.File().Fd()), tempName, unix.AT_REMOVEDIR)
				}
			}
			_ = parent.File().Sync()
		}
	}()
	file, err := temp.Open("database.db", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err = copyDatabase(file); err != nil {
		return "", err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != receipt.Header.Size {
		return "", fmt.Errorf("download size differs from receipt")
	}
	checksum, err := safefs.DigestFile(file)
	if err != nil || checksum != receipt.Header.SHA256 {
		return "", fmt.Errorf("download checksum differs from selected snapshot")
	}
	validation, err := validate(file)
	if err != nil {
		return "", err
	}
	if validation.Result != "ok" {
		return "", fmt.Errorf("download validation did not succeed")
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	var decoded SnapshotReceipt
	if err = protocol.StrictBackupJSON(rawReceipt, &decoded); err != nil {
		return "", err
	}
	a, _ := json.Marshal(decoded)
	b, _ := json.Marshal(receipt)
	if string(a) != string(b) {
		return "", fmt.Errorf("raw receipt differs from selected receipt")
	}
	rf, err := temp.Open("receipt.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, err = rf.Write(rawReceipt)
	if err == nil {
		err = rf.Sync()
	}
	closeErr := rf.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = temp.WriteJSON("download.json", DownloadRecord{1, receipt.ArchiveID, receipt.BackupID, checksum, time.Now().UTC(), validation}, false); err != nil {
		return "", err
	}
	if err = temp.File().Sync(); err != nil {
		return "", err
	}
	publicationAttempted = true
	if err = parent.Rename(tempName, parent, name); err != nil {
		return "", fmt.Errorf("download publication failed or durability uncertain (private staging %s): %w", tempName, err)
	}
	return absolute, nil
}

func (a *backupArchive) pull(receipt SnapshotReceipt, output string, validate func(*os.File) (protocol.SnapshotValidation, error)) (string, error) {
	dir, lock, err := a.openBundle(receipt)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	defer lock.Close()
	if err = a.downloadVerificationSafe(receipt); err != nil {
		return "", err
	}
	rf, err := dir.Open("receipt.json", unix.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(rf, protocol.MaxBackupHeader+1))
	rf.Close()
	if err != nil {
		return "", err
	}
	file, err := dir.Open("database.db", unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return publishLocalPull(receipt, raw, output, func(dst *os.File) error {
		_, err := io.CopyN(dst, file, receipt.Header.Size)
		if err != nil {
			return err
		}
		var trailing [1]byte
		if n, err := file.Read(trailing[:]); n != 0 || err != io.EOF {
			return fmt.Errorf("archive database size changed")
		}
		return nil
	}, validate)
}
