package worker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func retainedProofFixture(t *testing.T) (*recoveryReconciler, RecoveryStatus, RecoveryCheckpoint) {
	t.Helper()
	r, review := checkpointFixture(t)
	cp, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
	must(t, err)
	return r, review, cp
}

func verifyFixtureProof(ctx context.Context, r *recoveryReconciler, review RecoveryStatus, cp RecoveryCheckpoint, fault func(string) error) (RecoveryCheckpointProof, error) {
	return verifyRetainedCheckpoint(ctx, r.dir, r.lease, r.config, r.journal.Operation, review.ReviewHash, cp.CheckpointHash, fault)
}

func retainedTreeHash(t *testing.T, path string) string {
	t.Helper()
	type file struct {
		Mode fs.FileMode
		Hash string
	}
	entries := map[string]file{}
	must(t, filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		i, err := d.Info()
		if err != nil {
			return err
		}
		entry := file{Mode: i.Mode()}
		if !d.IsDir() {
			data, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			entry.Hash = protocol.Hash(data)
		}
		entries[p] = entry
		return nil
	}))
	return protocol.Hash(entries)
}

func TestVerifyRecoveryCheckpointReadOnlyReplay(t *testing.T) {
	r, review, cp := retainedProofFixture(t)
	path, c, op := r.dir.File().Name(), r.config, r.journal.Operation
	must(t, r.Close())
	before := retainedTreeHash(t, path)
	one, err := VerifyRecoveryCheckpoint(context.Background(), path, c, op, review.ReviewHash, cp.CheckpointHash)
	must(t, err)
	two, err := VerifyRecoveryCheckpoint(context.Background(), path, c, op, review.ReviewHash, cp.CheckpointHash)
	must(t, err)
	if one != two || one.CheckpointHash != cp.CheckpointHash || one.PendingStages != review.PendingStages || one.PublicationAllowed || one.DeliveryEnabled || one.ExecutionEnabled {
		t.Fatal(one, two)
	}
	hash := one.ProofHash
	one.ProofHash = ""
	if hash != protocol.Hash(one) {
		t.Fatal("proof hash does not cover result")
	}
	if retainedTreeHash(t, path) != before {
		t.Fatal("verification changed retained or canonical state")
	}
	if s, e := Open(path, c, false); e == nil {
		s.Close()
		t.Fatal("verification activated worker")
	}
}

func TestVerifyRecoveryCheckpointIndependentOfCanonicalState(t *testing.T) {
	r, review, cp := retainedProofFixture(t)
	ctx := context.Background()
	want, err := verifyFixtureProof(ctx, r, review, cp, nil)
	must(t, err)
	_, err = r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	current, err := r.reviewRecovery(ctx)
	must(t, err)
	_, err = r.checkpointRecovery(ctx, current.ReviewHash)
	must(t, err)
	got, err := verifyFixtureProof(ctx, r, review, cp, nil)
	must(t, err)
	if want != got {
		t.Fatal("new ownership affected historical proof")
	}
	path, c, op := r.dir.File().Name(), r.config, r.journal.Operation
	stage := r.stage.File().Name()
	must(t, r.Close())
	// Simulate the canonical and mutable stage files having moved to another
	// operation. No old authority may be read from these paths or regenerated.
	for _, name := range []string{"state.db", "identity.json", "recovery.json", ownershipName(producer) + ".json", ownershipName(producer) + ".journal.json"} {
		must(t, os.WriteFile(filepath.Join(path, name), []byte("new canonical state"), 0600))
	}
	for _, name := range []string{"reconciled.db", "reconciliation.json", originalManifestFile} {
		must(t, os.Remove(filepath.Join(stage, name)))
	}
	got, err = VerifyRecoveryCheckpoint(ctx, path, c, op, review.ReviewHash, cp.CheckpointHash)
	must(t, err)
	if want != got {
		t.Fatal("verifier followed mutable canonical files")
	}
}

