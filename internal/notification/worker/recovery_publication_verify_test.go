package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func preparedImageFixture(t *testing.T) (*recoveryReconciler, RecoveryStatus, RecoveryCheckpoint, RecoveryCheckpointProof, RecoveryPublicationImage) {
	t.Helper()
	r, review, cp, proof := publicationFixture(t)
	image, err := prepareFixturePublication(context.Background(), r, review, cp, proof)
	must(t, err)
	return r, review, cp, proof, image
}

func verifyImageFixture(ctx context.Context, r *recoveryReconciler, v RecoveryPublicationImage, fault func(string) error) (RecoveryPublicationImage, error) {
	return verifyRetainedPublication(ctx, r.dir, r.lease, r.config, v.Operation, v.ReviewHash, v.CheckpointHash, v.ProofHash, v.ImageHash, fault)
}

func TestVerifyRecoveryPublicationReadOnlyAndHistorical(t *testing.T) {
	r, review, _, _, image := preparedImageFixture(t)
	ctx := context.Background()
	before := retainedTreeHash(t, r.dir.File().Name())
	for i := 0; i < 2; i++ {
		got, err := verifyImageFixture(ctx, r, image, nil)
		must(t, err)
		if got != image || got.PublicationAllowed || got.DeliveryEnabled || got.ExecutionEnabled {
			t.Fatal("verification changed image or enabled runtime", got)
		}
	}
	if retainedTreeHash(t, r.dir.File().Name()) != before {
		t.Fatal("verification modified state")
	}
	_, err := r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	current, err := r.reviewRecovery(ctx)
	must(t, err)
	if current.ReviewHash == review.ReviewHash {
		t.Fatal("fixture did not advance recovery")
	}
	_, err = r.checkpointRecovery(ctx, current.ReviewHash)
	must(t, err)
	got, err := verifyImageFixture(ctx, r, image, nil)
	must(t, err)
	if got != image {
		t.Fatal("new checkpoint invalidated old image")
	}
	path, stage, c := r.dir.File().Name(), r.stage.File().Name(), r.config
	must(t, r.Close())
	for _, name := range []string{"state.db", "identity.json", "recovery.json", ownershipName(producer) + ".json", ownershipName(producer) + ".journal.json"} {
		must(t, os.WriteFile(filepath.Join(path, name), []byte("later canonical state"), 0600))
	}
	for _, name := range []string{"reconciled.db", "reconciliation.json", originalManifestFile} {
		must(t, os.Remove(filepath.Join(stage, name)))
	}
	before = retainedTreeHash(t, path)
	got, err = VerifyRecoveryPublication(ctx, path, c, image.Operation, image.ReviewHash, image.CheckpointHash, image.ProofHash, image.ImageHash)
	must(t, err)
	if got != image || before != retainedTreeHash(t, path) {
		t.Fatal("historical verification depended on or repaired mutable state")
	}
}

func TestVerifyRecoveryPublicationRequiresOfflineExactReference(t *testing.T) {
	r, _, _, _, image := preparedImageFixture(t)
	path, c := r.dir.File().Name(), r.config
	verify := func(ctx context.Context, path string, c Config, v RecoveryPublicationImage) error {
		_, err := VerifyRecoveryPublication(ctx, path, c, v.Operation, v.ReviewHash, v.CheckpointHash, v.ProofHash, v.ImageHash)
		return err
	}
	if err := verify(context.Background(), path, c, image); err == nil {
		t.Fatal("worker lease bypassed")
	}
	must(t, r.Close())
	for _, kind := range []string{"review", "checkpoint", "proof", "image", "malformed", "config", "cancelled", "missing-state", "missing-lock"} {
		t.Run(kind, func(t *testing.T) {
			v, cc, pp := image, c, path
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "review":
				v.ReviewHash = protocol.Hash("wrong")
			case "checkpoint":
				v.CheckpointHash = protocol.Hash("wrong")
			case "proof":
				v.ProofHash = protocol.Hash("wrong")
			case "image":
				v.ImageHash = protocol.Hash("wrong")
			case "malformed":
				v.ImageHash = ""
			case "config":
				cc.MinFreeBytes++
			case "cancelled":
				cancel()
			case "missing-state":
				pp = filepath.Join(path, "absent")
			case "missing-lock":
				must(t, os.Remove(filepath.Join(path, "worker.lock")))
			}
			before := retainedTreeHash(t, path)
			if err := verify(ctx, pp, cc, v); err == nil {
				t.Fatal("invalid or unavailable reference accepted")
			}
			if before != retainedTreeHash(t, path) {
				t.Fatal("failed verification initialized state")
			}
		})
	}
}

