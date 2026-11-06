package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

func recoveredDeliveryFixture(t *testing.T, oldSnapshot bool, reconcile ...bool) (string, Config, string, ManagedDeliveryRecovery) {
	return recoveredDeliveryFixtureSetup(t, oldSnapshot, false, reconcile...)
}
func recoveredDeliveryFixtureSetup(t *testing.T, oldSnapshot, commands bool, reconcile ...bool) (string, Config, string, ManagedDeliveryRecovery) {
	return recoveredDeliveryFixtureConfigured(t, oldSnapshot, commands, nil, nil, reconcile...)
}

func recoveredDeliveryFixtureConfigured(t *testing.T, oldSnapshot, commands bool, before func(*Store), recovered func(string, Config), reconcile ...bool) (string, Config, string, ManagedDeliveryRecovery) {
	t.Helper()
	ctx := context.Background()
	var s *Store
	var path string
	if commands {
		s, _, path, _ = commandFixture(t)
	} else {
		s, _, path = fixture(t)
	}
	if commands {
		must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: s.config.Interaction.Destinations[0].Account}))
	}
	if before != nil {
		before(s)
	}
	c, id := s.config, s.identity
	must(t, s.Revoke(ctx, producer, "generation-one"))
	artifact := filepath.Join(t.TempDir(), "snapshot")
	var manifest SnapshotManifest
	var err error
	if oldSnapshot {
		manifest, err = s.Snapshot(ctx, artifact)
		must(t, err)
	}
	must(t, s.Close())
	revision, err := safefs.NewID("n")
	must(t, err)
	installation, err := safefs.NewID("n")
	must(t, err)
	owner := ManagedDeliveryOwner{1, t.TempDir(), installation, id, protocol.Hash(c)}
	s, err = OpenManagedRuntime(path, c, id, revision)
	must(t, err)
	must(t, s.EnrollManagedDelivery(owner))
	if commands {
		must(t, s.Close())
		s, err = OpenManagedDelivery(path, c, id, revision, owner, protocol.Hash(owner), protocol.Hash("history-activation"))
		must(t, err)
		must(t, s.CommitManagedCommandAdmission("all", protocol.Hash("history-enabled"), ""))
		must(t, s.CommitManagedCommandAdmission("drain", protocol.Hash("history-disabled"), protocol.Hash("history-enabled")))
	}
	if !oldSnapshot {
		manifest, err = s.Snapshot(ctx, artifact)
		must(t, err)
	}
	must(t, s.Close())
	q, err := QuarantineManagedDeliveryRecovery(ctx, path, artifact, c, manifest.ID, manifest.DatabaseHash, revision, owner)
	must(t, err)
	_, err = FenceRecoveryOwnership(ctx, path, c, producer)
	must(t, err)
	if commands {
		account := c.Interaction.Destinations[0].Account
		_, err = ReconcileTelegramHistory(ctx, path, artifact, account, c)
		must(t, err)
		_, err = drainTelegramRecovery(ctx, path, c, account, &fakeTelegramDrain{Account: account, Batch: 16}, nil)
		must(t, err)
		requests, e := OpenRequestRecoverySession(ctx, path, c)
		must(t, e)
		_, err = requests.SealRecoveryRequests(ctx)
		must(t, err)
		must(t, requests.Close())
		cursors, e := OpenTelegramRecoveryCursorSession(ctx, path, c)
		must(t, e)
		_, err = cursors.CommitTelegramRecoveryCursor(ctx, account)
		must(t, err)
		must(t, cursors.Close())
	}
	if len(reconcile) == 1 && reconcile[0] {
		r, e := openRecoveryReconciler(ctx, path, c, nil)
		must(t, e)
		ch, e := r.Challenge(ctx, producer)
		must(t, e)
		must(t, r.Begin(ctx, ch, protocol.RecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: id, Producer: producer}}))
		_, e = r.Finish(ctx, ch, snapshot(ch.IssuedAt, 1, 1))
		must(t, e)
		_, e = r.PrepareRecoveryOwnership(ctx, producer)
		must(t, e)
		must(t, r.Close())
		generation, e := OpenRecoveryGenerationSession(ctx, path, c)
		must(t, e)
		_, e = generation.PrepareRecoveryGeneration(ctx, producer)
		must(t, e)
		must(t, generation.Close())
	}
	if recovered != nil {
		recovered(path, c)
	}
	review, err := ReviewRecovery(ctx, path, c, q.Operation)
	must(t, err)
	cp, err := CheckpointRecovery(ctx, path, c, q.Operation, review.ReviewHash)
	must(t, err)
	proof, err := VerifyRecoveryCheckpoint(ctx, path, c, q.Operation, review.ReviewHash, cp.CheckpointHash)
	must(t, err)
	image, err := PrepareRecoveryPublication(ctx, path, c, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
	must(t, err)
	result, err := PublishRecovery(ctx, path, c, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
	must(t, err)
	adopted, err := AdoptPublishedRecoveryVerification(ctx, path, c, result.Commit.CommitHash)
	must(t, err)
	return path, c, revision, ManagedDeliveryRecovery{Version: 1, Owner: owner, Before: adopted.Before, After: adopted.After, CommitHash: result.Commit.CommitHash}
}

func TestManagedRecoveredDeliveryDispatchRequiresReconciliationAndActivation(t *testing.T) {
	path, c, revision, r := recoveredDeliveryFixture(t, true, true)
	s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
	must(t, err)
	must(t, s.EnrollManagedDeliveryRecovery(r))
	must(t, s.Close())
	s, err = OpenManagedRecoveredDelivery(path, c, revision, r, protocol.Hash(r.Owner), protocol.Hash("activation"))
	must(t, err)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	must(t, s.Stage(producer, "after-recovery"))
	must(t, s.Replace(ctx, snapshot(now, 2, 1)))
	m := report(now, "after-recovery", 1, 1, 1)
	m.Generation = "after-recovery"
	_, err = s.Enqueue(ctx, m)
	must(t, err)
	calls := 0
	sender := senderFunc(func(context.Context, string, protocol.Message) Result { calls++; return Result{Accepted: true} })
	_, err = s.DispatchOne(ctx, producer, sender)
	must(t, err)
	if calls != 0 {
		t.Fatal("staged producer sent")
	}
	must(t, s.Activate(producer, m.Generation))
	_, err = s.DispatchOne(ctx, producer, sender)
	must(t, err)
	if calls != 1 {
		t.Fatal("reconciled admitted producer did not send", calls)
	}
	_, err = s.DispatchOne(ctx, producer, sender)
	must(t, err)
	if calls != 1 {
		t.Fatal("accepted event resent", calls)
	}
}

func TestManagedRecoveredDeliveryOwnership(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(map[bool]string{false: "current-snapshot", true: "pre-enrollment-snapshot"}[old], func(t *testing.T) {
			path, c, revision, r := recoveredDeliveryFixture(t, old)
			if s, err := OpenManagedRecoveredDelivery(path, c, revision, r, protocol.Hash(r.Owner), protocol.Hash("activation")); err == nil {
				s.Close()
				t.Fatal("unbound recovery activated")
			}
			s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
			must(t, err)
			must(t, s.EnrollManagedDeliveryRecovery(r))
			must(t, s.EnrollManagedDeliveryRecovery(r))
			must(t, s.RequireManagedDeliveryRecovery(r, protocol.Hash(r.Owner)))
			if !s.VerificationOnly() {
				t.Fatal("owner enrollment activated runtime")
			}
			must(t, s.Close())
			s, err = OpenManagedRecoveredDelivery(path, c, revision, r, protocol.Hash(r.Owner), protocol.Hash("activation"))
			must(t, err)
			if s.VerificationOnly() {
				t.Fatal("delivery not enabled")
			}
			must(t, s.RequireInactiveProducers())
			if s.Stage(producer, "new-generation") == nil || s.Activate(producer, "generation-one") == nil {
				t.Fatal("delivery activation bypassed producer reconciliation")
			}
			must(t, s.Close())
			s, err = OpenManagedRecoveredDelivery(path, c, revision, r, protocol.Hash(r.Owner), protocol.Hash("activation"))
			must(t, err)
			defer s.Close()
			status, err := s.OwnershipStatus(producer, "generation-one")
			must(t, err)
			if status.State != "revoked" || status.Active {
				t.Fatal(status)
			}
			must(t, os.Remove(filepath.Join(path, recoveryVerificationComplete)))
			if s.RequireManagedDeliveryRecovery(r, protocol.Hash(r.Owner)) == nil {
				t.Fatal("live authority ignored lost adoption")
			}
		})
	}
}

