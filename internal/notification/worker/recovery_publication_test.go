package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func publicationFixture(t *testing.T) (*recoveryReconciler, RecoveryStatus, RecoveryCheckpoint, RecoveryCheckpointProof) {
	t.Helper()
	r, review, cp := retainedProofFixture(t)
	proof, err := verifyFixtureProof(context.Background(), r, review, cp, nil)
	must(t, err)
	return r, review, cp, proof
}

func prepareFixturePublication(ctx context.Context, r *recoveryReconciler, review RecoveryStatus, cp RecoveryCheckpoint, proof RecoveryCheckpointProof) (RecoveryPublicationImage, error) {
	return r.preparePublication(ctx, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
}

func TestRecoveryPublicationRequiresCurrentPinsBeforeWriting(t *testing.T) {
	for _, kind := range []string{"review", "checkpoint", "proof", "stale"} {
		t.Run(kind, func(t *testing.T) {
			r, review, cp, proof := publicationFixture(t)
			original := review
			switch kind {
			case "review":
				review.ReviewHash = strings.Repeat("a", 64)
			case "checkpoint":
				cp.CheckpointHash = strings.Repeat("a", 64)
			case "proof":
				proof.ProofHash = strings.Repeat("a", 64)
			case "stale":
				_, err := r.db.Exec("UPDATE messages SET state='queued'")
				must(t, err)
			}
			if _, err := prepareFixturePublication(context.Background(), r, review, cp, proof); err == nil {
				t.Fatal("unreviewed input accepted")
			}
			if _, err := os.Stat(checkpointPath(r, original, "publication-intent.json")); !os.IsNotExist(err) {
				t.Fatal("invalid pins created an intent", err)
			}
		})
	}
}

func TestRecoveryPublicationNeverOverwritesAnchor(t *testing.T) {
	r, review, cp, proof := publicationFixture(t)
	_, err := prepareFixturePublication(context.Background(), r, review, cp, proof)
	must(t, err)
	d, err := r.stage.Sub("checkpoints/" + review.ReviewHash)
	must(t, err)
	defer d.Close()
	must(t, d.Rename(publicationDatabase, d, publicationCopy))
	before, err := os.ReadFile(checkpointPath(r, review, publicationCopy))
	must(t, err)
	changed := proof
	changed.Operation = "n-another"
	if err = addPublicationAnchor(context.Background(), d, recoveryPublicationAnchor{1, "fenced", changed}, r.config.MaxDatabaseBytes, nil); err == nil || !strings.Contains(err.Error(), "ancestor reconciliation") {
		t.Fatal("overwrote an existing anchor", err)
	}
	after, err := os.ReadFile(checkpointPath(r, review, publicationCopy))
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("rejected anchor changed image bytes")
	}
}

func TestRecoveryPublicationRejectsReplacedCopyBeforeWrite(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "bytes", "sidecar"} {
		t.Run(kind, func(t *testing.T) {
			r, review, cp, proof := publicationFixture(t)
			canonicalPath := filepath.Join(r.dir.File().Name(), "state.db")
			before, err := os.ReadFile(canonicalPath)
			must(t, err)
			r.fault = func(at string) error {
				if at != "publication_copied" {
					return nil
				}
				path := checkpointPath(r, review, publicationCopy)
				switch kind {
				case "symlink", "hardlink":
					if err := os.Remove(path); err != nil {
						return err
					}
					if kind == "symlink" {
						return os.Symlink(canonicalPath, path)
					}
					return os.Link(canonicalPath, path)
				case "bytes":
					return os.WriteFile(path, []byte("changed"), 0600)
				default:
					return os.WriteFile(path+"-wal", nil, 0600)
				}
			}
			if _, err := prepareFixturePublication(context.Background(), r, review, cp, proof); err == nil {
				t.Fatal("replaced temporary accepted")
			}
			after, err := os.ReadFile(canonicalPath)
			must(t, err)
			if string(before) != string(after) {
				t.Fatal("temporary redirected a write to canonical state")
			}
			if _, err := os.Stat(checkpointPath(r, review, "publication-sealed.json")); !os.IsNotExist(err) {
				t.Fatal("replaced temporary sealed", err)
			}
		})
	}
}

