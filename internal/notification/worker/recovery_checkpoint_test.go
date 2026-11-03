package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func checkpointFixture(t *testing.T) (*recoveryReconciler, RecoveryStatus) {
	t.Helper()
	r := originalFixture(t)
	_, err := r.preserveOriginal(context.Background())
	must(t, err)
	review, err := r.reviewRecovery(context.Background())
	must(t, err)
	return r, review
}

func checkpointPath(r *recoveryReconciler, review RecoveryStatus, name string) string {
	return filepath.Join(r.stage.File().Name(), "checkpoints", review.ReviewHash, name)
}

func TestRecoveryCheckpointPreservesWALAndRowIDs(t *testing.T) {
	ctx := context.Background()
	r, _ := checkpointFixture(t)
	_, err := r.db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE checkpoint_values(v); INSERT INTO checkpoint_values(rowid,v) VALUES(42,CAST(x'80' AS TEXT)),(99,x'80'); PRAGMA application_id=37")
	must(t, err)
	main, err := immutableOriginalDB(r.stage, "reconciled.db")
	must(t, err)
	var count int
	must(t, main.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='checkpoint_values'").Scan(&count))
	must(t, main.Close())
	if count != 0 {
		t.Fatal("fixture changes are not confined to WAL")
	}
	review, err := r.reviewRecovery(ctx)
	must(t, err)
	result, err := r.checkpointRecovery(ctx, review.ReviewHash)
	must(t, err)
	replay, err := r.checkpointRecovery(ctx, review.ReviewHash)
	must(t, err)
	if result != replay || result.PublicationAllowed || result.DeliveryEnabled || result.ExecutionEnabled {
		t.Fatal(result, replay)
	}
	d, err := r.stage.Sub("checkpoints/" + review.ReviewHash)
	must(t, err)
	defer d.Close()
	db, err := immutableOriginalDB(d, checkpointDatabase)
	must(t, err)
	defer db.Close()
	var value, kind string
	must(t, db.QueryRow("SELECT hex(v),typeof(v) FROM checkpoint_values WHERE rowid=42").Scan(&value, &kind))
	if value != "80" || kind != "text" {
		t.Fatal("changed retained row identity or text bytes", value, kind)
	}
	must(t, db.QueryRow("SELECT hex(v),typeof(v) FROM checkpoint_values WHERE rowid=99").Scan(&value, &kind))
	if value != "80" || kind != "blob" {
		t.Fatal(value, kind)
	}
	must(t, db.QueryRow("PRAGMA application_id").Scan(&count))
	if count != 37 {
		t.Fatal("lost SQLite header")
	}
	after, err := r.reviewRecovery(ctx)
	must(t, err)
	if protocol.Hash(after) != protocol.Hash(review) {
		t.Fatal("checkpoint changed source review")
	}
	path, c := r.dir.File().Name(), r.config
	must(t, r.Close())
	if s, e := Open(path, c, false); e == nil {
		s.Close()
		t.Fatal("checkpoint activated worker")
	}
}

func TestRecoveryCheckpointResume(t *testing.T) {
	for _, point := range []string{"checkpoint_intent", "checkpoint_copy_step", "checkpoint_copy", "checkpoint_sealed", "checkpoint_database", "checkpoint_ready"} {
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
			result, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
			must(t, err)
			replay, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
			must(t, err)
			if result != replay || !safefs.DigestPattern.MatchString(result.CheckpointHash) {
				t.Fatal(result, replay)
			}
		})
	}
}

func TestRecoveryCheckpointRejectsLostOrChangedCompletedEvidence(t *testing.T) {
	for _, kind := range []string{"database", "missing-database", "missing-seal", "intent", "seal", "manifest", "database-sidecar", "temporary-sidecar", "unowned-temporary", "symlink", "hardlink", "permissions", "directory-permissions"} {
		t.Run(kind, func(t *testing.T) {
			r, review := checkpointFixture(t)
			_, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
			must(t, err)
			file := checkpointPath(r, review, checkpointDatabase)
			switch kind {
			case "database":
				must(t, os.WriteFile(file, []byte("corrupt"), 0600))
			case "missing-database":
				must(t, os.Remove(file))
			case "missing-seal":
				must(t, os.Remove(checkpointPath(r, review, "sealed.json")))
			case "intent", "seal", "manifest":
				name := map[string]string{"intent": "intent.json", "seal": "sealed.json", "manifest": "checkpoint.json"}[kind]
				must(t, os.WriteFile(checkpointPath(r, review, name), []byte("{}"), 0600))
			case "database-sidecar":
				must(t, os.WriteFile(file+"-wal", nil, 0600))
			case "temporary-sidecar":
				must(t, os.WriteFile(checkpointPath(r, review, checkpointTemporary+"-journal"), nil, 0600))
			case "unowned-temporary":
				must(t, os.WriteFile(checkpointPath(r, review, checkpointTemporary), nil, 0600))
			case "symlink", "hardlink":
				moved := file + ".retained"
				must(t, os.Rename(file, moved))
				if kind == "symlink" {
					must(t, os.Symlink(moved, file))
				} else {
					must(t, os.Link(moved, file))
				}
			case "permissions":
				must(t, os.Chmod(file, 0644))
			case "directory-permissions":
				must(t, os.Chmod(filepath.Dir(file), 0755))
			}
			if _, err = r.checkpointRecovery(context.Background(), review.ReviewHash); err == nil {
				t.Fatal("changed checkpoint accepted")
			}
			if kind == "missing-database" {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Fatal("completed database was regenerated", err)
				}
			}
		})
	}
}

