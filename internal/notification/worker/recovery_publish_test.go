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

func publishFixture(t *testing.T) (string, Config, RecoveryPublicationImage) {
	t.Helper()
	r := originalFixture(t)
	ctx := context.Background()
	_, err := r.preserveOriginal(ctx)
	must(t, err)
	_, err = r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	return sealPublicationFixture(t, r)
}

func sealPublicationFixture(t *testing.T, r *recoveryReconciler) (string, Config, RecoveryPublicationImage) {
	t.Helper()
	ctx := context.Background()
	_, err := r.preserveOriginal(ctx)
	must(t, err)
	review, err := r.reviewRecovery(ctx)
	must(t, err)
	must(t, r.publicationReady(ctx, review))
	cp, err := r.checkpointRecovery(ctx, review.ReviewHash)
	must(t, err)
	proof, err := verifyFixtureProof(ctx, r, review, cp, nil)
	must(t, err)
	image, err := prepareFixturePublication(ctx, r, review, cp, proof)
	must(t, err)
	path, c := r.dir.File().Name(), r.config
	must(t, r.Close())
	return path, c, image
}

func TestRecoveryPublishPreservesProviderAndLEDFences(t *testing.T) {
	for _, kind := range []string{"signal", "telegram", "led"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var r *recoveryReconciler
			var externalUnchanged func()
			switch kind {
			case "signal":
				f, proof := signalCursorFixture(t)
				must(t, f.r.InstallSignalCursor(ctx, proof))
				r = f.r
			case "telegram":
				f, session, _ := telegramCursorFixture(t)
				_, err := session.CommitTelegramRecoveryCursor(ctx, f.Account)
				must(t, err)
				r = session
			case "led":
				session, _, _, api, adoption := epochFixture(t, true)
				_, err := session.handoffLED(ctx, migrationSocket, adoption)
				must(t, err)
				r = session
				before := protocol.Hash(api.states)
				externalUnchanged = func() {
					if before != protocol.Hash(api.states) {
						t.Fatal("publication contacted LED daemon")
					}
				}
			}
			_, err := r.preserveOriginal(ctx)
			must(t, err)
			v, err := r.reviewRecovery(ctx)
			must(t, err)
			for _, check := range v.Checks {
				if check.Scope == "producer/"+producer.ID && check.Stage == "ownership" && check.State == "pending" {
					_, err = r.fenceRecoveryOwnership(ctx, producer)
					must(t, err)
				}
			}
			path, c, image := sealPublicationFixture(t, r)
			result, err := publishImage(ctx, path, c, image, nil)
			must(t, err)
			if !result.CanonicalInstalled || result.DeliveryEnabled || result.ExecutionEnabled {
				t.Fatal(result)
			}
			if externalUnchanged != nil {
				externalUnchanged()
			}
			if s, e := Open(path, c, false); e == nil {
				s.Close()
				t.Fatal("publication enabled provider or LED runtime")
			}
			adoption, e := AdoptPublishedRecoveryVerification(ctx, path, c, result.Commit.CommitHash)
			must(t, e)
			revision, e := safefs.NewID("n")
			must(t, e)
			s, e := OpenManagedRecoveryVerification(path, c, adoption.After, revision, result.Commit.CommitHash)
			must(t, e)
			must(t, s.RequireInactiveProducers())
			health, e := s.Health(ctx)
			must(t, e)
			if !health.VerificationOnly || health.Condition == "ok" {
				t.Fatal(health)
			}
			must(t, s.Close())
			if externalUnchanged != nil {
				externalUnchanged()
			}
		})
	}
}

func publishImage(ctx context.Context, path string, c Config, image RecoveryPublicationImage, fault func(string) error) (RecoveryPublicationResult, error) {
	return publishRecovery(ctx, path, c, image.Operation, image.ReviewHash, image.CheckpointHash, image.ProofHash, image.ImageHash, fault)
}

