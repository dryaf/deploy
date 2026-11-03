package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/safefs"
)

const maxRecoveryEvidenceFiles = 32768

// Paths are logical source names, never executable instructions. Checkpoint
// artifacts live outside this inventory to avoid recursive review digests.
type recoveryEvidenceFile struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Hash  string `json:"hash"`
}

func (r *recoveryReconciler) recoveryEvidenceInventory(ctx context.Context) ([]recoveryEvidenceFile, error) {
	var entries []recoveryEvidenceFile
	remaining := r.config.MaxDatabaseBytes
	add := func(d *safefs.Directory, prefix, name string, optional bool) error {
		if !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") {
			return fmt.Errorf("invalid recovery evidence filename")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := snapshotFile(d, name, safefs.MaxRecordBytes)
		if optional && os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		i, err := f.Stat()
		if err != nil {
			return err
		}
		if i.Size() > remaining || len(entries) == maxRecoveryEvidenceFiles {
			return fmt.Errorf("recovery evidence exceeds byte/file budget")
		}
		hash, err := boundedEvidenceDigest(f, i.Size())
		if err != nil {
			return err
		}
		remaining -= i.Size()
		entries = append(entries, recoveryEvidenceFile{prefix + "/" + name, i.Size(), hash})
		return nil
	}
	dir, err := r.stage.Sub(".")
	if err != nil {
		return nil, err
	}
	names, err := dir.File().Readdirnames(maxRecoveryEvidenceFiles + 1)
	dir.Close()
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > maxRecoveryEvidenceFiles {
		return nil, fmt.Errorf("recovery stage exceeds file budget")
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".json") && !safefs.IsJSONTemporary(name) {
			if err = add(r.stage, "stage", name, false); err != nil {
				return nil, err
			}
		}
	}
	for _, name := range []string{"identity.json", "recovery.json"} {
		if err = add(r.dir, "worker", name, false); err != nil {
			return nil, err
		}
	}
	for _, e := range r.config.Producers {
		for _, suffix := range []string{".json", ".journal.json"} {
			if err = add(r.dir, "worker", ownershipName(e.Producer)+suffix, false); err != nil {
				return nil, err
			}
		}
		var owner ownership
		if err = r.dir.ReadJSON(ownershipName(e.Producer)+".json", &owner); err != nil {
			return nil, err
		}
		if !validOwnership(owner) || owner.Producer != e.Producer || owner.Revision > uint64(maxRecoveryEvidenceFiles-len(entries)) {
			return nil, fmt.Errorf("invalid or oversized recovery ownership history")
		}
		for revision := uint64(1); revision <= owner.Revision; revision++ {
			if err = add(r.dir, "worker", ownershipHistoryPath(e.Producer, revision), false); err != nil {
				return nil, err
			}
		}
	}
	// Preserve pending administrative intents too. Missing SQL acknowledgment
	// files remain absent; the review validates these states without repairing.
	a := messageAdministrator{s: &Store{dir: r.dir}}
	ids, err := a.historyFiles()
	if err != nil {
		return nil, err
	}
	for id := range ids {
		for _, name := range []string{messageIntentFile(id), messageResultFile(id)} {
			if err = add(r.dir, "worker", name, true); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(entries, func(a, b recoveryEvidenceFile) int { return strings.Compare(a.Path, b.Path) })
	return entries, ctx.Err()
}
