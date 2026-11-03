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

func evidenceFixture(t *testing.T) (*recoveryReconciler, RecoveryStatus, RecoveryCheckpoint, *safefs.Directory, recoveryEvidenceArchive) {
	t.Helper()
	r, review := checkpointFixture(t)
	checkpoint, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
	must(t, err)
	d, err := r.stage.Sub("checkpoints/" + review.ReviewHash)
	must(t, err)
	t.Cleanup(func() { d.Close() })
	v, err := readCheckpointEvidence(context.Background(), d, checkpoint, review, r.config.MaxDatabaseBytes)
	must(t, err)
	return r, review, checkpoint, d, v
}

func TestRecoveryEvidenceRetainsIndependentHistory(t *testing.T) {
	r, review, checkpoint, d, v := evidenceFixture(t)
	seen := map[string]bool{}
	for i, f := range v.Files {
		seen[f.Path] = true
		scope, relative, _ := strings.Cut(f.Path, "/")
		base := r.stage.File().Name()
		if scope == "worker" {
			base = r.dir.File().Name()
		}
		original, err := os.ReadFile(filepath.Join(base, relative))
		must(t, err)
		copy, err := os.ReadFile(filepath.Join(d.File().Name(), "evidence", evidenceCopyName(i)))
		must(t, err)
		if string(original) != string(copy) {
			t.Fatal("evidence bytes changed", f.Path)
		}
	}
	for _, required := range []string{"worker/identity.json", "worker/recovery.json", "stage/original-source.json", "stage/reconciliation.json", "worker/" + ownershipName(producer) + ".json", "worker/" + ownershipName(producer) + ".journal.json", "worker/" + ownershipHistoryPath(producer, 1)} {
		if !seen[required] {
			t.Fatal("evidence not captured", required)
		}
	}
	// A later epoch may change canonical heads. The saved proof reader must
	// still validate the historical bytes without following mutable source paths.
	must(t, r.dir.WriteJSON(ownershipName(producer)+".json", map[string]string{}, true))
	retained, err := readCheckpointEvidence(context.Background(), d, checkpoint, review, r.config.MaxDatabaseBytes)
	must(t, err)
	if protocol.Hash(retained) != protocol.Hash(v) {
		t.Fatal("retained history changed")
	}
	if _, err = r.reviewRecovery(context.Background()); err == nil {
		t.Fatal("live corruption hidden by retained history")
	}
}

func TestRecoveryEvidenceInterruptedCapture(t *testing.T) {
	for _, point := range []string{"checkpoint_evidence_intent", "checkpoint_evidence_copy", "checkpoint_evidence_file", "checkpoint_evidence_sealed", "checkpoint_evidence_ready"} {
		t.Run(point, func(t *testing.T) {
			r, review := checkpointFixture(t)
			r.fault = func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := r.checkpointRecovery(context.Background(), review.ReviewHash); err == nil {
				t.Fatal("fault not reached")
			}
			r.fault = nil
			first, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
			must(t, err)
			again, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
			must(t, err)
			if first != again {
				t.Fatal("checkpoint changed on evidence resume")
			}
		})
	}
}

