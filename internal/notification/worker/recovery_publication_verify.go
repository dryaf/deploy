package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// VerifyRecoveryPublication verifies a completed prepared image independently of
// current canonical state. It proves preparation, never publication or activation.
// The original configuration and all exact artifact references are required.
func VerifyRecoveryPublication(ctx context.Context, path string, c Config, operation, reviewHash, checkpointHash, proofHash, imageHash string) (out RecoveryPublicationImage, err error) {
	if err = c.Validate(); err != nil {
		return
	}
	if err = validPublicationReference(operation, reviewHash, checkpointHash, proofHash, imageHash); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return
	}
	dir, err := privateDirectory(path)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	lease, err := dir.Lock("worker.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	return verifyRetainedPublication(ctx, dir, lease, c, operation, reviewHash, checkpointHash, proofHash, imageHash, nil)
}

func validPublicationReference(operation string, hashes ...string) error {
	if !safefs.ValidID(operation, "n") {
		return fmt.Errorf("exact recovery operation required")
	}
	for _, hash := range hashes {
		if !safefs.DigestPattern.MatchString(hash) {
			return fmt.Errorf("exact recovery review, checkpoint, proof and image digests required")
		}
	}
	return nil
}

// Only immutable retained inputs are read; producer leases are unnecessary.
// Callers already holding the worker lease use this function without reacquiring it.
func verifyRetainedPublication(ctx context.Context, root *safefs.Directory, lease *safefs.Lock, c Config, operation, reviewHash, checkpointHash, proofHash, imageHash string, fault func(string) error) (out RecoveryPublicationImage, err error) {
	if err = validPublicationReference(operation, reviewHash, checkpointHash, proofHash, imageHash); err != nil {
		return
	}
	err = withRetainedCheckpoint(ctx, root, lease, operation, reviewHash, func(stage, d *safefs.Directory, check func() error) error {
		read := func() (RecoveryPublicationImage, error) {
			proof, e := readRetainedCheckpoint(ctx, stage, d, c, operation, reviewHash, checkpointHash)
			if e != nil {
				return RecoveryPublicationImage{}, e
			}
			if proof.ProofHash != proofHash {
				return RecoveryPublicationImage{}, fmt.Errorf("publication proof digest mismatch")
			}
			var intent recoveryCheckpointIntent
			if e = readCheckpointJSON(d, "intent.json", &intent); e != nil {
				return RecoveryPublicationImage{}, e
			}
			review := intent.Review
			review.ReviewHash = ""
			if intent.Review.ReviewHash != reviewHash || protocol.Hash(review) != reviewHash {
				return RecoveryPublicationImage{}, fmt.Errorf("publication checkpoint review changed")
			}
			return readPreparedPublication(ctx, d, proof, intent.Review, imageHash, c.MaxDatabaseBytes)
		}
		var e error
		if out, e = read(); e != nil {
			return e
		}
		if e = recoveryBoundary(fault, "recovery_publication_verified"); e != nil {
			return e
		}
		if e = check(); e != nil {
			return e
		}
		again, e := read()
		if e != nil {
			return e
		}
		if again != out {
			return fmt.Errorf("retained publication image changed during verification")
		}
		return nil
	})
	return
}

// proof and review must have been independently validated by the caller. A seal
// alone is insufficient: this reader never finishes an interrupted preparation.
func readPreparedPublication(ctx context.Context, d *safefs.Directory, proof RecoveryCheckpointProof, review RecoveryStatus, imageHash string, limit int64) (out RecoveryPublicationImage, err error) {
	anchor := recoveryPublicationAnchor{1, "fenced", proof}
	var saved recoveryPublicationAnchor
	var sealed, completed RecoveryPublicationImage
	if err = readCheckpointJSON(d, "publication-intent.json", &saved); err != nil {
		return
	}
	if saved != anchor {
		return out, fmt.Errorf("publication intent differs from historical proof")
	}
	if err = readCheckpointJSON(d, "publication-sealed.json", &sealed); err != nil {
		return
	}
	if err = readCheckpointJSON(d, "publication-image.json", &completed); err != nil {
		return
	}
	out, err = inspectPublicationImage(ctx, d, publicationDatabase, anchor, review, limit)
	if err != nil {
		return
	}
	if out.ImageHash != imageHash || out != sealed || out != completed {
		return out, fmt.Errorf("publication image reference, seal or completion mismatch")
	}
	// The immutable SQL read and byte hashing can take time. Do not return
	// authority from records that changed while that work was in progress.
	if err = readCheckpointJSON(d, "publication-intent.json", &saved); err != nil {
		return
	}
	if err = readCheckpointJSON(d, "publication-sealed.json", &sealed); err != nil {
		return
	}
	if err = readCheckpointJSON(d, "publication-image.json", &completed); err != nil {
		return
	}
	if saved != anchor || out != sealed || out != completed {
		return out, fmt.Errorf("publication records changed during image verification")
	}
	return out, ctx.Err()
}
