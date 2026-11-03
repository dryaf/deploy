package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

const publicationCommitIntent = "recovery-publication.json"
const publicationCommitSeal = "recovery-publication-sealed.json"
const publicationCommitComplete = "recovery-publication-complete.json"
const publicationExchange = "recovery-publication-exchange.db"

// The intent freezes reconciliation. Its separately durable seal and completion
// bind the canonical transition; the prepared image's anchor alone cannot do so.
type RecoveryPublicationCommit struct {
	Version               int                      `json:"version"`
	Image                 RecoveryPublicationImage `json:"image"`
	ConfigHash            string                   `json:"config_hash"`
	OriginalSourceHash    string                   `json:"original_source_hash"`
	OriginalDatabaseHash  string                   `json:"original_database_hash"`
	OriginalDatabaseBytes int64                    `json:"original_database_bytes"`
	CommitHash            string                   `json:"commit_hash"`
}

type RecoveryPublicationResult struct {
	Commit             RecoveryPublicationCommit `json:"commit"`
	CanonicalInstalled bool                      `json:"canonical_installed"`
	DeliveryEnabled    bool                      `json:"delivery_enabled"`
	ExecutionEnabled   bool                      `json:"execution_enabled"`
}

// PublishRecovery installs an exact prepared image while retaining quarantine.
// It neither activates services nor grants delivery, intake or command authority.
func PublishRecovery(ctx context.Context, path string, c Config, operation, review, checkpoint, proof, image string) (RecoveryPublicationResult, error) {
	return publishRecovery(ctx, path, c, operation, review, checkpoint, proof, image, nil)
}

