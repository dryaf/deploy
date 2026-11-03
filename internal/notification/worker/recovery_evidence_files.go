package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func boundedEvidenceDigest(f *os.File, size int64) (string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("recovery evidence changed length during read")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkEvidenceRecord(d *safefs.Directory, name string, want recoveryEvidenceArchive) (bool, error) {
	var v recoveryEvidenceArchive
	err := readCheckpointJSON(d, name, &v)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if protocol.Hash(v) != protocol.Hash(want) {
		return false, fmt.Errorf("checkpoint evidence record changed: %s", name)
	}
	return true, nil
}

func evidenceFileMatches(d *safefs.Directory, name string, want recoveryEvidenceFile) error {
	f, err := snapshotFile(d, name, want.Bytes)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return err
	}
	hash, err := boundedEvidenceDigest(f, want.Bytes)
	if err != nil {
		return err
	}
	if i.Size() != want.Bytes || hash != want.Hash {
		return fmt.Errorf("checkpoint evidence bytes changed: %s", want.Path)
	}
	return nil
}

func checkEvidenceDirectory(d *safefs.Directory, count int, allowCopy bool) error {
	scan, err := d.Sub(".")
	if err != nil {
		return err
	}
	defer scan.Close()
	names, err := scan.File().Readdirnames(count + 2)
	if err != nil && err != io.EOF {
		return err
	}
	allowed := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		allowed[evidenceCopyName(i)] = true
	}
	for _, name := range names {
		if !allowed[name] && !(allowCopy && name == evidenceCopyFile) {
			return fmt.Errorf("unrecognized checkpoint evidence artifact")
		}
	}
	return nil
}

func verifyEvidenceCopies(ctx context.Context, d *safefs.Directory, files []recoveryEvidenceFile) error {
	if err := privateCheckpointDirectory(d); err != nil {
		return err
	}
	if err := checkEvidenceDirectory(d, len(files), false); err != nil {
		return err
	}
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := evidenceFileMatches(d, evidenceCopyName(i), file); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func sameEvidenceDirectory(parent, copies *safefs.Directory) error {
	current, err := parent.Sub("evidence")
	if err != nil {
		return err
	}
	defer current.Close()
	if err = privateCheckpointDirectory(current); err != nil {
		return err
	}
	a, err := current.File().Stat()
	if err != nil {
		return err
	}
	b, err := copies.File().Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return fmt.Errorf("checkpoint evidence directory replaced")
	}
	return nil
}

// Validate retained evidence without consulting live canonical files. A future
// ancestor reader can use this after ownership/administrative heads have moved;
// this proves retained bytes, not publication or freshness of the old review.
func readCheckpointEvidence(ctx context.Context, d *safefs.Directory, checkpoint RecoveryCheckpoint, review RecoveryStatus, limit int64) (v recoveryEvidenceArchive, err error) {
	// Callers supply the independently pinned checkpoint/review. Reject a
	// changed struct retaining an old digest before trusting its evidence hash.
	cpHash, reviewHash := checkpoint.CheckpointHash, review.ReviewHash
	cpCopy, reviewCopy := checkpoint, review
	cpCopy.CheckpointHash, reviewCopy.ReviewHash = "", ""
	if checkpoint.Version != 1 || review.Version != 1 || !safefs.DigestPattern.MatchString(cpHash) ||
		protocol.Hash(cpCopy) != cpHash || protocol.Hash(reviewCopy) != reviewHash ||
		checkpoint.ReviewHash != reviewHash || checkpoint.Operation != review.Operation || checkpoint.ProjectionHash != review.ProjectionHash ||
		checkpoint.PublicationAllowed || checkpoint.DeliveryEnabled || checkpoint.ExecutionEnabled ||
		review.PublicationAllowed || review.DeliveryEnabled || review.ExecutionEnabled {
		return v, fmt.Errorf("invalid retained checkpoint/review authority")
	}
	if err = readCheckpointJSON(d, "evidence.json", &v); err != nil {
		return
	}
	hash := v.ArchiveHash
	v.ArchiveHash = ""
	valid := protocol.Hash(v) == hash
	v.ArchiveHash = hash
	if !valid || v.Version != 1 || v.CheckpointHash != checkpoint.CheckpointHash || v.ReviewHash != checkpoint.ReviewHash ||
		v.ReviewHash != review.ReviewHash || v.EvidenceHash != review.EvidenceHash || v.EvidenceHash != protocol.Hash(v.Files) ||
		len(v.Files) == 0 || len(v.Files) > maxRecoveryEvidenceFiles {
		return v, fmt.Errorf("invalid checkpoint evidence binding")
	}
	previous := ""
	for _, f := range v.Files {
		scope, relative, ok := strings.Cut(f.Path, "/")
		if !ok || (scope != "stage" && scope != "worker") || relative == "" || path.IsAbs(relative) ||
			!utf8.ValidString(f.Path) || strings.ContainsAny(f.Path, "\x00\r\n") ||
			path.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, "../") ||
			f.Path <= previous || f.Bytes <= 0 || f.Bytes > safefs.MaxRecordBytes || f.Bytes > limit || !safefs.DigestPattern.MatchString(f.Hash) {
			return v, fmt.Errorf("invalid checkpoint evidence inventory")
		}
		previous = f.Path
		limit -= f.Bytes
	}
	for _, name := range []string{"evidence-intent.json", "evidence-sealed.json"} {
		if exists, e := checkEvidenceRecord(d, name, v); e != nil {
			return v, e
		} else if !exists {
			return v, fmt.Errorf("retained checkpoint evidence is incomplete")
		}
	}
	copies, err := d.Sub("evidence")
	if err != nil {
		return v, err
	}
	defer copies.Close()
	err = verifyEvidenceCopies(ctx, copies, v.Files)
	return
}
