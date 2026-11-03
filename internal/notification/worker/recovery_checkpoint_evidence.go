package worker

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// The companion archive is bound to a completed projection. It preserves the
// bytes of the current epoch's independent proofs, not copies of credentials,
// live locks or unrelated worker files. Original/source/candidate databases are
// still required; this is not a standalone restore or an activation grant.
type recoveryEvidenceArchive struct {
	Version        int                    `json:"version"`
	CheckpointHash string                 `json:"checkpoint_hash"`
	ReviewHash     string                 `json:"review_hash"`
	EvidenceHash   string                 `json:"evidence_hash"`
	Files          []recoveryEvidenceFile `json:"files"`
	ArchiveHash    string                 `json:"archive_hash"`
}

const evidenceCopyFile = "copy.tmp"

func evidenceCopyName(index int) string { return fmt.Sprintf("%05d.json", index) }

// Called with all recovery/producer leases retained. Existing projection hashes
// remain stable: the companion has its own intent, seal and completion record.
func (r *recoveryReconciler) checkpointEvidence(ctx context.Context, d *safefs.Directory, intent recoveryCheckpointIntent, checkpoint RecoveryCheckpoint, checks []func() error) error {
	files, err := r.recoveryEvidenceInventory(ctx)
	if err != nil {
		return err
	}
	if protocol.Hash(files) != intent.Review.EvidenceHash {
		return fmt.Errorf("checkpoint evidence inventory changed")
	}
	want := recoveryEvidenceArchive{Version: 1, CheckpointHash: checkpoint.CheckpointHash,
		ReviewHash: checkpoint.ReviewHash, EvidenceHash: intent.Review.EvidenceHash, Files: files}
	want.ArchiveHash = protocol.Hash(want)
	var saved recoveryEvidenceArchive
	err = readCheckpointJSON(d, "evidence-intent.json", &saved)
	if os.IsNotExist(err) {
		for _, name := range []string{"evidence-sealed.json", "evidence.json"} {
			if err = absentOriginalFile(d, name); err != nil {
				return err
			}
		}
		unexpected, e := d.Sub("evidence")
		if e == nil {
			unexpected.Close()
			return fmt.Errorf("checkpoint evidence directory has no owning intent")
		}
		if !os.IsNotExist(e) {
			return e
		}
		if err = d.WriteJSON("evidence-intent.json", want, false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if protocol.Hash(saved) != protocol.Hash(want) {
		return fmt.Errorf("checkpoint evidence intent changed")
	}
	if err = recoveryBoundary(r.fault, "checkpoint_evidence_intent"); err != nil {
		return err
	}
	complete, err := checkEvidenceRecord(d, "evidence.json", want)
	if err != nil {
		return err
	}
	sealed, err := checkEvidenceRecord(d, "evidence-sealed.json", want)
	if err != nil {
		return err
	}
	if complete && !sealed {
		return fmt.Errorf("completed checkpoint evidence lost its seal")
	}
	if !sealed {
		if err = d.Mkdir("evidence"); err != nil {
			return err
		}
	}
	copies, err := d.Sub("evidence")
	if err != nil {
		return err
	}
	defer copies.Close()
	if err = privateCheckpointDirectory(copies); err != nil {
		return err
	}
	if err = checkEvidenceDirectory(copies, len(files), !sealed); err != nil {
		return err
	}
	if !sealed {
		if err = resetRecoveryCopy(copies, evidenceCopyFile, safefs.MaxRecordBytes); err != nil {
			return err
		}
		var required int64
		for _, file := range files {
			required += file.Bytes // bounded by the inventory byte budget
		}
		if err = copies.Capacity(required + r.config.MinFreeBytes); err != nil {
			return err
		}
		for i, file := range files {
			if err = ctx.Err(); err != nil {
				return err
			}
			name := evidenceCopyName(i)
			if err = evidenceFileMatches(copies, name, file); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return err // A renamed copy is immutable, even before sealing.
			}
			scope, relative, ok := strings.Cut(file.Path, "/")
			if !ok || (scope != "stage" && scope != "worker") {
				return fmt.Errorf("invalid checkpoint evidence source")
			}
			source := r.stage
			if scope == "worker" {
				source = r.dir
			}
			if err = copySnapshotFile(source, relative, copies, evidenceCopyFile, SnapshotManifest{DatabaseBytes: file.Bytes, DatabaseHash: file.Hash}); err != nil {
				return err
			}
			if err = recoveryBoundary(r.fault, "checkpoint_evidence_copy"); err != nil {
				return err
			}
			if err = copies.Rename(evidenceCopyFile, copies, name); err != nil {
				return err
			}
			if err = recoveryBoundary(r.fault, "checkpoint_evidence_file"); err != nil {
				return err
			}
		}
	}
	validate := func() error {
		if e := r.validateCheckpointReview(ctx, d, intent, checks); e != nil {
			return e
		}
		if exists, e := checkEvidenceRecord(d, "evidence-intent.json", want); e != nil {
			return e
		} else if !exists {
			return fmt.Errorf("checkpoint evidence lost its intent")
		}
		if e := sameEvidenceDirectory(d, copies); e != nil {
			return e
		}
		return verifyEvidenceCopies(ctx, copies, files)
	}
	if err = validate(); err != nil {
		return err
	}
	if !sealed {
		if err = d.WriteJSON("evidence-sealed.json", want, false); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(r.fault, "checkpoint_evidence_sealed"); err != nil {
		return err
	}
	if err = validate(); err != nil {
		return err
	}
	if exists, e := checkEvidenceRecord(d, "evidence-sealed.json", want); e != nil {
		return e
	} else if !exists {
		return fmt.Errorf("checkpoint evidence seal disappeared")
	}
	if !complete {
		if err = d.WriteJSON("evidence.json", want, false); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(r.fault, "checkpoint_evidence_ready"); err != nil {
		return err
	}
	if err = validate(); err != nil {
		return err
	}
	for _, name := range []string{"evidence-sealed.json", "evidence.json"} {
		if exists, e := checkEvidenceRecord(d, name, want); e != nil {
			return e
		} else if !exists {
			return fmt.Errorf("checkpoint evidence completion disappeared")
		}
	}
	_, err = readCheckpointEvidence(ctx, d, checkpoint, intent.Review, r.config.MaxDatabaseBytes)
	return err
}