func TestRecoveryPublishInstallsAndRetainsQuarantine(t *testing.T) {
	path, c, image := publishFixture(t)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(path, "state.db"))
	must(t, err)
	one, err := publishImage(ctx, path, c, image, nil)
	must(t, err)
	if !one.CanonicalInstalled || one.DeliveryEnabled || one.ExecutionEnabled || one.Commit.Image != image {
		t.Fatal(one)
	}
	after, err := os.ReadFile(filepath.Join(path, "state.db"))
	must(t, err)
	old, err := os.ReadFile(filepath.Join(path, publicationExchange))
	must(t, err)
	if string(before) != string(old) || string(before) == string(after) {
		t.Fatal("canonical swap lost original or failed to install")
	}
	d, err := safefs.Open(path)
	must(t, err)
	must(t, verifyRetainedDatabase(ctx, d, "state.db", image.DatabaseHash, image.DatabaseBytes, c.MaxDatabaseBytes))
	must(t, d.Close())
	two, err := publishImage(ctx, path, c, image, nil)
	must(t, err)
	if one != two {
		t.Fatal("publication replay changed", one, two)
	}
	verified, err := VerifyRecoveryPublication(ctx, path, c, image.Operation, image.ReviewHash, image.CheckpointHash, image.ProofHash, image.ImageHash)
	must(t, err)
	if verified != image {
		t.Fatal("canonical swap invalidated historical image")
	}
	if s, err := Open(path, c, false); err == nil {
		s.Close()
		t.Fatal("publication activated worker")
	}
	if r, err := openRecoveryReconciler(ctx, path, c, nil); err == nil {
		r.Close()
		t.Fatal("publication did not freeze reconciliation")
	}
	must(t, os.Remove(filepath.Join(path, "recovery.json")))
	if s, err := Open(path, c, false); err == nil {
		s.Close()
		t.Fatal("lost recovery journal bypassed publication gate")
	}
}

func TestRecoveryPublishRevalidatesBeforeExchange(t *testing.T) {
	for _, kind := range []string{"seal", "completion", "lease", "image", "projection", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			path, c, image := publishFixture(t)
			before, err := os.ReadFile(filepath.Join(path, "state.db"))
			must(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fault := func(at string) error {
				if at != "publication_install_seal" {
					return nil
				}
				switch kind {
				case "seal":
					return os.Remove(filepath.Join(path, publicationCommitSeal))
				case "completion":
					return os.WriteFile(filepath.Join(path, publicationCommitComplete), []byte("{}"), 0600)
				case "lease":
					p := filepath.Join(path, "worker.lock")
					if err := os.Rename(p, p+".old"); err != nil {
						return err
					}
					return os.WriteFile(p, nil, 0600)
				case "image":
					return os.WriteFile(filepath.Join(path, "recovery", image.Operation, "checkpoints", image.ReviewHash, publicationDatabase), []byte("bad"), 0600)
				case "projection":
					return os.WriteFile(filepath.Join(path, "recovery", image.Operation, "reconciled.db"), []byte("bad"), 0600)
				default:
					cancel()
					return nil
				}
			}
			if _, err := publishImage(ctx, path, c, image, fault); err == nil {
				t.Fatal("changed evidence accepted")
			}
			after, err := os.ReadFile(filepath.Join(path, "state.db"))
			must(t, err)
			if string(before) != string(after) {
				t.Fatal("changed evidence caused canonical exchange")
			}
		})
	}
}

func TestRecoveryPublishNoLEDExemptionRequiresEmptyHistory(t *testing.T) {
	r, review, _, _, _ := preparedImageFixture(t)
	ok, err := r.noPublicationLEDHistory(context.Background())
	must(t, err)
	if !ok {
		t.Fatal("empty LED history not recognized")
	}
	_, err = r.db.Exec("INSERT INTO led_states(id,condition) VALUES('retained-fault','critical')")
	must(t, err)
	ok, err = r.noPublicationLEDHistory(context.Background())
	must(t, err)
	if ok {
		t.Fatal("unconfigured retained fault ignored")
	}
	for _, c := range review.Checks {
		if c.Scope == "led" && c.State == "pending" {
			if err := r.publicationReady(context.Background(), RecoveryStatus{Checks: []RecoveryStatusCheck{c}}); err == nil {
				t.Fatal("LED pending stage waived")
			}
		}
	}
}

var publicationInstallBoundaries = []string{"publication_freeze", "publication_sidecars", "publication_install_copy", "publication_install_seal", "publication_exchange", "publication_install_sync", "publication_install_complete"}