func TestRecoveryPublicationPreparationPreservesSourceAndFences(t *testing.T) {
	r, review, cp, proof := publicationFixture(t)
	ctx := context.Background()
	canonical, err := os.ReadFile(filepath.Join(r.dir.File().Name(), "state.db"))
	must(t, err)
	one, err := prepareFixturePublication(ctx, r, review, cp, proof)
	must(t, err)
	two, err := prepareFixturePublication(ctx, r, review, cp, proof)
	must(t, err)
	if one != two || one.DatabaseHash == cp.DatabaseHash || one.ProofHash != proof.ProofHash || one.PublicationAllowed || one.ExecutionEnabled || one.DeliveryEnabled {
		t.Fatal(one, two)
	}
	hash := one.ImageHash
	one.ImageHash = ""
	if protocol.Hash(one) != hash {
		t.Fatal("image hash does not cover result")
	}
	after, err := r.reviewRecovery(ctx)
	must(t, err)
	if review.ReviewHash != after.ReviewHash {
		t.Fatal("publication preparation changed recovery state")
	}
	data, err := os.ReadFile(filepath.Join(r.dir.File().Name(), "state.db"))
	must(t, err)
	if string(data) != string(canonical) {
		t.Fatal("preparation replaced canonical database")
	}
	d, err := r.stage.Sub("checkpoints/" + review.ReviewHash)
	must(t, err)
	defer d.Close()
	db, err := immutableOriginalDB(d, publicationDatabase)
	must(t, err)
	var raw string
	must(t, db.QueryRow("SELECT value FROM metadata WHERE key=?", publicationAnchorKey).Scan(&raw))
	var anchor recoveryPublicationAnchor
	must(t, protocol.Decode([]byte(raw), &anchor, safefs.MaxRecordBytes))
	if anchor.Proof != proof || anchor.Mode != "fenced" {
		t.Fatal(anchor)
	}
	must(t, db.Close())
	path, c := r.dir.File().Name(), r.config
	must(t, r.Close())
	if store, e := Open(path, c, false); e == nil {
		store.Close()
		t.Fatal("prepared image activated worker")
	}
}

func TestRecoveryPublicationInterruptedPreparation(t *testing.T) {
	for _, point := range []string{"publication_intent", "publication_copied", "publication_anchor_transaction", "publication_anchor_commit", "publication_copy", "publication_sealed", "publication_image", "publication_ready"} {
		t.Run(point, func(t *testing.T) {
			r, review, cp, proof := publicationFixture(t)
			r.fault = func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := prepareFixturePublication(context.Background(), r, review, cp, proof); err == nil {
				t.Fatal("fault missed")
			}
			r.fault = nil
			one, err := prepareFixturePublication(context.Background(), r, review, cp, proof)
			must(t, err)
			two, err := prepareFixturePublication(context.Background(), r, review, cp, proof)
			must(t, err)
			if one != two {
				t.Fatal("resume changed publication image")
			}
		})
	}
}