func publishRecovery(ctx context.Context, path string, c Config, operation, review, checkpoint, proof, image string, fault func(string) error) (out RecoveryPublicationResult, err error) {
	if err = validPublicationReference(operation, review, checkpoint, proof, image); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	r, err := openRecoveryStatusMode(ctx, path, c, operation, true)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	r.fault = fault
	err = r.withRecoveryReviewLeases(ctx, func(checks []func() error) error {
		pinned, e := verifyRetainedPublication(ctx, r.dir, r.lease, c, operation, review, checkpoint, proof, image, nil)
		if e != nil {
			return e
		}
		v, e := r.reviewRecoveryLocked(ctx, checks)
		if e != nil {
			return e
		}
		if v.ReviewHash != review {
			return fmt.Errorf("publication requires the exact current review")
		}
		if e = r.publicationReady(ctx, v); e != nil {
			return e
		}
		original, _, exists, e := r.originalManifest(ctx, r.db)
		if e != nil {
			return e
		}
		if !exists || original.State != "ready" {
			return fmt.Errorf("publication requires preserved original state")
		}
		want := RecoveryPublicationCommit{Version: 1, Image: pinned, ConfigHash: protocol.Hash(c), OriginalSourceHash: original.hash(), OriginalDatabaseHash: original.DatabaseHash, OriginalDatabaseBytes: original.DatabaseBytes}
		want.CommitHash = protocol.Hash(want)
		if r.publication == nil {
			if e = originalSidecars(r.dir, "state.db", false); e != nil {
				return e
			}
			if e = r.dir.WriteJSON(publicationCommitIntent, want, false); e != nil {
				return e
			}
			r.publication = &want
		} else if *r.publication != want {
			return fmt.Errorf("another recovery publication is frozen")
		}
		if e = recoveryBoundary(fault, "publication_freeze"); e != nil {
			return e
		}
		validate := func() error {
			if e := r.checkPublicationGate(); e != nil {
				return e
			}
			current, e := r.reviewRecoveryLocked(ctx, checks)
			if e != nil {
				return e
			}
			if current.ReviewHash != review {
				return fmt.Errorf("frozen recovery evidence changed")
			}
			verified, e := verifyRetainedPublication(ctx, r.dir, r.lease, c, operation, review, checkpoint, proof, image, nil)
			if e != nil {
				return e
			}
			if verified != pinned {
				return fmt.Errorf("frozen publication image changed")
			}
			return r.publicationReady(ctx, current)
		}
		complete, e := publicationRecord(r.dir, publicationCommitComplete, want)
		if e != nil {
			return e
		}
		sealed, e := publicationRecord(r.dir, publicationCommitSeal, want)
		if e != nil {
			return e
		}
		if complete && !sealed {
			return fmt.Errorf("publication completion lost its seal")
		}
		if !sealed {
			if e = r.removePublicationSidecars(original); e != nil {
				return e
			}
			if e = recoveryBoundary(fault, "publication_sidecars"); e != nil {
				return e
			}
			if e = verifyRetainedDatabase(ctx, r.dir, "state.db", want.OriginalDatabaseHash, want.OriginalDatabaseBytes, c.MaxDatabaseBytes); e != nil {
				return e
			}
			if e = resetRecoveryCopy(r.dir, publicationExchange, c.MaxDatabaseBytes); e != nil {
				return e
			}
			if e = r.dir.Capacity(c.MaxDatabaseBytes + c.MinFreeBytes); e != nil {
				return e
			}
			d, e := retainedDirectory(r.stage, "checkpoints", review)
			if e != nil {
				return e
			}
			e = copySnapshotFile(d, publicationDatabase, r.dir, publicationExchange, SnapshotManifest{DatabaseHash: pinned.DatabaseHash, DatabaseBytes: pinned.DatabaseBytes})
			d.Close()
			if e != nil {
				return e
			}
			if e = recoveryBoundary(fault, "publication_install_copy"); e != nil {
				return e
			}
			if e = verifyRetainedDatabase(ctx, r.dir, publicationExchange, pinned.DatabaseHash, pinned.DatabaseBytes, c.MaxDatabaseBytes); e != nil {
				return e
			}
			if e = validate(); e != nil {
				return e
			}
			if e = r.dir.WriteJSON(publicationCommitSeal, want, false); e != nil {
				return e
			}
		}
		if e = recoveryBoundary(fault, "publication_install_seal"); e != nil {
			return e
		}
		installed, e := r.publicationPair(ctx, want)
		if e != nil {
			return e
		}
		if complete && !installed {
			return fmt.Errorf("completed canonical publication was rolled back")
		}
		if e = validate(); e != nil {
			return e
		}
		if sealed, e = publicationRecord(r.dir, publicationCommitSeal, want); e != nil {
			return e
		} else if !sealed {
			return fmt.Errorf("publication seal disappeared before exchange")
		}
		if nowComplete, e := publicationRecord(r.dir, publicationCommitComplete, want); e != nil {
			return e
		} else if nowComplete != complete {
			return fmt.Errorf("publication completion changed before exchange")
		}
		if !installed {
			// RENAME_EXCHANGE retains the exact old inode and has no missing-state
			// window. Resume recognizes both sides by sealed hashes, not a phase bit.
			if e = safefs.RenameAt(int(r.dir.File().Fd()), publicationExchange, int(r.dir.File().Fd()), "state.db", unix.RENAME_EXCHANGE); e != nil {
				return e
			}
		}
		if e = recoveryBoundary(fault, "publication_exchange"); e != nil {
			return e
		}
		if e = r.dir.File().Sync(); e != nil {
			return e
		}
		if e = recoveryBoundary(fault, "publication_install_sync"); e != nil {
			return e
		}
		if installed, e = r.publicationPair(ctx, want); e != nil {
			return e
		} else if !installed {
			return fmt.Errorf("canonical publication not installed")
		}
		if e = validate(); e != nil {
			return e
		}
		if sealed, e = publicationRecord(r.dir, publicationCommitSeal, want); e != nil {
			return e
		} else if !sealed {
			return fmt.Errorf("publication seal disappeared")
		}
		if !complete {
			if e = r.dir.WriteJSON(publicationCommitComplete, want, false); e != nil {
				return e
			}
		}
		if e = recoveryBoundary(fault, "publication_install_complete"); e != nil {
			return e
		}
		if e = validate(); e != nil {
			return e
		}
		if installed, e = r.publicationPair(ctx, want); e != nil {
			return e
		} else if !installed {
			return fmt.Errorf("canonical publication changed")
		}
		for _, name := range []string{publicationCommitSeal, publicationCommitComplete} {
			if ok, e := publicationRecord(r.dir, name, want); e != nil {
				return e
			} else if !ok {
				return fmt.Errorf("publication record disappeared")
			}
		}
		out = RecoveryPublicationResult{Commit: want, CanonicalInstalled: true}
		return nil
	})
	return
}
