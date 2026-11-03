package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
)

const publicationAnchorKey = "recovery-publication-v1"
const publicationCopy = "publication-copy.db"
const publicationDatabase = "publication.db"

// The anchor records historical evidence only. A later publication/activation
// protocol must supply separate completed authority and current handoff checks.
type recoveryPublicationAnchor struct {
	Version int                     `json:"version"`
	Mode    string                  `json:"mode"`
	Proof   RecoveryCheckpointProof `json:"proof"`
}

type RecoveryPublicationImage struct {
	Version            int    `json:"version"`
	Operation          string `json:"operation"`
	ReviewHash         string `json:"review_hash"`
	CheckpointHash     string `json:"checkpoint_hash"`
	ProofHash          string `json:"proof_hash"`
	AnchorHash         string `json:"anchor_hash"`
	DatabaseHash       string `json:"database_hash"`
	DatabaseBytes      int64  `json:"database_bytes"`
	ImageHash          string `json:"image_hash"`
	PublicationAllowed bool   `json:"publication_allowed"`
	DeliveryEnabled    bool   `json:"delivery_enabled"`
	ExecutionEnabled   bool   `json:"execution_enabled"`
}

// PrepareRecoveryPublication constructs a sealed, fenced database image. It does
// not replace canonical state or declare pending reconciliation stages complete.
func PrepareRecoveryPublication(ctx context.Context, path string, c Config, operation, reviewHash, checkpointHash, proofHash string) (out RecoveryPublicationImage, err error) {
	if !safefs.ValidID(operation, "n") || !safefs.DigestPattern.MatchString(reviewHash) || !safefs.DigestPattern.MatchString(checkpointHash) || !safefs.DigestPattern.MatchString(proofHash) {
		return out, fmt.Errorf("exact recovery operation, review, checkpoint and proof digests required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r, err := openRecoveryStatus(ctx, path, c, operation)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	return r.preparePublication(ctx, reviewHash, checkpointHash, proofHash)
}

func (r *recoveryReconciler) preparePublication(ctx context.Context, reviewHash, checkpointHash, proofHash string) (out RecoveryPublicationImage, err error) {
	err = r.withRecoveryReviewLeases(ctx, func(checks []func() error) error {
		review, e := r.reviewRecoveryLocked(ctx, checks)
		if e != nil {
			return e
		}
		if review.ReviewHash != reviewHash {
			return fmt.Errorf("publication preparation requires the current review")
		}
		proof, e := verifyRetainedCheckpoint(ctx, r.dir, r.lease, r.config, r.journal.Operation, reviewHash, checkpointHash, nil)
		if e != nil {
			return e
		}
		if proof.ProofHash != proofHash {
			return fmt.Errorf("publication proof digest mismatch")
		}
		d, e := retainedDirectory(r.stage, "checkpoints", reviewHash)
		if e != nil {
			return e
		}
		defer d.Close()
		anchor := recoveryPublicationAnchor{1, "fenced", proof}
		var saved recoveryPublicationAnchor
		e = readCheckpointJSON(d, "publication-intent.json", &saved)
		if os.IsNotExist(e) {
			for _, name := range []string{publicationCopy, publicationDatabase, "publication-sealed.json", "publication-image.json"} {
				if e = absentOriginalFile(d, name); e != nil {
					return e
				}
			}
			for _, name := range []string{publicationCopy, publicationDatabase} {
				if e = originalSidecars(d, name, true); e != nil {
					return e
				}
			}
			if e = d.WriteJSON("publication-intent.json", anchor, false); e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else if saved != anchor {
			return fmt.Errorf("publication image intent changed")
		}
		if e = recoveryBoundary(r.fault, "publication_intent"); e != nil {
			return e
		}
		var sealed, completed RecoveryPublicationImage
		e = readCheckpointJSON(d, "publication-image.json", &completed)
		ready := e == nil
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		e = readCheckpointJSON(d, "publication-sealed.json", &sealed)
		if os.IsNotExist(e) && !ready {
			if e = absentOriginalFile(d, publicationDatabase); e != nil {
				return e
			}
			if e = r.buildPublicationImage(ctx, d, anchor); e != nil {
				return e
			}
			if e = recoveryBoundary(r.fault, "publication_copy"); e != nil {
				return e
			}
			sealed, e = inspectPublicationImage(ctx, d, publicationCopy, anchor, review, r.config.MaxDatabaseBytes)
			if e != nil {
				return e
			}
			if e = r.validatePublicationPreparation(ctx, d, anchor, review, checks); e != nil {
				return e
			}
			if e = d.WriteJSON("publication-sealed.json", sealed, false); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		if e = recoveryBoundary(r.fault, "publication_sealed"); e != nil {
			return e
		}
		f, fileErr := snapshotFile(d, publicationDatabase, r.config.MaxDatabaseBytes)
		if fileErr == nil {
			f.Close()
			if e = absentOriginalFile(d, publicationCopy); e != nil {
				return e
			}
		} else if os.IsNotExist(fileErr) && !ready {
			copy, e := inspectPublicationImage(ctx, d, publicationCopy, anchor, review, r.config.MaxDatabaseBytes)
			if e != nil {
				return e
			}
			if copy != sealed {
				return fmt.Errorf("sealed publication image changed")
			}
			if e = d.Rename(publicationCopy, d, publicationDatabase); e != nil {
				return e
			}
		} else {
			return fileErr
		}
		if e = recoveryBoundary(r.fault, "publication_image"); e != nil {
			return e
		}
		out, e = inspectPublicationImage(ctx, d, publicationDatabase, anchor, review, r.config.MaxDatabaseBytes)
		if e != nil {
			return e
		}
		if out != sealed || (ready && completed != out) {
			return fmt.Errorf("publication image completion changed")
		}
		if e = r.validatePublicationPreparation(ctx, d, anchor, review, checks); e != nil {
			return e
		}
		if e = readCheckpointJSON(d, "publication-sealed.json", &sealed); e != nil {
			return e
		}
		if sealed != out {
			return fmt.Errorf("publication image seal changed before completion")
		}
		if !ready {
			if e = d.WriteJSON("publication-image.json", out, false); e != nil {
				return e
			}
		}
		if e = recoveryBoundary(r.fault, "publication_ready"); e != nil {
			return e
		}
		if e = r.validatePublicationPreparation(ctx, d, anchor, review, checks); e != nil {
			return e
		}
		actual, e := readPreparedPublication(ctx, d, proof, review, out.ImageHash, r.config.MaxDatabaseBytes)
		if e != nil {
			return e
		}
		if actual != out {
			return fmt.Errorf("publication image changed before result")
		}
		return nil
	})
	return
}

func (r *recoveryReconciler) validatePublicationPreparation(ctx context.Context, d *safefs.Directory, anchor recoveryPublicationAnchor, review RecoveryStatus, checks []func() error) error {
	if err := r.checkCheckpointDirectory(d, review.ReviewHash); err != nil {
		return err
	}
	current, err := r.reviewRecoveryLocked(ctx, checks)
	if err != nil {
		return err
	}
	if current.ReviewHash != review.ReviewHash {
		return fmt.Errorf("recovery state changed during publication preparation")
	}
	proof, err := verifyRetainedCheckpoint(ctx, r.dir, r.lease, r.config, r.journal.Operation, review.ReviewHash, anchor.Proof.CheckpointHash, nil)
	if err != nil {
		return err
	}
	if proof != anchor.Proof {
		return fmt.Errorf("publication source proof changed")
	}
	var saved recoveryPublicationAnchor
	if err = readCheckpointJSON(d, "publication-intent.json", &saved); err != nil {
		return err
	}
	if saved != anchor {
		return fmt.Errorf("publication intent changed during preparation")
	}
	return r.checkCheckpointDirectory(d, review.ReviewHash)
}
