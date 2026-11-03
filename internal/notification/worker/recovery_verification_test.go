package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func verificationFixture(t *testing.T) (string, Config, RecoveryPublicationResult, RecoveryVerification) {
	t.Helper()
	path, c, image := publishFixture(t)
	result, err := publishImage(context.Background(), path, c, image, nil)
	must(t, err)
	review, err := ReviewPublishedRecovery(context.Background(), path, c, result.Commit.CommitHash)
	must(t, err)
	return path, c, result, review
}

func TestRecoveryVerificationRuntime(t *testing.T) {
	path, c, result, review := verificationFixture(t)
	ctx := context.Background()
	revision, err := safefs.NewID("n")
	must(t, err)
	if s, e := OpenManagedRecoveryVerification(path, c, review.After, revision, result.Commit.CommitHash); e == nil {
		s.Close()
		t.Fatal("unadopted publication opened")
	}
	adopted, err := AdoptPublishedRecoveryVerification(ctx, path, c, result.Commit.CommitHash)
	must(t, err)
	if adopted != review {
		t.Fatal("adoption changed review")
	}
	for n := 0; n < 2; n++ {
		s, e := OpenManagedRecoveryVerification(path, c, review.After, revision, result.Commit.CommitHash)
		must(t, e)
		if !s.VerificationOnly() {
			t.Fatal("recovery gained production authority")
		}
		must(t, s.RequireInactiveProducers())
		if e = s.Activate(producer, "one"); e == nil {
			t.Fatal("recovered verification activated producer")
		}
		if _, e = s.GrantCommand(ctx, producer, "anything"); e == nil {
			t.Fatal("recovered verification granted command")
		}
		if _, e = s.AdmitCommand(ctx, interaction.Inbound{}); e == nil {
			t.Fatal("recovered verification admitted command")
		}
		if e = s.PullTelegram(ctx, provider.TelegramPoller{}); e == nil {
			t.Fatal("recovered verification polled Telegram")
		}
		if e = s.PullSignal(ctx, provider.SignalInboxClient{}); e == nil {
			t.Fatal("recovered verification pulled Signal")
		}
		health, e := s.Health(ctx)
		must(t, e)
		if health.RecoveryCommitHash != result.Commit.CommitHash || health.InstallationRevision != revision || health.Condition == "ok" {
			t.Fatal(health)
		}
		// Exercise real canonical writes so restart cannot rely on image-byte equality.
		_, e = s.db.Exec("INSERT OR REPLACE INTO provider_cooldowns(provider,due) VALUES('telegram',123)")
		must(t, e)
		must(t, s.Close())
		again, e := AdoptPublishedRecoveryVerification(ctx, path, c, result.Commit.CommitHash)
		must(t, e)
		if again != adopted {
			t.Fatal("adoption replay changed")
		}
	}
	if s, e := Open(path, c, false); e == nil {
		s.Close()
		t.Fatal("ordinary startup bypassed quarantine")
	}
	if s, e := OpenManagedVerification(path, c, review.After); e == nil {
		s.Close()
		t.Fatal("unbound managed startup bypassed quarantine")
	}
	im := result.Commit.Image
	_, err = VerifyRecoveryPublication(ctx, path, c, im.Operation, im.ReviewHash, im.CheckpointHash, im.ProofHash, im.ImageHash)
	must(t, err)
}