func TestVerifyRecoveryPublicationRejectsDamagedEvidence(t *testing.T) {
	for _, kind := range []string{"image", "intent", "seal", "completion", "missing-completion", "missing-image", "sidecar", "temporary", "temporary-sidecar", "checkpoint", "source", "archive", "symlink", "hardlink", "permissions", "permission-claim"} {
		t.Run(kind, func(t *testing.T) {
			r, review, _, _, image := preparedImageFixture(t)
			path := checkpointPath(r, review, publicationDatabase)
			switch kind {
			case "image":
				must(t, os.WriteFile(path, []byte("bad"), 0600))
			case "intent", "seal", "completion":
				name := map[string]string{"intent": "publication-intent.json", "seal": "publication-sealed.json", "completion": "publication-image.json"}[kind]
				must(t, os.WriteFile(checkpointPath(r, review, name), []byte("{}"), 0600))
			case "missing-completion":
				must(t, os.Remove(checkpointPath(r, review, "publication-image.json")))
			case "missing-image":
				must(t, os.Remove(path))
			case "sidecar":
				must(t, os.WriteFile(path+"-wal", nil, 0600))
			case "temporary", "temporary-sidecar":
				name := publicationCopy
				if kind == "temporary-sidecar" {
					name += "-journal"
				}
				must(t, os.WriteFile(checkpointPath(r, review, name), nil, 0600))
			case "checkpoint":
				must(t, os.WriteFile(checkpointPath(r, review, checkpointDatabase), []byte("bad"), 0600))
			case "source":
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), "source.db"), []byte("bad"), 0600))
			case "archive":
				must(t, os.Remove(checkpointPath(r, review, "evidence/00000.json")))
			case "symlink", "hardlink":
				must(t, os.Rename(path, path+".saved"))
				if kind == "symlink" {
					must(t, os.Symlink(path+".saved", path))
				} else {
					must(t, os.Link(path+".saved", path))
				}
			case "permissions":
				must(t, os.Chmod(path, 0644))
			case "permission-claim":
				forged := image
				forged.PublicationAllowed = true
				d, err := r.stage.Sub("checkpoints/" + review.ReviewHash)
				must(t, err)
				must(t, d.WriteJSON("publication-image.json", forged, true))
				must(t, d.WriteJSON("publication-sealed.json", forged, true))
				must(t, d.Close())
			}
			before := retainedTreeHash(t, r.dir.File().Name())
			if _, err := verifyImageFixture(context.Background(), r, image, nil); err == nil {
				t.Fatal("damaged prepared image accepted")
			}
			if before != retainedTreeHash(t, r.dir.File().Name()) {
				t.Fatal("verification repaired damaged preparation")
			}
		})
	}
}

func TestVerifyRecoveryPublicationRejectsChangesDuringRead(t *testing.T) {
	for _, kind := range []string{"image", "completion", "checkpoint-review", "source", "lease", "directory", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			r, review, _, _, image := preparedImageFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fault := func(point string) error {
				if point != "recovery_publication_verified" {
					t.Fatal("unexpected hook", point)
				}
				path := checkpointPath(r, review, publicationDatabase)
				switch kind {
				case "image":
					return os.WriteFile(path, []byte("changed"), 0600)
				case "completion":
					return os.Remove(checkpointPath(r, review, "publication-image.json"))
				case "checkpoint-review":
					return os.WriteFile(checkpointPath(r, review, "intent.json"), []byte("{}"), 0600)
				case "source":
					return os.WriteFile(filepath.Join(r.stage.File().Name(), "source.db"), []byte("changed"), 0600)
				case "lease":
					path = filepath.Join(r.dir.File().Name(), "worker.lock")
					if err := os.Rename(path, path+".old"); err != nil {
						return err
					}
					return os.WriteFile(path, nil, 0600)
				case "directory":
					path = filepath.Dir(path)
					if err := os.Rename(path, path+".old"); err != nil {
						return err
					}
					return os.Mkdir(path, 0700)
				default:
					cancel()
					return nil
				}
			}
			if _, err := verifyImageFixture(ctx, r, image, fault); err == nil {
				t.Fatal("changed retained evidence accepted")
			}
		})
	}
}
