package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestLegacyOwnershipExcludesSharedDispatchAndRevocation(t *testing.T) {
	s, now, dir := fixture(t)
	ctx := context.Background()
	must(t, s.Revoke(ctx, producer, "generation-one"))
	must(t, s.RestoreLegacy(producer, "generation-one", "legacy-one"))
	before := readOwner(t, s)
	must(t, s.RestoreLegacy(producer, "generation-one", "legacy-one"))
	if !sameOwnership(before, readOwner(t, s)) {
		t.Fatal("exact restoration appended history")
	}
	m := report(*now, "legacy-injected", 1, 1, 1)
	m.Generation = "legacy-one"
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	_, granted, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if granted {
		t.Fatal("shared dispatcher granted a legacy generation")
	}
	check, release, err := AcquireLegacyAuthority(dir, s.Identity(), producer, "legacy-one")
	must(t, err)
	must(t, check())
	if err = s.RevokeLegacy(producer, "legacy-one"); err == nil {
		t.Fatal("revoked during retained legacy I/O")
	}
	release()
	must(t, s.RevokeLegacy(producer, "legacy-one"))
	if _, release, err = AcquireLegacyAuthority(dir, s.Identity(), producer, "legacy-one"); err == nil {
		release()
		t.Fatal("revoked legacy owner granted")
	}
	if err = s.RestoreLegacy(producer, "generation-one", "legacy-one"); err == nil {
		t.Fatal("reactivated tombstone")
	}
	must(t, s.verifyOwnershipPrefix(readOwner(t, s)))
	must(t, s.Close())
	reopened, err := Open(dir, configuration(), false)
	must(t, err)
	reopened.Close()
}

func TestLegacyOwnershipPendingCancellationResumes(t *testing.T) {
	for _, operation := range []string{"stage", "drain", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			s, _, _ := fixture(t)
			must(t, s.Revoke(context.Background(), producer, "generation-one"))
			stop := errors.New("interrupted cancellation")
			s.ownershipFault = func(at string) error {
				if at != "owner" {
					return nil
				}
				var intent ownershipIntent
				if err := s.dir.ReadJSON(ownershipName(producer)+".journal.json", &intent); err != nil {
					return err
				}
				if intent.Transition.Operation == operation {
					return stop
				}
				return nil
			}
			if err := s.RevokeLegacy(producer, "legacy-abandoned"); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			s.ownershipFault = nil
			must(t, s.RevokeLegacy(producer, "legacy-abandoned"))
			if err := s.RestoreLegacy(producer, "generation-one", "legacy-abandoned"); err == nil {
				t.Fatal("cancelled generation reused")
			}
		})
	}
}

func TestLegacyOwnershipPreservesAnotherProducer(t *testing.T) {
	c := configuration()
	other := c.Producers[0]
	other.Producer.ID = "unrelated"
	c.Producers = append(c.Producers, other)
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	s, err := Open(dir, c, true)
	must(t, err)
	defer s.Close()
	ctx := context.Background()
	for _, p := range []protocol.Producer{producer, other.Producer} {
		must(t, s.Stage(p, "generation-one"))
		must(t, s.Activate(p, "generation-one"))
	}
	m := report(time.Now().UTC(), "unrelated-message", 1, 1, 1)
	m.Producer = other.Producer
	before, err := s.Enqueue(ctx, m)
	must(t, err)
	must(t, s.Revoke(ctx, producer, "generation-one"))
	must(t, s.RestoreLegacy(producer, "generation-one", "legacy-one"))
	must(t, s.RevokeLegacy(producer, "legacy-one"))
	after, err := s.Get(ctx, other.Producer, m.ID)
	must(t, err)
	status, err := s.OwnershipStatus(other.Producer, "generation-one")
	must(t, err)
	if protocol.Hash(before) != protocol.Hash(after) || !status.Active {
		t.Fatal("another producer changed", status)
	}
}