func TestRecoveryEvidenceRejectsDamagedArchive(t *testing.T) {
	for _, kind := range []string{"missing-file", "corrupt-file", "symlink", "hardlink", "permissions", "directory-permissions", "missing-intent", "missing-seal", "changed-manifest", "unexpected-file", "remaining-temporary"} {
		t.Run(kind, func(t *testing.T) {
			r, review, cp, d, _ := evidenceFixture(t)
			file := filepath.Join(d.File().Name(), "evidence", evidenceCopyName(0))
			switch kind {
			case "missing-file":
				must(t, os.Remove(file))
			case "corrupt-file":
				must(t, os.WriteFile(file, []byte("bad"), 0600))
			case "symlink", "hardlink":
				other := filepath.Join(t.TempDir(), "retained")
				must(t, os.Rename(file, other))
				if kind == "symlink" {
					must(t, os.Symlink(other, file))
				} else {
					must(t, os.Link(other, file))
				}
			case "permissions":
				must(t, os.Chmod(file, 0644))
			case "directory-permissions":
				must(t, os.Chmod(filepath.Dir(file), 0755))
			case "missing-intent":
				must(t, os.Remove(filepath.Join(d.File().Name(), "evidence-intent.json")))
			case "missing-seal":
				must(t, os.Remove(filepath.Join(d.File().Name(), "evidence-sealed.json")))
			case "changed-manifest":
				must(t, d.WriteJSON("evidence.json", map[string]string{}, true))
			case "unexpected-file":
				must(t, os.WriteFile(filepath.Join(filepath.Dir(file), "extra.json"), []byte("{}"), 0600))
			case "remaining-temporary":
				must(t, os.WriteFile(filepath.Join(filepath.Dir(file), evidenceCopyFile), nil, 0600))
			}
			if _, err := readCheckpointEvidence(context.Background(), d, cp, review, r.config.MaxDatabaseBytes); err == nil {
				t.Fatal("damaged retained archive accepted")
			}
			if _, err := r.checkpointRecovery(context.Background(), review.ReviewHash); err == nil {
				t.Fatal("damaged archive silently repaired")
			}
			if kind == "missing-file" {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Fatal("sealed file regenerated", err)
				}
			}
		})
	}
}

func TestRecoveryEvidenceRejectsChangesDuringCapture(t *testing.T) {
	for _, kind := range []string{"source", "history", "intent", "seal", "copy", "directory", "worker-lease", "producer-lease", "cancel", "projection"} {
		t.Run(kind, func(t *testing.T) {
			r, review := checkpointFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.fault = func(at string) error {
				if at != "checkpoint_evidence_ready" {
					return nil
				}
				dir := filepath.Dir(checkpointPath(r, review, "evidence.json"))
				switch kind {
				case "source":
					return r.stage.WriteJSON("late.json", map[string]bool{"new": true}, false)
				case "history":
					return r.dir.WriteJSON(ownershipHistoryPath(producer, 1), map[string]bool{"changed": true}, true)
				case "intent":
					return os.WriteFile(filepath.Join(dir, "evidence-intent.json"), []byte("{}"), 0600)
				case "seal":
					return os.WriteFile(filepath.Join(dir, "evidence-sealed.json"), []byte("{}"), 0600)
				case "copy":
					return os.WriteFile(filepath.Join(dir, "evidence", evidenceCopyName(0)), []byte("{}"), 0600)
				case "projection":
					return os.WriteFile(filepath.Join(dir, checkpointDatabase), []byte("bad"), 0600)
				case "directory":
					if err := os.Rename(filepath.Join(dir, "evidence"), filepath.Join(dir, "moved")); err != nil {
						return err
					}
					return os.Mkdir(filepath.Join(dir, "evidence"), 0700)
				case "worker-lease", "producer-lease":
					name := "worker.lock"
					if kind == "producer-lease" {
						name = ownershipName(producer) + ".lock"
					}
					lock := filepath.Join(r.dir.File().Name(), name)
					if err := os.Rename(lock, lock+".old"); err != nil {
						return err
					}
					return os.WriteFile(lock, nil, 0600)
				default:
					cancel()
					return nil
				}
			}
			if _, err := r.checkpointRecovery(ctx, review.ReviewHash); err == nil {
				t.Fatal("capture accepted changed evidence")
			}
		})
	}
}

