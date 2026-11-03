package worker

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
	"os"
)

// A checkpointed original may still have an empty WAL and its rebuildable SHM
// index. Only the frozen publisher may remove them, before sealing the exchange
// copy. Nonempty WAL/journal data is drift, never disposable recovery debris.
func (r *recoveryReconciler) removePublicationSidecars(original recoveryOriginalManifest) error {
	if err := r.checkPublicationGate(); err != nil {
		return err
	}
	if r.publication == nil {
		return fmt.Errorf("sidecar cleanup requires frozen publication")
	}
	if err := originalFileMatches(r.dir, "state.db", original); err != nil {
		return err
	}
	if err := originalSidecars(r.dir, "state.db", false); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		name := "state.db" + suffix
		f, err := r.dir.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		if (suffix != "-shm" && info.Size() != 0) || info.Size() > r.config.MaxDatabaseBytes {
			return fmt.Errorf("original sidecar changed before publication")
		}
		if err = originalSidecars(r.dir, "state.db", false); err != nil {
			return err
		}
		if err = unix.Unlinkat(int(r.dir.File().Fd()), name, 0); err != nil {
			return err
		}
	}
	return r.dir.File().Sync()
}

func publicationRecord(d *safefs.Directory, name string, want RecoveryPublicationCommit) (bool, error) {
	var got RecoveryPublicationCommit
	err := readCheckpointJSON(d, name, &got)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if got != want {
		return false, fmt.Errorf("publication record changed: %s", name)
	}
	return true, nil
}

func (r *recoveryReconciler) checkPublicationGate() error {
	var saved RecoveryPublicationCommit
	err := readCheckpointJSON(r.dir, publicationCommitIntent, &saved)
	if os.IsNotExist(err) && r.publication == nil {
		for _, name := range []string{publicationCommitSeal, publicationCommitComplete, publicationExchange} {
			if err := absentOriginalFile(r.dir, name); err != nil {
				return err
			}
		}
		return originalSidecars(r.dir, publicationExchange, true)
	}
	if err != nil {
		return err
	}
	if r.publication == nil {
		return fmt.Errorf("reconciliation is frozen for publication; resume recovery-publish")
	}
	if saved != *r.publication || saved.Version != 1 || saved.ConfigHash != protocol.Hash(r.config) || saved.OriginalDatabaseBytes <= 0 || saved.OriginalDatabaseBytes > r.config.MaxDatabaseBytes || !safefs.DigestPattern.MatchString(saved.OriginalDatabaseHash) || !safefs.DigestPattern.MatchString(saved.OriginalSourceHash) {
		return fmt.Errorf("invalid publication freeze authority")
	}
	hash := saved.CommitHash
	saved.CommitHash = ""
	if protocol.Hash(saved) != hash {
		return fmt.Errorf("publication freeze digest changed")
	}
	return validPublicationReference(saved.Image.Operation, saved.Image.ReviewHash, saved.Image.CheckpointHash, saved.Image.ProofHash, saved.Image.ImageHash)
}

func (r *recoveryReconciler) checkPublicationSource(original recoveryOriginalManifest) error {
	p := r.publication
	if p.OriginalSourceHash != original.hash() || p.OriginalDatabaseHash != original.DatabaseHash || p.OriginalDatabaseBytes != original.DatabaseBytes {
		return fmt.Errorf("publication original source changed")
	}
	if err := originalFileMatches(r.dir, "state.db", original); err == nil {
		return nil
	}
	return verifyRetainedDatabase(context.Background(), r.dir, "state.db", p.Image.DatabaseHash, p.Image.DatabaseBytes, r.config.MaxDatabaseBytes)
}

func (r *recoveryReconciler) publicationPair(ctx context.Context, p RecoveryPublicationCommit) (bool, error) {
	if err := verifyRetainedDatabase(ctx, r.dir, "state.db", p.Image.DatabaseHash, p.Image.DatabaseBytes, r.config.MaxDatabaseBytes); err == nil {
		return true, verifyRetainedDatabase(ctx, r.dir, publicationExchange, p.OriginalDatabaseHash, p.OriginalDatabaseBytes, r.config.MaxDatabaseBytes)
	}
	if err := verifyRetainedDatabase(ctx, r.dir, "state.db", p.OriginalDatabaseHash, p.OriginalDatabaseBytes, r.config.MaxDatabaseBytes); err != nil {
		return false, err
	}
	return false, verifyRetainedDatabase(ctx, r.dir, publicationExchange, p.Image.DatabaseHash, p.Image.DatabaseBytes, r.config.MaxDatabaseBytes)
}
