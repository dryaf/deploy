package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func readRetainedCheckpoint(ctx context.Context, stage, d *safefs.Directory, c Config, operation, reviewHash, checkpointHash string) (out RecoveryCheckpointProof, err error) {
	var intent recoveryCheckpointIntent
	var cp, seal RecoveryCheckpoint
	if err = readCheckpointJSON(d, "intent.json", &intent); err != nil {
		return
	}
	if err = readCheckpointJSON(d, "checkpoint.json", &cp); err != nil {
		return
	}
	if err = readCheckpointJSON(d, "sealed.json", &seal); err != nil {
		return
	}
	review := intent.Review
	if cp != seal || cp.Operation != operation || cp.ReviewHash != reviewHash || cp.CheckpointHash != checkpointHash ||
		intent.Version != 1 || intent.Binding.Operation != operation || !validOwnershipRecoveryBinding(intent.Binding) ||
		review.Identity != intent.Binding.After || review.ConfigHash != protocol.Hash(c) || !safefs.DigestPattern.MatchString(intent.CandidateHash) ||
		!safefs.DigestPattern.MatchString(review.OriginalSourceHash) || !safefs.DigestPattern.MatchString(cp.DatabaseHash) ||
		cp.DatabaseBytes <= 0 || cp.DatabaseBytes > c.MaxDatabaseBytes {
		return out, fmt.Errorf("retained checkpoint reference or configuration mismatch")
	}
	evidence, err := readCheckpointEvidence(ctx, d, cp, review, c.MaxDatabaseBytes)
	if err != nil {
		return out, err
	}
	files, err := d.Sub("evidence")
	if err != nil {
		return out, err
	}
	defer files.Close()
	index := make(map[string]int, len(evidence.Files))
	for i, f := range evidence.Files {
		index[f.Path] = i
	}
	read := func(name string, value any) error {
		i, ok := index[name]
		if !ok {
			return fmt.Errorf("retained recovery evidence missing: %s", name)
		}
		return files.ReadJSON(evidenceCopyName(i), value)
	}
	var journal snapshotRecovery
	if err = read("worker/recovery.json", &journal); err != nil {
		return out, err
	}
	if err = journal.validate(c); err != nil {
		return out, err
	}
	if err = verifyOriginalRuntime(ctx, stage, journal, c); err != nil {
		return out, err
	}
	if journal.Phase != "quarantined" || journal.binding() != intent.Binding || journal.CandidateHash != intent.CandidateHash {
		return out, fmt.Errorf("retained recovery journal mismatch")
	}
	var identity protocol.Identity
	if err = read("worker/identity.json", &identity); err != nil {
		return out, err
	}
	if identity != journal.After {
		return out, fmt.Errorf("retained worker identity mismatch")
	}
	var original recoveryOriginalManifest
	if err = read("stage/"+originalManifestFile, &original); err != nil {
		return out, err
	}
	if original.Version != 1 || original.Binding != journal.binding() || original.ConfigHash != review.ConfigHash ||
		original.State != "ready" || !supportedSnapshotSchema(original.Schema) || original.hash() != review.OriginalSourceHash ||
		!safefs.DigestPattern.MatchString(original.DatabaseHash) || original.DatabaseBytes <= 0 || original.DatabaseBytes > c.MaxDatabaseBytes {
		return out, fmt.Errorf("retained original-source authority mismatch")
	}
	var reconciliation reconciliationManifest
	if err = read("stage/reconciliation.json", &reconciliation); err != nil {
		return out, err
	}
	want := reconciliationManifest{1, journal.binding(), review.ConfigHash, journal.CandidateHash, "ready"}
	if reconciliation != want {
		return out, fmt.Errorf("retained reconciliation intent mismatch")
	}
	if err = verifyRetainedOwnership(ctx, c, journal, read); err != nil {
		return out, err
	}
	inputs := []struct {
		Name, Hash string
		Bytes      int64
	}{
		{"source.db", journal.Snapshot.DatabaseHash, journal.Snapshot.DatabaseBytes},
		{"candidate.db", journal.CandidateHash, 0},
		{originalArchiveFile, original.DatabaseHash, original.DatabaseBytes},
	}
	for _, input := range inputs {
		if err = verifyRetainedDatabase(ctx, stage, input.Name, input.Hash, input.Bytes, c.MaxDatabaseBytes); err != nil {
			return out, err
		}
	}
	if err = absentOriginalFile(d, checkpointTemporary); err != nil {
		return out, err
	}
	if err = originalSidecars(d, checkpointTemporary, true); err != nil {
		return out, err
	}
	if err = verifyRetainedDatabase(ctx, d, checkpointDatabase, cp.DatabaseHash, cp.DatabaseBytes, c.MaxDatabaseBytes); err != nil {
		return out, err
	}
	if err = verifyCheckpointProjection(ctx, d, checkpointDatabase, cp.ProjectionHash, c.MaxDatabaseBytes); err != nil {
		return out, err
	}
	// Snapshot/original SQL has its old identity. Candidate/projection SQL has
	// the rotated one. Reading these immutably must never checkpoint a WAL.
	if err = checkRetainedSnapshot(ctx, stage, "source.db", journal.Snapshot, c); err != nil {
		return out, err
	}
	if err = checkRetainedSnapshot(ctx, stage, originalArchiveFile, journal.originalSnapshot(original.Schema), c); err != nil {
		return out, err
	}
	if err = checkRetainedRecoverySQL(ctx, stage, "candidate.db", journal, "", ""); err != nil {
		return out, err
	}
	want.State = "initializing"
	if err = checkRetainedRecoverySQL(ctx, d, checkpointDatabase, journal, protocol.Hash(want), original.hash()); err != nil {
		return out, err
	}
	// Recheck exact bytes after SQL and typed evidence reads as well. A pinned
	// initial checksum alone cannot detect a file changed during those reads.
	for _, input := range inputs {
		if err = verifyRetainedDatabase(ctx, stage, input.Name, input.Hash, input.Bytes, c.MaxDatabaseBytes); err != nil {
			return out, err
		}
	}
	if err = verifyRetainedDatabase(ctx, d, checkpointDatabase, cp.DatabaseHash, cp.DatabaseBytes, c.MaxDatabaseBytes); err != nil {
		return out, err
	}
	checked, err := readCheckpointEvidence(ctx, d, cp, review, c.MaxDatabaseBytes)
	if err != nil {
		return out, err
	}
	if checked.ArchiveHash != evidence.ArchiveHash {
		return out, fmt.Errorf("retained proof evidence changed during SQL checks")
	}
	out = RecoveryCheckpointProof{OriginalRuntimeHash: journal.RuntimeHash, AncestorHash: journal.Snapshot.RecoveryHash, Version: 1, Operation: operation, Snapshot: journal.Snapshot.ID,
		Before: journal.Before, After: journal.After, ConfigHash: review.ConfigHash, ReviewHash: reviewHash,
		CheckpointHash: checkpointHash, EvidenceArchiveHash: evidence.ArchiveHash, OriginalSourceHash: original.hash(),
		SourceDatabaseHash: journal.Snapshot.DatabaseHash, CandidateDatabaseHash: journal.CandidateHash,
		OriginalDatabaseHash: original.DatabaseHash, ProjectionDatabaseHash: cp.DatabaseHash, PendingStages: review.PendingStages}
	out.ProofHash = protocol.Hash(out)
	return out, ctx.Err()
}