func TestLegacyOwnershipRequiresRevocationAndStableLease(t *testing.T) {
	s, _, dir := fixture(t)
	if err := s.RestoreLegacy(producer, "generation-one", "legacy-one"); err == nil {
		t.Fatal("restored over active shared sender")
	}
	must(t, s.Revoke(context.Background(), producer, "generation-one"))
	must(t, s.RestoreLegacy(producer, "generation-one", "legacy-one"))
	check, release, err := AcquireLegacyAuthority(dir, s.Identity(), producer, "legacy-one")
	must(t, err)
	defer release()
	name := filepath.Join(dir, ownershipName(producer)+".lock")
	must(t, os.Rename(name, name+".old"))
	must(t, os.WriteFile(name, nil, 0600))
	if err = check(); err == nil {
		t.Fatal("replaced canonical lease accepted")
	}
}

func TestLegacyRollbackUnsentProofCannotIgnoreAttemptsOrMissingHistory(t *testing.T) {
	s, now, _ := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	m := report(*now, "attempted", 1, 1, 1)
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	j, granted, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !granted {
		t.Fatal("fixture not granted")
	}
	must(t, s.Finish(ctx, j, Result{Ambiguous: true}))
	must(t, s.Revoke(ctx, producer, "generation-one"))
	if err = s.RequireLegacyUnsent(ctx, m); err == nil {
		t.Fatal("ambiguous attempt treated as unsent")
	}
	_, err = s.db.Exec("DELETE FROM messages WHERE id=?", m.ID)
	must(t, err)
	if err = s.RequireLegacyUnsent(ctx, m); err == nil {
		t.Fatal("missing submitted history treated as unsent")
	}
	m.ID, m.Sequence = "never-submitted", 2
	must(t, s.RequireLegacyUnsent(ctx, m))
}

func TestLegacyOwnershipStopsAtCanonicalRecoveryBeforeOwnerMigration(t *testing.T) {
	for _, mutation := range []string{"identity", "missing-identity", "recovery.json", publicationCommitIntent, "schema-migration.json"} {
		t.Run(mutation, func(t *testing.T) {
			s, _, dir := fixture(t)
			must(t, s.Revoke(context.Background(), producer, "generation-one"))
			must(t, s.RestoreLegacy(producer, "generation-one", "legacy-one"))
			check, release, err := AcquireLegacyAuthority(dir, s.Identity(), producer, "legacy-one")
			must(t, err)
			defer release()
			switch mutation {
			case "identity":
				must(t, s.dir.WriteJSON("identity.json", protocol.Identity{}, true))
			case "missing-identity":
				must(t, os.Remove(filepath.Join(dir, "identity.json")))
			default:
				must(t, os.WriteFile(filepath.Join(dir, mutation), []byte("{}"), 0600))
			}
			if err := check(); err == nil {
				t.Fatal("old owner authorized a send during recovery")
			}
			if _, close, err := AcquireLegacyAuthority(dir, s.Identity(), producer, "legacy-one"); err == nil {
				close()
				t.Fatal("old owner admitted a restarted sender during recovery")
			}
		})
	}
}

func TestLegacyOwnershipAuditsHistoryWithoutOpeningGateway(t *testing.T) {
	for _, mode := range []string{"lost-prefix", "hidden-revocation"} {
		t.Run(mode, func(t *testing.T) {
			s, _, dir := fixture(t)
			must(t, s.Revoke(context.Background(), producer, "generation-one"))
			must(t, s.RestoreLegacy(producer, "generation-one", "legacy-one"))
			owner := readOwner(t, s)
			if mode == "lost-prefix" {
				must(t, os.Remove(filepath.Join(dir, ownershipHistoryPath(producer, 1))))
			} else {
				var intent ownershipIntent
				must(t, s.dir.ReadJSON(ownershipName(producer)+".journal.json", &intent))
				must(t, s.RevokeLegacy(producer, "legacy-one"))
				must(t, s.Stage(producer, "later-generation"))
				must(t, os.Remove(filepath.Join(dir, ownershipHistoryPath(producer, owner.Revision+1))))
				must(t, s.dir.WriteJSON(ownershipName(producer)+".json", owner, true))
				must(t, s.dir.WriteJSON(ownershipName(producer)+".journal.json", intent, true))
			}
			if _, release, err := AcquireLegacyAuthority(dir, s.Identity(), producer, "legacy-one"); err == nil {
				release()
				t.Fatal("incomplete canonical history reauthorized native delivery")
			}
		})
	}
}