func TestVerifyRecoveryCheckpointRejectsDamagedRetainedInputs(t *testing.T) {
	for _, kind := range []string{"source", "candidate", "original", "projection", "source-wal", "candidate-shm", "original-journal", "projection-wal", "missing-evidence", "missing-seal", "checkpoint", "intent", "symlink", "hardlink", "permissions", "directory-permissions"} {
		t.Run(kind, func(t *testing.T) {
			r, review, cp := retainedProofFixture(t)
			file := filepath.Join(r.stage.File().Name(), "source.db")
			checkpoint := filepath.Dir(checkpointPath(r, review, "checkpoint.json"))
			switch kind {
			case "source":
				must(t, os.WriteFile(file, []byte("bad"), 0600))
			case "candidate", "original":
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), kind+".db"), []byte("bad"), 0600))
			case "projection":
				must(t, os.WriteFile(filepath.Join(checkpoint, checkpointDatabase), []byte("bad"), 0600))
			case "source-wal":
				must(t, os.WriteFile(file+"-wal", nil, 0600))
			case "candidate-shm":
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), "candidate.db-shm"), nil, 0600))
			case "original-journal":
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), "original.db-journal"), nil, 0600))
			case "projection-wal":
				must(t, os.WriteFile(filepath.Join(checkpoint, checkpointDatabase+"-wal"), nil, 0600))
			case "missing-evidence":
				must(t, os.Remove(filepath.Join(checkpoint, "evidence", "00000.json")))
			case "missing-seal":
				must(t, os.Remove(filepath.Join(checkpoint, "sealed.json")))
			case "checkpoint", "intent":
				must(t, os.WriteFile(filepath.Join(checkpoint, kind+".json"), []byte("{}"), 0600))
			case "symlink", "hardlink":
				other := filepath.Join(t.TempDir(), "database")
				must(t, os.Rename(file, other))
				if kind == "symlink" {
					must(t, os.Symlink(other, file))
				} else {
					must(t, os.Link(other, file))
				}
			case "permissions":
				must(t, os.Chmod(file, 0644))
			case "directory-permissions":
				must(t, os.Chmod(checkpoint, 0755))
			}
			if _, err := verifyFixtureProof(context.Background(), r, review, cp, nil); err == nil {
				t.Fatal("damaged retained input accepted")
			}
		})
	}
}

func TestVerifyRecoveryCheckpointRejectsChangesDuringRead(t *testing.T) {
	for _, kind := range []string{"source", "projection", "evidence", "checkpoint", "lease", "directory", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			r, review, cp := retainedProofFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fault := func(at string) error {
				if at != "recovery_checkpoint_verified" {
					return nil
				}
				path := filepath.Dir(checkpointPath(r, review, "checkpoint.json"))
				switch kind {
				case "source":
					return os.WriteFile(filepath.Join(r.stage.File().Name(), "source.db"), []byte("bad"), 0600)
				case "projection":
					return os.WriteFile(filepath.Join(path, checkpointDatabase), []byte("bad"), 0600)
				case "evidence":
					return os.WriteFile(filepath.Join(path, "evidence", "00000.json"), []byte("{}"), 0600)
				case "checkpoint":
					return os.WriteFile(filepath.Join(path, "checkpoint.json"), []byte("{}"), 0600)
				case "lease":
					lock := filepath.Join(r.dir.File().Name(), "worker.lock")
					if err := os.Rename(lock, lock+".old"); err != nil {
						return err
					}
					return os.WriteFile(lock, nil, 0600)
				case "directory":
					if err := os.Rename(path, path+".old"); err != nil {
						return err
					}
					return os.Mkdir(path, 0700)
				default:
					cancel()
					return nil
				}
			}
			if _, err := verifyFixtureProof(ctx, r, review, cp, fault); err == nil {
				t.Fatal("changed evidence accepted")
			}
		})
	}
}

func TestVerifyRecoveryCheckpointRequiresExactOfflineReference(t *testing.T) {
	r, review, cp := retainedProofFixture(t)
	path, c, op := r.dir.File().Name(), r.config, r.journal.Operation
	if _, err := VerifyRecoveryCheckpoint(context.Background(), path, c, op, review.ReviewHash, cp.CheckpointHash); err == nil {
		t.Fatal("worker lease bypassed")
	}
	must(t, r.Close())
	for _, kind := range []string{"operation", "review", "checkpoint", "configuration", "cancelled", "missing-state"} {
		t.Run(kind, func(t *testing.T) {
			cc, pp, oo, rr, hh := c, path, op, review.ReviewHash, cp.CheckpointHash
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "operation":
				oo, _ = safefs.NewID("n")
			case "review":
				rr = protocol.Hash("foreign-review")
			case "checkpoint":
				hh = protocol.Hash("foreign-checkpoint")
			case "configuration":
				cc.MinFreeBytes++
			case "cancelled":
				cancel()
			case "missing-state":
				pp = filepath.Join(path, "absent")
			}
			if _, err := VerifyRecoveryCheckpoint(ctx, pp, cc, oo, rr, hh); err == nil {
				t.Fatal("invalid reference accepted")
			}
			if kind == "missing-state" {
				if _, err := os.Stat(pp); !os.IsNotExist(err) {
					t.Fatal("verification initialized state")
				}
			}
		})
	}
}