func TestRecoveryCheckpointRejectsStaleReviewAndChangesDuringCapture(t *testing.T) {
	for _, kind := range []string{"stale", "sql", "header", "evidence", "owner-lock", "worker-lock", "intent", "intent-during-final-review", "temporary", "seal", "sealed-copy", "directory", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			r, review := checkpointFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "stale" {
				_, err := r.db.Exec("INSERT INTO metadata VALUES('changed','yes')")
				must(t, err)
			} else {
				finalReview := false
				r.fault = func(at string) error {
					if finalReview && at == "recovery_status_observed" && kind == "intent-during-final-review" {
						return os.WriteFile(checkpointPath(r, review, "intent.json"), []byte("{}"), 0600)
					}
					if at != "checkpoint_ready" {
						return nil
					}
					switch kind {
					case "sql":
						_, e := r.db.Exec("INSERT INTO metadata VALUES('changed','yes')")
						return e
					case "header":
						_, e := r.db.Exec("PRAGMA application_id=38")
						return e
					case "evidence":
						return r.stage.WriteJSON("new-evidence.json", map[string]string{"new": "yes"}, false)
					case "owner-lock", "worker-lock":
						name := "worker.lock"
						if kind == "owner-lock" {
							name = ownershipName(producer) + ".lock"
						}
						path := filepath.Join(r.dir.File().Name(), name)
						if e := os.Rename(path, path+".old"); e != nil {
							return e
						}
						return os.WriteFile(path, nil, 0600)
					case "intent-during-final-review":
						finalReview = true
						return nil
					case "temporary":
						return os.WriteFile(checkpointPath(r, review, checkpointTemporary), nil, 0600)
					case "seal":
						return os.WriteFile(checkpointPath(r, review, "sealed.json"), []byte("{}"), 0600)
					case "intent":
						return os.WriteFile(checkpointPath(r, review, "intent.json"), []byte("{}"), 0600)
					case "sealed-copy":
						return os.WriteFile(checkpointPath(r, review, checkpointDatabase), []byte("bad"), 0600)
					case "directory":
						path := filepath.Dir(checkpointPath(r, review, checkpointDatabase))
						if e := os.Rename(path, path+".old"); e != nil {
							return e
						}
						return os.Mkdir(path, 0700)
					default:
						cancel()
						return nil
					}
				}
			}
			if _, err := r.checkpointRecovery(ctx, review.ReviewHash); err == nil {
				t.Fatal("changed review accepted")
			}
			if kind == "stale" {
				if _, err := os.Stat(checkpointPath(r, review, "intent.json")); !os.IsNotExist(err) {
					t.Fatal("stale review created checkpoint", err)
				}
			}
		})
	}
}

func TestRecoveryCheckpointMissingSealedCopyNeverRebuilt(t *testing.T) {
	r, review := checkpointFixture(t)
	r.fault = func(at string) error {
		if at == "checkpoint_sealed" {
			return errors.New("interrupted")
		}
		return nil
	}
	_, err := r.checkpointRecovery(context.Background(), review.ReviewHash)
	if err == nil {
		t.Fatal("fault not reached")
	}
	must(t, os.Remove(checkpointPath(r, review, checkpointTemporary)))
	r.fault = nil
	if _, err = r.checkpointRecovery(context.Background(), review.ReviewHash); err == nil {
		t.Fatal("lost sealed copy regenerated")
	}
}

func TestRecoveryCheckpointRequiresOriginalAndExistingState(t *testing.T) {
	r := originalFixture(t)
	review, err := r.reviewRecovery(context.Background())
	must(t, err)
	if _, err = r.checkpointRecovery(context.Background(), review.ReviewHash); err == nil {
		t.Fatal("checkpoint allowed without retained original")
	}
	_, err = r.preserveOriginal(context.Background())
	must(t, err)
	review, err = r.reviewRecovery(context.Background())
	must(t, err)
	path, c, op := r.dir.File().Name(), r.config, r.journal.Operation
	if _, err = CheckpointRecovery(context.Background(), path, c, op, review.ReviewHash); err == nil {
		t.Fatal("checkpoint bypassed worker lease")
	}
	must(t, r.Close())
	_, err = CheckpointRecovery(context.Background(), path, c, op, review.ReviewHash)
	must(t, err)
}