func TestRecoveryEvidenceBoundsAndScope(t *testing.T) {
	t.Run("retained-byte-budget", func(t *testing.T) {
		r, review, cp, d, _ := evidenceFixture(t)
		if _, err := readCheckpointEvidence(context.Background(), d, cp, review, 1); err == nil {
			t.Fatal("reader ignored evidence byte budget")
		}
		if _, err := readCheckpointEvidence(context.Background(), d, RecoveryCheckpoint{}, review, r.config.MaxDatabaseBytes); err == nil {
			t.Fatal("reader accepted another checkpoint binding")
		}
		changed := review
		changed.ConfigHash = protocol.Hash("another configuration")
		if _, err := readCheckpointEvidence(context.Background(), d, cp, changed, r.config.MaxDatabaseBytes); err == nil {
			t.Fatal("reader accepted a changed review retaining its old hash")
		}
		cp.DatabaseHash = protocol.Hash("another projection")
		if _, err := readCheckpointEvidence(context.Background(), d, cp, review, r.config.MaxDatabaseBytes); err == nil {
			t.Fatal("reader accepted a changed checkpoint retaining its old hash")
		}
	})
	t.Run("ownership-budget", func(t *testing.T) {
		r, _ := checkpointFixture(t)
		var owner ownership
		must(t, r.dir.ReadJSON(ownershipName(producer)+".json", &owner))
		owner.Revision = ^uint64(0)
		must(t, r.dir.WriteJSON(ownershipName(producer)+".json", owner, true))
		if _, err := r.reviewRecovery(context.Background()); err == nil {
			t.Fatal("unbounded history accepted")
		}
	})
	t.Run("unrelated-root-files", func(t *testing.T) {
		r, before := checkpointFixture(t)
		must(t, r.dir.WriteJSON("unrelated-private-config.json", map[string]string{"value": "not-enrolled"}, false))
		after, err := r.reviewRecovery(context.Background())
		must(t, err)
		if before.ReviewHash != after.ReviewHash {
			t.Fatal("review captured unrelated worker file")
		}
	})
	t.Run("invalid-filename", func(t *testing.T) {
		r, _ := checkpointFixture(t)
		must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), string([]byte{0x80})+".json"), []byte("{}"), 0600))
		if _, err := r.reviewRecovery(context.Background()); err == nil {
			t.Fatal("ambiguous invalid UTF-8 filename accepted")
		}
	})
}

func TestRecoveryEvidenceMultipleCheckpointsRetainHistory(t *testing.T) {
	r, review, cp, d, saved := evidenceFixture(t)
	ctx := context.Background()
	_, err := r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	current, err := r.reviewRecovery(ctx)
	must(t, err)
	if current.ReviewHash == review.ReviewHash {
		t.Fatal("ownership transition did not change review")
	}
	_, err = r.checkpointRecovery(ctx, current.ReviewHash)
	must(t, err)
	retained, err := readCheckpointEvidence(ctx, d, cp, review, r.config.MaxDatabaseBytes)
	must(t, err)
	if protocol.Hash(retained) != protocol.Hash(saved) {
		t.Fatal("new checkpoint replaced ancestor evidence")
	}
	if _, err = r.checkpointRecovery(ctx, review.ReviewHash); err == nil {
		t.Fatal("old proof authorized capture of new state")
	}
}

func TestRecoveryEvidencePartialCopyOwnership(t *testing.T) {
	for _, unowned := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "unowned"}[unowned], func(t *testing.T) {
			r, review := checkpointFixture(t)
			r.fault = func(at string) error {
				if at == "checkpoint_evidence_intent" {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := r.checkpointRecovery(context.Background(), review.ReviewHash); err == nil {
				t.Fatal("fault not reached")
			}
			dir := filepath.Join(filepath.Dir(checkpointPath(r, review, "evidence.json")), "evidence")
			must(t, os.Mkdir(dir, 0700))
			partial := filepath.Join(dir, evidenceCopyFile)
			must(t, os.WriteFile(partial, []byte("partial-copy"), 0600))
			if unowned {
				must(t, os.Remove(checkpointPath(r, review, "evidence-intent.json")))
			}
			r.fault = nil
			_, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
			if unowned {
				if err == nil {
					t.Fatal("unowned copy adopted")
				}
				bytes, e := os.ReadFile(partial)
				must(t, e)
				if string(bytes) != "partial-copy" {
					t.Fatal("unowned copy modified")
				}
			} else {
				must(t, err)
			}
		})
	}
}