func verifyRetainedDatabase(ctx context.Context, d *safefs.Directory, name, hash string, size, limit int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := originalSidecars(d, name, true); err != nil {
		return err
	}
	f, err := snapshotFile(d, name, limit)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return err
	}
	if size != 0 && i.Size() != size {
		return fmt.Errorf("retained database length changed: %s", name)
	}
	got, err := boundedEvidenceDigest(f, i.Size())
	if err != nil {
		return err
	}
	if got != hash {
		return fmt.Errorf("retained database checksum changed: %s", name)
	}
	return ctx.Err()
}

func checkRetainedSnapshot(ctx context.Context, d *safefs.Directory, name string, manifest SnapshotManifest, c Config) (err error) {
	db, err := immutableOriginalDB(d, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	return verifyRetainedSnapshotSQL(ctx, d, db, manifest, c)
}

func checkRetainedRecoverySQL(ctx context.Context, d *safefs.Directory, name string, journal snapshotRecovery, reconciliation, original string) (err error) {
	db, err := immutableOriginalDB(d, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var integrity string
	var schema int
	if err = tx.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil {
		return err
	}
	if integrity != "ok" || schema != journal.Snapshot.Schema {
		return fmt.Errorf("retained recovery integrity/schema mismatch")
	}
	for key, want := range map[string]string{"identity": protocol.Hash(journal.After), "recovery": protocol.Hash(journal.binding())} {
		var raw string
		if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&raw); err != nil {
			return err
		}
		var got string
		if key == "identity" {
			var v protocol.Identity
			if err = protocol.Decode([]byte(raw), &v, 4096); err != nil {
				return err
			}
			got = protocol.Hash(v)
		} else {
			var v recoveryBinding
			if err = protocol.Decode([]byte(raw), &v, 4096); err != nil {
				return err
			}
			got = protocol.Hash(v)
		}
		if got != want {
			return fmt.Errorf("retained recovery SQL binding changed")
		}
	}
	for key, want := range map[string]string{"reconciliation": reconciliation, originalArchiveKey: original} {
		if want == "" {
			continue
		}
		var got string
		if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&got); err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("retained recovery SQL acknowledgment changed")
		}
	}
	return tx.Commit()
}

func verifyRetainedOwnership(ctx context.Context, c Config, journal snapshotRecovery, read func(string, any) error) error {
	for _, enrollment := range c.Producers {
		p := enrollment.Producer
		var owner ownership
		var intent ownershipIntent
		if err := read("worker/"+ownershipName(p)+".json", &owner); err != nil {
			return err
		}
		if err := read("worker/"+ownershipName(p)+".journal.json", &intent); err != nil {
			return err
		}
		if !validOwnership(owner) || owner.Producer != p || owner.Revision == 0 || owner.Revision > maxRecoveryEvidenceFiles ||
			(owner.Identity != journal.Before && owner.Identity != journal.After) {
			return fmt.Errorf("invalid retained ownership head")
		}
		after, err := applyOwnershipTransition(intent.Before, intent.Transition)
		if err != nil {
			return err
		}
		if !sameOwnership(owner, after) {
			return fmt.Errorf("retained ownership intent differs from head")
		}
		var current ownership
		for revision := uint64(1); revision <= owner.Revision; revision++ {
			if err = ctx.Err(); err != nil {
				return err
			}
			var event ownershipTransition
			if err = read("worker/"+ownershipHistoryPath(p, revision), &event); err != nil {
				return err
			}
			if revision == 1 {
				if event.Identity.Worker != journal.Before.Worker {
					return fmt.Errorf("retained ownership history changed worker")
				}
				current = emptyOwnership(event.Identity, p)
				if event.Operation == "adopt" && event.Baseline != nil {
					current = *event.Baseline
				}
			}
			current, err = applyOwnershipTransition(current, event)
			if err != nil {
				return err
			}
		}
		if !sameOwnership(current, owner) {
			return fmt.Errorf("retained ownership history differs from head")
		}
	}
	return nil
}
