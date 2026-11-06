package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func (s *Store) runtimeLEDDirectory() *safefs.Directory {
	if s.runtimeEvidenceDir != nil {
		return s.runtimeEvidenceDir
	}
	return s.dir
}
func (s *Store) runtimeLEDClearFilePrefix() string {
	if s.recoveryProof.AncestorHash == "" {
		return "led-runtime-clear-"
	}
	return "led-runtime-clear-" + protocol.Hash(s.identity) + "-"
}
func (s *Store) runtimeLEDClearKeyPrefix() string {
	if s.recoveryProof.AncestorHash == "" {
		return runtimeLEDClearPrefix
	}
	return runtimeLEDClearPrefix + protocol.Hash(s.identity) + "/"
}
func (s *Store) runtimeLEDClearFile(n int) string {
	return fmt.Sprintf("%s%06d", s.runtimeLEDClearFilePrefix(), n)
}
func (s *Store) runtimeLEDClearHead() string { return s.runtimeLEDClearFilePrefix() + "head.json" }
func (s *Store) runtimeLEDClearKey(n int) string {
	return fmt.Sprintf("%s%06d", s.runtimeLEDClearKeyPrefix(), n)
}

// Old epochs alone do not prove ownership. Require the exact inherited fault
// in a verified parent adoption/cleanup chain, with the latest runtime files
// retained independently when the original is newer than the restored snapshot.
func (r *recoveryReconciler) validateAncestorLEDFault(f recoveryLEDFault) error {
	if r.journal.Version != 2 {
		return fmt.Errorf("LED fault has no proven ancestor")
	}
	ctx := context.Background()
	e, err := readSnapshotRecovery(r.stage, r.journal.Snapshot, r.config)
	if err != nil {
		return err
	}
	d, err := r.stage.Sub(snapshotProofDirectory)
	if err != nil {
		return err
	}
	defer d.Close()
	for _, name := range []string{"source.db", originalArchiveFile} {
		if name == "source.db" {
			if err = verifyRetainedDatabase(ctx, r.stage, name, r.journal.Snapshot.DatabaseHash, r.journal.Snapshot.DatabaseBytes, r.config.MaxDatabaseBytes); err != nil {
				return err
			}
		} else {
			var original recoveryOriginalManifest
			if err = readCheckpointJSON(r.stage, originalManifestFile, &original); err != nil {
				return err
			}
			if err = originalFileMatches(r.stage, name, original); err != nil {
				return err
			}
			if err = verifyOriginalRuntime(ctx, r.stage, r.journal, r.config); err != nil {
				return err
			}
		}
		db, err := immutableOriginalDB(r.stage, name)
		if err != nil {
			return err
		}
		parent := &Store{dir: d, db: db, config: r.config, identity: r.journal.Before, recoveryCommitHash: e.Publication.CommitHash, recoveryProof: e.Proof, recoveryPublication: e.Publication, verificationOnly: true}
		if name == originalArchiveFile {
			parent.runtimeEvidenceDir, err = r.stage.Sub("original-runtime")
			if err != nil {
				db.Close()
				return err
			}
		}
		faults, err := parent.recoveredLEDFaults(ctx, db)
		db.Close()
		if parent.runtimeEvidenceDir != nil {
			parent.runtimeEvidenceDir.Close()
		}
		if err != nil {
			return err
		}
		if owned, ok := faults[f.ID]; ok {
			owned.Obsolete, f.Obsolete = false, false
			if owned == f {
				return nil
			}
			return fmt.Errorf("ancestor LED fault incarnation changed")
		}
	}
	return fmt.Errorf("LED fault is not retained by either parent source")
}
