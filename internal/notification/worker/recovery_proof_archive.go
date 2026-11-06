package worker

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"syscall"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

const snapshotProofDirectory = "recovery-proof"

type recoveryArchiveBudget struct {
	bytes          int64
	files          int
	limit, reserve int64
}

// Artifacts retain independent file copies, never symlinks or hardlinks into
// live state. Snapshot completion is published only after the copy verifies.
func copyRecoveryProofArchive(ctx context.Context, source, target *safefs.Directory, c Config, e snapshotRecoveryEvidence) error {
	if c.MaxDatabaseBytes > math.MaxInt64/32 {
		return fmt.Errorf("recovery proof archive budget overflows")
	}
	budget := recoveryArchiveBudget{limit: 32 * c.MaxDatabaseBytes, reserve: c.MinFreeBytes}
	if err := target.Mkdir(snapshotProofDirectory); err != nil {
		return err
	}
	dest, err := target.Sub(snapshotProofDirectory)
	if err != nil {
		return err
	}
	defer dest.Close()
	for _, name := range []string{"worker.lock", "identity.json", "recovery.json", publicationCommitIntent, publicationCommitSeal, publicationCommitComplete, publicationExchange, recoveryVerificationIntent, recoveryVerificationComplete} {
		if err = copyRecoveryProofFile(ctx, source, dest, name, &budget); err != nil {
			return err
		}
	}
	if err = validateSnapshotRuntimeInventory(e.RuntimeFiles, c); err != nil {
		return err
	}
	for _, file := range e.RuntimeFiles {
		if err = evidenceFileMatches(source, file.Path, file); err != nil {
			return err
		}
		if parent := path.Dir(file.Path); parent != "." {
			if err = dest.Mkdir(parent); err != nil {
				return err
			}
		}
		if err = copyRecoveryProofFile(ctx, source, dest, file.Path, &budget); err != nil {
			return err
		}
	}
	relative := "recovery/" + e.Proof.Operation
	from, err := source.Sub(relative)
	if err != nil {
		return err
	}
	defer from.Close()
	if err = dest.Mkdir(relative); err != nil {
		return err
	}
	to, err := dest.Sub(relative)
	if err != nil {
		return err
	}
	defer to.Close()
	if err = copyRecoveryProofTree(ctx, from, to, &budget, 0); err != nil {
		return err
	}
	return verifySnapshotProofArchive(ctx, target, c, e)
}

func copyRecoveryProofTree(ctx context.Context, source, target *safefs.Directory, budget *recoveryArchiveBudget, depth int) error {
	if depth > 128 {
		return fmt.Errorf("recovery proof archive nesting exceeds limit")
	}
	for {
		entries, err := source.File().ReadDir(128)
		for _, entry := range entries {
			if e := ctx.Err(); e != nil {
				return e
			}
			if entry.IsDir() {
				budget.files++
				if budget.files > 16384 {
					return fmt.Errorf("recovery proof archive entry limit exceeded")
				}
				from, e := source.Sub(entry.Name())
				if e != nil {
					return e
				}
				if e = target.Mkdir(entry.Name()); e != nil {
					from.Close()
					return e
				}
				to, e := target.Sub(entry.Name())
				if e != nil {
					from.Close()
					return e
				}
				e = copyRecoveryProofTree(ctx, from, to, budget, depth+1)
				from.Close()
				to.Close()
				if e != nil {
					return e
				}
			} else if !entry.Type().IsRegular() {
				return fmt.Errorf("recovery proof archive contains a nonregular entry")
			} else {
				if e := copyRecoveryProofFile(ctx, source, target, entry.Name(), budget); e != nil {
					return e
				}
			}
		}
		if err == io.EOF {
			return target.File().Sync()
		}
		if err != nil {
			return err
		}
	}
}

func copyRecoveryProofFile(ctx context.Context, source, target *safefs.Directory, name string, budget *recoveryArchiveBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in, err := source.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || info.Size() < 0 || info.Size() > budget.limit-budget.bytes {
		return fmt.Errorf("unsafe or oversized recovery proof file")
	}
	budget.files++
	if budget.files > 16384 {
		return fmt.Errorf("recovery proof archive entry limit exceeded")
	}
	budget.bytes += info.Size()
	if info.Size() > math.MaxInt64-budget.reserve {
		return fmt.Errorf("recovery proof capacity overflow")
	}
	if err = target.Capacity(info.Size() + budget.reserve); err != nil {
		return err
	}
	before, err := safefs.DigestFile(in)
	if err != nil {
		return err
	}
	if _, err = in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := target.Open(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(&recoveryArchiveReader{ctx, in}, info.Size()+1))
	if err == nil && n != info.Size() {
		err = fmt.Errorf("recovery proof source length changed")
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	copied, err := target.Open(name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	digest, err := safefs.DigestFile(copied)
	copied.Close()
	if err != nil {
		return err
	}
	if digest != before {
		return fmt.Errorf("recovery proof changed during copy")
	}
	return target.File().Sync()
}

type recoveryArchiveReader struct {
	ctx   context.Context
	input io.Reader
}

func (r *recoveryArchiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.input.Read(p)
}

type recoveryProofDepthKey struct{}

// Each level already rechecks its immutable files. Walk its parent once after
// checking that level, avoiding exponential recursion through the same source
// and original SQL proofs. No verification result is cached across calls.
type recoveryProofWalkKey struct{}

func verifySnapshotProofArchive(ctx context.Context, target *safefs.Directory, c Config, e snapshotRecoveryEvidence) error {
	depth, _ := ctx.Value(recoveryProofDepthKey{}).(int)
	if depth >= 32 {
		return fmt.Errorf("recovery ancestor chain exceeds limit")
	}
	ctx = context.WithValue(ctx, recoveryProofDepthKey{}, depth+1)
	d, err := target.Sub(snapshotProofDirectory)
	if err != nil {
		return err
	}
	defer d.Close()
	lease, err := d.Lock("worker.lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	publication, proof, err := verifyCompletedPublication(context.WithValue(ctx, recoveryProofWalkKey{}, true), d, lease, c, e.Publication.CommitHash)
	if err != nil {
		return err
	}
	if publication != e.Publication || proof != e.Proof {
		return fmt.Errorf("snapshot ancestor proof changed")
	}
	for _, name := range []string{recoveryVerificationIntent, recoveryVerificationComplete} {
		ok, err := verificationRecord(d, name, e.Adoption)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("snapshot ancestor adoption missing")
		}
	}
	if err = validateSnapshotRuntimeInventory(e.RuntimeFiles, c); err != nil {
		return err
	}
	for _, file := range e.RuntimeFiles {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = evidenceFileMatches(d, file.Path, file); err != nil {
			return err
		}
	}
	if proof.AncestorHash != "" {
		var j snapshotRecovery
		if err = readCheckpointJSON(d, "recovery.json", &j); err != nil {
			return err
		}
		stage, err := d.Sub("recovery/" + proof.Operation)
		if err != nil {
			return err
		}
		defer stage.Close()
		parent, err := readSnapshotRecovery(stage, j.Snapshot, c)
		if err != nil {
			return err
		}
		if parent == nil || j.Snapshot.RecoveryHash != proof.AncestorHash {
			return fmt.Errorf("ancestor chain link changed")
		}
		return verifySnapshotProofArchive(ctx, stage, c, *parent)
	}
	return nil
}