func TestManagedRecoveredDeliveryInterruptedAndAnchorLoss(t *testing.T) {
	for _, point := range []string{"delivery-recovery-intent", "delivery-recovery-sql", "delivery-recovery-complete"} {
		t.Run(point, func(t *testing.T) {
			path, c, revision, r := recoveredDeliveryFixture(t, true)
			s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
			must(t, err)
			stop := errors.New("interrupted")
			err = s.enrollManagedDeliveryRecovery(r, func(at string) error {
				if at == point {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			must(t, s.Close())
			s, err = OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
			must(t, err)
			defer s.Close()
			must(t, s.EnrollManagedDeliveryRecovery(r))
			must(t, s.RequireManagedDeliveryRecovery(r, protocol.Hash(r.Owner)))
			if point == "delivery-recovery-intent" {
				must(t, os.Remove(filepath.Join(path, r.name()+".json")))
			} else {
				key := r.name()
				if point == "delivery-recovery-complete" {
					key = managedDeliveryOwnerKey
				}
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", key)
				must(t, err)
			}
			if s.EnrollManagedDeliveryRecovery(r) == nil || s.RequireManagedDeliveryRecovery(r, protocol.Hash(r.Owner)) == nil {
				t.Fatal("lost committed authority was repaired or accepted")
			}
		})
	}
}

func TestManagedRecoveredDeliveryInitialEnrollment(t *testing.T) {
	path, c, result, review := verificationFixture(t)
	_, err := AdoptPublishedRecoveryVerification(context.Background(), path, c, result.Commit.CommitHash)
	must(t, err)
	revision, err := safefs.NewID("n")
	must(t, err)
	installation, err := safefs.NewID("n")
	must(t, err)
	r := ManagedDeliveryRecovery{Version: 1, Owner: ManagedDeliveryOwner{1, t.TempDir(), installation, review.After, protocol.Hash(c)}, Before: review.Before, After: review.After, CommitHash: result.Commit.CommitHash}
	s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
	must(t, err)
	must(t, s.EnrollManagedDeliveryRecovery(r))
	must(t, s.EnrollManagedDeliveryRecovery(r))
	must(t, s.Close())
	s, err = OpenManagedRecoveredDelivery(path, c, revision, r, protocol.Hash(r.Owner), protocol.Hash("activation"))
	must(t, err)
	must(t, s.Close())
	s, err = OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
	must(t, err)
	defer s.Close()
	must(t, os.Remove(filepath.Join(path, managedDeliveryOwnerFile)))
	if s.EnrollManagedDeliveryRecovery(r) == nil {
		t.Fatal("lost fresh owner repaired")
	}
	if _, err = os.Stat(filepath.Join(path, managedDeliveryOwnerFile)); !os.IsNotExist(err) {
		t.Fatal("missing authority was rewritten", err)
	}
}

func TestManagedRecoveredDeliveryCannotReplaceOriginalOwner(t *testing.T) {
	path, c, revision, r := recoveredDeliveryFixture(t, true)
	s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
	must(t, err)
	defer s.Close()
	r.Owner.Identity = r.After
	must(t, os.Remove(filepath.Join(path, managedDeliveryOwnerFile)))
	if s.EnrollManagedDeliveryRecovery(r) == nil {
		t.Fatal("fresh enrollment bypassed independently retained original owner")
	}
}

type deliveryRecoveryCrashInput struct {
	Path, Revision, Point string
	Config                Config
	Binding               ManagedDeliveryRecovery
}

func TestManagedRecoveredDeliveryCrashChild(t *testing.T) {
	file := os.Getenv("DEPLOY_DELIVERY_RECOVERY_CRASH")
	if file == "" {
		return
	}
	var in deliveryRecoveryCrashInput
	b, err := os.ReadFile(file)
	must(t, err)
	must(t, json.Unmarshal(b, &in))
	s, err := OpenManagedRecoveryVerification(in.Path, in.Config, in.Binding.After, in.Revision, in.Binding.CommitHash)
	must(t, err)
	defer s.Close()
	err = s.enrollManagedDeliveryRecovery(in.Binding, func(point string) error {
		if point == in.Point {
			fmt.Println("ready")
			select {}
		}
		return nil
	})
	t.Fatal("child did not stop at requested boundary", err)
}

func TestManagedRecoveredDeliverySIGKILL(t *testing.T) {
	for _, point := range []string{"delivery-recovery-intent", "delivery-recovery-sql", "delivery-recovery-complete"} {
		t.Run(point, func(t *testing.T) {
			path, c, revision, r := recoveredDeliveryFixture(t, true)
			in := deliveryRecoveryCrashInput{path, revision, point, c, r}
			b, err := json.Marshal(in)
			must(t, err)
			file := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(file, b, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManagedRecoveredDeliveryCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_DELIVERY_RECOVERY_CRASH="+file)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			must(t, cmd.Start())
			scanner := bufio.NewScanner(pipe)
			ready := false
			for scanner.Scan() {
				if scanner.Text() == "ready" {
					ready = true
					break
				}
			}
			if !ready {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("child did not reach durable boundary", scanner.Err())
			}
			must(t, cmd.Process.Kill())
			if err = cmd.Wait(); err == nil {
				t.Fatal("child was not killed")
			}
			s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
			must(t, err)
			defer s.Close()
			must(t, s.EnrollManagedDeliveryRecovery(r))
			must(t, s.RequireManagedDeliveryRecovery(r, protocol.Hash(r.Owner)))
			if !s.VerificationOnly() {
				t.Fatal("crash resume enabled delivery")
			}
			must(t, s.RequireInactiveProducers())
		})
	}
}