func TestRecoveryVerificationInterrupted(t *testing.T) {
	for _, boundary := range []string{"recovery_verification_intent", "recovery_verification_complete"} {
		t.Run(boundary, func(t *testing.T) {
			path, c, result, review := verificationFixture(t)
			stop := errors.New("interruption")
			_, err := recoveryVerification(context.Background(), path, c, result.Commit.CommitHash, true, func(b string) error {
				if b == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			got, err := AdoptPublishedRecoveryVerification(context.Background(), path, c, result.Commit.CommitHash)
			must(t, err)
			if got != review {
				t.Fatal(got)
			}
		})
	}
}

func TestRecoveryVerificationRefusesChangedAuthority(t *testing.T) {
	for _, kind := range []string{"commit", "identity", "completion", "anchor", "quarantine", "original", "proof", "root-phase", "candidate", "sidecar"} {
		t.Run(kind, func(t *testing.T) {
			path, c, result, review := verificationFixture(t)
			_, err := AdoptPublishedRecoveryVerification(context.Background(), path, c, result.Commit.CommitHash)
			must(t, err)
			d, err := privateDirectory(path)
			must(t, err)
			defer d.Close()
			pin := result.Commit.CommitHash
			switch kind {
			case "commit":
				pin = protocol.Hash("wrong")
			case "identity":
				must(t, d.WriteJSON("identity.json", review.Before, true))
			case "completion":
				must(t, os.Remove(filepath.Join(path, recoveryVerificationComplete)))
			case "anchor", "quarantine":
				db, e := snapshotDB(d, "state.db", false)
				must(t, e)
				key := publicationAnchorKey
				if kind == "quarantine" {
					key = "recovery"
				}
				_, e = db.Exec("DELETE FROM metadata WHERE key=?", key)
				must(t, e)
				must(t, db.Close())
			case "original":
				must(t, os.Remove(filepath.Join(path, publicationExchange)))
			case "root-phase", "candidate":
				var j snapshotRecovery
				must(t, d.ReadJSON("recovery.json", &j))
				if kind == "root-phase" {
					j.Phase = "prepared"
					j.CandidateHash = ""
				} else {
					j.CandidateHash = protocol.Hash("wrong candidate")
				}
				must(t, d.WriteJSON("recovery.json", j, true))
			case "sidecar":
				must(t, os.Symlink(filepath.Join(path, "state.db"), filepath.Join(path, "state.db-wal")))
			case "proof":
				must(t, os.Remove(filepath.Join(path, publicationCommitSeal)))
			}
			revision, e := safefs.NewID("n")
			must(t, e)
			if s, e := OpenManagedRecoveryVerification(path, c, review.After, revision, pin); e == nil {
				s.Close()
				t.Fatal("accepted changed " + kind)
			}
		})
	}
}

func TestRecoveryVerificationLostCompletionAfterRuntime(t *testing.T) {
	path, c, result, review := verificationFixture(t)
	ctx := context.Background()
	_, err := AdoptPublishedRecoveryVerification(ctx, path, c, result.Commit.CommitHash)
	must(t, err)
	revision, err := safefs.NewID("n")
	must(t, err)
	s, err := OpenManagedRecoveryVerification(path, c, review.After, revision, result.Commit.CommitHash)
	must(t, err)
	_, err = s.db.Exec("INSERT OR REPLACE INTO provider_cooldowns(provider,due) VALUES('telegram',456)")
	must(t, err)
	must(t, s.Close())
	must(t, os.Remove(filepath.Join(path, recoveryVerificationComplete)))
	if _, err = AdoptPublishedRecoveryVerification(ctx, path, c, result.Commit.CommitHash); err == nil {
		t.Fatal("lost completion reconstructed from modified runtime")
	}
}

func TestRecoveryVerificationBoundaryRejectsChangedEvidence(t *testing.T) {
	for _, name := range []string{recoveryVerificationIntent, publicationCommitComplete, "worker.lock"} {
		t.Run(name, func(t *testing.T) {
			path, c, result, _ := verificationFixture(t)
			_, err := recoveryVerification(context.Background(), path, c, result.Commit.CommitHash, true, func(at string) error {
				if at == "recovery_verification_intent" {
					return os.Remove(filepath.Join(path, name))
				}
				return nil
			})
			if err == nil {
				t.Fatal("changed authority accepted")
			}
			if _, err = os.Stat(filepath.Join(path, recoveryVerificationComplete)); !os.IsNotExist(err) {
				t.Fatal("changed authority completed adoption", err)
			}
		})
	}
}