func TestRecoveryPublicationRejectsDamage(t *testing.T) {
	for _, kind := range []string{"image", "lost-image", "intent", "seal", "lost-seal", "completion", "sidecar", "copy", "symlink", "hardlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			r, review, cp, proof := publicationFixture(t)
			_, err := prepareFixturePublication(context.Background(), r, review, cp, proof)
			must(t, err)
			dir := filepath.Dir(checkpointPath(r, review, "checkpoint.json"))
			path := filepath.Join(dir, publicationDatabase)
			switch kind {
			case "image":
				must(t, os.WriteFile(path, []byte("bad"), 0600))
			case "lost-image":
				must(t, os.Remove(path))
			case "intent", "seal", "completion":
				name := map[string]string{"intent": "publication-intent.json", "seal": "publication-sealed.json", "completion": "publication-image.json"}[kind]
				must(t, os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600))
			case "lost-seal":
				must(t, os.Remove(filepath.Join(dir, "publication-sealed.json")))
			case "sidecar":
				must(t, os.WriteFile(path+"-wal", nil, 0600))
			case "copy":
				must(t, os.WriteFile(filepath.Join(dir, publicationCopy), nil, 0600))
			case "permissions":
				must(t, os.Chmod(path, 0644))
			case "symlink", "hardlink":
				other := filepath.Join(t.TempDir(), "image")
				must(t, os.Rename(path, other))
				if kind == "symlink" {
					must(t, os.Symlink(other, path))
				} else {
					must(t, os.Link(other, path))
				}
			}
			if _, err = prepareFixturePublication(context.Background(), r, review, cp, proof); err == nil {
				t.Fatal("damaged image repaired or accepted")
			}
			if kind == "lost-image" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("sealed image regenerated")
				}
			}
		})
	}
}

func TestRecoveryPublicationRefusesChangesDuringPreparation(t *testing.T) {
	for _, kind := range []string{"sql", "evidence", "lease", "intent", "image", "seal", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			r, review, cp, proof := publicationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.fault = func(at string) error {
				if at != "publication_ready" {
					return nil
				}
				switch kind {
				case "sql":
					_, e := r.db.Exec("INSERT INTO metadata VALUES('late-publication-change','yes')")
					return e
				case "evidence":
					return r.stage.WriteJSON("late-proof.json", map[string]bool{"changed": true}, false)
				case "lease":
					path := filepath.Join(r.dir.File().Name(), "worker.lock")
					if e := os.Rename(path, path+".old"); e != nil {
						return e
					}
					return os.WriteFile(path, nil, 0600)
				case "intent":
					return os.WriteFile(checkpointPath(r, review, "publication-intent.json"), []byte("{}"), 0600)
				case "image":
					return os.WriteFile(checkpointPath(r, review, publicationDatabase), []byte("bad"), 0600)
				case "seal":
					return os.WriteFile(checkpointPath(r, review, "publication-sealed.json"), []byte("{}"), 0600)
				default:
					cancel()
					return nil
				}
			}
			if _, err := prepareFixturePublication(ctx, r, review, cp, proof); err == nil {
				t.Fatal("changed preparation accepted")
			}
		})
	}
}

func TestRecoveryPublicationImageOnlyAddsAnchor(t *testing.T) {
	for _, mutation := range []string{"null-metadata", "message", "schema", "anchor"} {
		t.Run(mutation, func(t *testing.T) {
			r, review, cp, proof := publicationFixture(t)
			_, err := prepareFixturePublication(context.Background(), r, review, cp, proof)
			must(t, err)
			d, err := r.stage.Sub("checkpoints/" + review.ReviewHash)
			must(t, err)
			defer d.Close()
			db, err := snapshotDB(d, publicationDatabase, false)
			must(t, err)
			switch mutation {
			case "null-metadata":
				_, err = db.Exec("INSERT INTO metadata(key,value) VALUES(NULL,'must-not-be-hidden')")
			case "message":
				_, err = db.Exec("UPDATE messages SET state='queued'")
			case "schema":
				_, err = db.Exec("PRAGMA application_id=21")
			case "anchor":
				_, err = db.Exec("UPDATE metadata SET value='{}' WHERE key=?", publicationAnchorKey)
			}
			must(t, err)
			must(t, db.Close())
			if _, err = inspectPublicationImage(context.Background(), d, publicationDatabase, recoveryPublicationAnchor{1, "fenced", proof}, review, r.config.MaxDatabaseBytes); err == nil {
				t.Fatal("image changed data beyond its anchor")
			}
		})
	}
}