func TestRecoveryPublishOriginalSidecars(t *testing.T) {
	for _, nonempty := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkpointed", true: "uncheckpointed"}[nonempty], func(t *testing.T) {
			path, c, image := publishFixture(t)
			wal := []byte{}
			if nonempty {
				wal = []byte("uncheckpointed")
			}
			must(t, os.WriteFile(filepath.Join(path, "state.db-wal"), wal, 0600))
			must(t, os.WriteFile(filepath.Join(path, "state.db-shm"), make([]byte, 32768), 0600))
			before := retainedTreeHash(t, path)
			_, err := publishImage(context.Background(), path, c, image, nil)
			if nonempty {
				if err == nil || before != retainedTreeHash(t, path) {
					t.Fatal("uncheckpointed data discarded", err)
				}
			} else {
				must(t, err)
				for _, suffix := range []string{"-wal", "-shm"} {
					if _, err := os.Stat(filepath.Join(path, "state.db"+suffix)); !os.IsNotExist(err) {
						t.Fatal("obsolete index retained", err)
					}
				}
			}
		})
	}
}

func TestRecoveryPublishInterruptedTransitions(t *testing.T) {
	for _, point := range publicationInstallBoundaries {
		t.Run(point, func(t *testing.T) {
			path, c, image := publishFixture(t)
			fault := func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := publishImage(context.Background(), path, c, image, fault); err == nil {
				t.Fatal("boundary missed")
			}
			if r, err := openRecoveryReconciler(context.Background(), path, c, nil); err == nil {
				r.Close()
				t.Fatal("interrupted publication allowed edits")
			}
			one, err := publishImage(context.Background(), path, c, image, nil)
			must(t, err)
			two, err := publishImage(context.Background(), path, c, image, nil)
			must(t, err)
			if one != two || !one.CanonicalInstalled {
				t.Fatal(one, two)
			}
		})
	}
}

func TestRecoveryPublishRefusesMissingOrChangedEvidence(t *testing.T) {
	for _, kind := range []string{"canonical", "original", "image", "completion", "missing-seal", "intent", "sidecar", "rolled-back"} {
		t.Run(kind, func(t *testing.T) {
			path, c, image := publishFixture(t)
			_, err := publishImage(context.Background(), path, c, image, nil)
			must(t, err)
			switch kind {
			case "canonical":
				must(t, os.WriteFile(filepath.Join(path, "state.db"), []byte("bad"), 0600))
			case "original":
				must(t, os.Remove(filepath.Join(path, publicationExchange)))
			case "image":
				must(t, os.Remove(filepath.Join(path, "recovery", image.Operation, "checkpoints", image.ReviewHash, publicationDatabase)))
			case "completion":
				must(t, os.WriteFile(filepath.Join(path, publicationCommitComplete), []byte("{}"), 0600))
			case "missing-seal":
				must(t, os.Remove(filepath.Join(path, publicationCommitSeal)))
			case "intent":
				must(t, os.WriteFile(filepath.Join(path, publicationCommitIntent), []byte("{}"), 0600))
			case "sidecar":
				must(t, os.WriteFile(filepath.Join(path, "state.db-wal"), nil, 0600))
			case "rolled-back":
				data, e := os.ReadFile(filepath.Join(path, publicationExchange))
				must(t, e)
				must(t, os.WriteFile(filepath.Join(path, "state.db"), data, 0600))
			}
			before := retainedTreeHash(t, path)
			if _, err = publishImage(context.Background(), path, c, image, nil); err == nil {
				t.Fatal("damaged transition accepted")
			}
			if before != retainedTreeHash(t, path) {
				t.Fatal("damaged completed transition rewritten")
			}
		})
	}
}

func TestRecoveryPublishReadinessAndStalePins(t *testing.T) {
	r, review, _, _, image := preparedImageFixture(t)
	path, c := r.dir.File().Name(), r.config
	if err := r.publicationReady(context.Background(), review); err == nil {
		t.Fatal("unfenced producer passed readiness")
	}
	must(t, r.Close())
	if _, err := publishImage(context.Background(), path, c, image, nil); err == nil {
		t.Fatal("unready recovery published")
	}
	if _, err := os.Stat(filepath.Join(path, publicationCommitIntent)); !os.IsNotExist(err) {
		t.Fatal("unready recovery frozen", err)
	}
	path, c, image = publishFixture(t)
	image.ImageHash = protocol.Hash("wrong-image")
	if _, err := publishImage(context.Background(), path, c, image, nil); err == nil {
		t.Fatal("wrong image published")
	}
	if _, err := os.Stat(filepath.Join(path, publicationCommitIntent)); !os.IsNotExist(err) {
		t.Fatal("wrong image frozen", err)
	}
}
