package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// RecoveryCheckpoint identifies an immutable copy of one reviewed projection
// and its separately sealed companion evidence archive.
// It is not a standalone recovery archive: the original databases, ownership
// histories and independent stage evidence must still be retained. Publication
// must revalidate them and complete the remaining handoffs under its own leases.
type RecoveryCheckpoint struct {
	Version            int    `json:"version"`
	Operation          string `json:"operation"`
	ReviewHash         string `json:"review_hash"`
	ProjectionHash     string `json:"projection_hash"`
	DatabaseHash       string `json:"database_hash"`
	DatabaseBytes      int64  `json:"database_bytes"`
	CheckpointHash     string `json:"checkpoint_hash"`
	PublicationAllowed bool   `json:"publication_allowed"`
	DeliveryEnabled    bool   `json:"delivery_enabled"`
	ExecutionEnabled   bool   `json:"execution_enabled"`
}

type recoveryCheckpointIntent struct {
	Version       int
	Binding       recoveryBinding
	CandidateHash string
	Review        RecoveryStatus
}

const checkpointDatabase = "projection.db"
const checkpointTemporary = "copy.db"

// CheckpointRecovery creates or resumes a checkpoint of the exact reviewed
// state. A stale review is rejected even when its old checkpoint still exists.
// No source SQL, canonical ownership, receipt or runtime authority is changed.
func CheckpointRecovery(ctx context.Context, path string, c Config, operation, reviewHash string) (out RecoveryCheckpoint, err error) {
	if !safefs.ValidID(operation, "n") || !safefs.DigestPattern.MatchString(reviewHash) {
		return out, fmt.Errorf("exact recovery operation and review digest required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, err := openRecoveryStatus(ctx, path, c, operation)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	return r.checkpointRecovery(ctx, reviewHash)
}

func (r *recoveryReconciler) checkpointRecovery(ctx context.Context, reviewHash string) (out RecoveryCheckpoint, err error) {
	if !safefs.DigestPattern.MatchString(reviewHash) {
		return out, fmt.Errorf("exact recovery review digest required")
	}
	err = r.withRecoveryReviewLeases(ctx, func(checks []func() error) error {
		review, e := r.reviewRecoveryLocked(ctx, checks)
		if e != nil {
			return e
		}
		if review.ReviewHash != reviewHash || review.OriginalSourceHash == "" {
			return fmt.Errorf("checkpoint requires current review and completed original-source preservation")
		}
		intent := recoveryCheckpointIntent{1, r.journal.binding(), r.journal.CandidateHash, review}
		d, e := r.openCheckpointDirectory(reviewHash)
		if e != nil {
			return e
		}
		defer d.Close()
		var previous recoveryCheckpointIntent
		e = readCheckpointJSON(d, "intent.json", &previous)
		if os.IsNotExist(e) {
			// An empty newly created directory is resumable. Files without an
			// intent are never adopted, overwritten or deleted.
			if e = emptyCheckpointDirectory(d); e != nil {
				return e
			}
			if e = d.WriteJSON("intent.json", intent, false); e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else if protocol.Hash(previous) != protocol.Hash(intent) {
			return fmt.Errorf("checkpoint intent changed")
		}
		if e = recoveryBoundary(r.fault, "checkpoint_intent"); e != nil {
			return e
		}
		var completed, sealed RecoveryCheckpoint
		e = readCheckpointJSON(d, "checkpoint.json", &completed)
		ready := e == nil
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		e = readCheckpointJSON(d, "sealed.json", &sealed)
		if os.IsNotExist(e) && !ready {
			if e = absentOriginalFile(d, checkpointDatabase); e != nil {
				return e
			}
			if e = r.copyCheckpoint(ctx, d, review.ProjectionHash); e != nil {
				return e
			}
			if e = recoveryBoundary(r.fault, "checkpoint_copy"); e != nil {
				return e
			}
			sealed, e = r.inspectCheckpoint(ctx, d, checkpointTemporary, review)
			if e != nil {
				return e
			}
			if e = r.validateCheckpointReview(ctx, d, intent, checks); e != nil {
				return e
			}
			if e = d.WriteJSON("sealed.json", sealed, false); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		if e = recoveryBoundary(r.fault, "checkpoint_sealed"); e != nil {
			return e
		}
		f, fileErr := snapshotFile(d, checkpointDatabase, r.config.MaxDatabaseBytes)
		if fileErr == nil {
			f.Close()
			if e = absentOriginalFile(d, checkpointTemporary); e != nil {
				return e
			}
		} else if os.IsNotExist(fileErr) && !ready {
			// A seal owns exact bytes. Lost or damaged sealed copies are never
			// rebuilt from the mutable reconciliation database.
			copy, e := r.inspectCheckpoint(ctx, d, checkpointTemporary, review)
			if e != nil {
				return e
			}
			if copy != sealed {
				return fmt.Errorf("sealed recovery copy changed")
			}
			if e = d.Rename(checkpointTemporary, d, checkpointDatabase); e != nil {
				return e
			}
		} else {
			return fileErr
		}
		if e = recoveryBoundary(r.fault, "checkpoint_database"); e != nil {
			return e
		}
		out, e = r.inspectCheckpoint(ctx, d, checkpointDatabase, review)
		if e != nil {
			return e
		}
		if sealed != out || (ready && completed != out) {
			return fmt.Errorf("completed recovery checkpoint changed")
		}
		// Retain all leases through the second full observation, not just
		// through the copy. Recheck file intents before publishing a result.
		if e = r.validateCheckpointReview(ctx, d, intent, checks); e != nil {
			return e
		}
		var checkedSeal RecoveryCheckpoint
		if e = readCheckpointJSON(d, "sealed.json", &checkedSeal); e != nil {
			return e
		}
		if checkedSeal != out {
			return fmt.Errorf("checkpoint seal changed before completion")
		}
		if !ready {
			if e = d.WriteJSON("checkpoint.json", out, false); e != nil {
				return e
			}
		}
		if e = recoveryBoundary(r.fault, "checkpoint_ready"); e != nil {
			return e
		}
		if e = r.validateCheckpointReview(ctx, d, intent, checks); e != nil {
			return e
		}
		var final RecoveryCheckpoint
		if e = readCheckpointJSON(d, "checkpoint.json", &final); e != nil {
			return e
		}
		verified, e := r.inspectCheckpoint(ctx, d, checkpointDatabase, review)
		if e != nil {
			return e
		}
		var finalSeal RecoveryCheckpoint
		if e = readCheckpointJSON(d, "sealed.json", &finalSeal); e != nil {
			return e
		}
		if final != out || verified != out || finalSeal != out {
			return fmt.Errorf("recovery checkpoint changed before result")
		}
		if e = r.checkpointEvidence(ctx, d, intent, out, checks); e != nil {
			return e
		}
		// Evidence capture can be long: recheck the projection and its independent
		// records before reporting the combined checkpoint as complete.
		verified, e = r.inspectCheckpoint(ctx, d, checkpointDatabase, review)
		if e != nil {
			return e
		}
		if e = readCheckpointJSON(d, "checkpoint.json", &final); e != nil {
			return e
		}
		if e = readCheckpointJSON(d, "sealed.json", &finalSeal); e != nil {
			return e
		}
		if final != out || verified != out || finalSeal != out {
			return fmt.Errorf("checkpoint projection changed during evidence capture")
		}
		return nil
	})
	return
}

func (r *recoveryReconciler) validateCheckpointReview(ctx context.Context, d *safefs.Directory, intent recoveryCheckpointIntent, checks []func() error) error {
	if err := r.checkCheckpointDirectory(d, intent.Review.ReviewHash); err != nil {
		return err
	}
	var current recoveryCheckpointIntent
	if err := readCheckpointJSON(d, "intent.json", &current); err != nil {
		return err
	}
	if protocol.Hash(current) != protocol.Hash(intent) {
		return fmt.Errorf("checkpoint intent changed during capture")
	}
	review, err := r.reviewRecoveryLocked(ctx, checks)
	if err != nil {
		return err
	}
	if protocol.Hash(review) != protocol.Hash(intent.Review) {
		return fmt.Errorf("recovery evidence changed during checkpoint")
	}
	// Checkpoint artifacts are deliberately outside the source evidence hash.
	// Validate them again after the potentially long source observation.
	if err = readCheckpointJSON(d, "intent.json", &current); err != nil {
		return err
	}
	if protocol.Hash(current) != protocol.Hash(intent) {
		return fmt.Errorf("checkpoint intent changed during review")
	}
	return r.checkCheckpointDirectory(d, intent.Review.ReviewHash)
}
