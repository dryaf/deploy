package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func admissionState(t *testing.T, checks []AdmissionCheck, scope, stage, state string) {
	t.Helper()
	for _, check := range checks {
		if check.Scope == scope && check.Stage == stage {
			if check.State != state {
				t.Fatalf("%s/%s = %+v; want %s", scope, stage, check, state)
			}
			return
		}
	}
	t.Fatalf("missing check %s/%s: %+v", scope, stage, checks)
}

func TestManagedAdmissionInitializedReadOnly(t *testing.T) {
	s, _, path := fixture(t)
	c, id := s.config, s.Identity()
	if _, err := ReviewManagedAdmission(context.Background(), path, c, id, "", nil); err == nil {
		t.Fatal("review ignored a held worker lease")
	}
	must(t, s.Close())
	before := retainedTreeHash(t, path)
	one, err := ReviewManagedAdmission(context.Background(), path, c, id, "", nil)
	must(t, err)
	admissionState(t, one.Checks, "producer/"+producer.ID, "inactive_generation", "blocked")
	admissionState(t, one.Checks, "recovery", "publication", "not_required")
	if retainedTreeHash(t, path) != before {
		t.Fatal("review changed initialized state")
	}
	s, err = Open(path, c, false)
	must(t, err)
	must(t, s.Revoke(context.Background(), producer, "generation-one"))
	must(t, s.Close())
	before = retainedTreeHash(t, path)
	two, err := ReviewManagedAdmission(context.Background(), path, c, id, "", nil)
	must(t, err)
	admissionState(t, two.Checks, "producer/"+producer.ID, "inactive_generation", "satisfied")
	admissionState(t, two.Checks, "producer/"+producer.ID, "activation", "blocked")
	if retainedTreeHash(t, path) != before {
		t.Fatal("review changed generation fences")
	}
}

func TestManagedAdmissionSignalPublication(t *testing.T) {
	path, c, _, publication, cursor := signalRuntimeGatewayFixture(t)
	ctx := context.Background()
	adopted, err := AdoptPublishedRecoveryVerification(ctx, path, c, publication.Commit.CommitHash)
	must(t, err)
	before := retainedTreeHash(t, path)
	review, err := ReviewManagedAdmission(ctx, path, c, adopted.After, publication.Commit.CommitHash, nil)
	must(t, err)
	admissionState(t, review.Checks, "recovery", "publication", "satisfied")
	admissionState(t, review.Checks, "commands", "request_seal", "satisfied")
	admissionState(t, review.Checks, "intake/signal/"+cursor.Handoff.Target.Account, "cursor", "satisfied")
	if protocol.Hash(review.Signal[cursor.Handoff.Target.Account]) != protocol.Hash(cursor) || retainedTreeHash(t, path) != before {
		t.Fatal("review changed Signal publication/cursor")
	}
	root, err := privateDirectory(path)
	must(t, err)
	db, err := snapshotDB(root, "state.db", false)
	must(t, err)
	changed, err := db.Exec("UPDATE reconciliation_request_fences SET body='{}' WHERE key=(SELECT key FROM reconciliation_request_fences LIMIT 1)")
	must(t, err)
	count, err := changed.RowsAffected()
	must(t, err)
	if count != 1 {
		t.Fatal("fixture did not contain historical request fences")
	}
	must(t, db.Close())
	before = retainedTreeHash(t, path)
	review, err = ReviewManagedAdmission(ctx, path, c, adopted.After, publication.Commit.CommitHash, nil)
	must(t, err)
	admissionState(t, review.Checks, "commands", "request_seal", "blocked")
	if retainedTreeHash(t, path) != before {
		t.Fatal("review repaired a changed request fence")
	}
	db, err = snapshotDB(root, "state.db", false)
	must(t, err)
	_, err = db.Exec("DELETE FROM reconciliation_request_seal")
	must(t, err)
	must(t, db.Close())
	must(t, root.Close())
	before = retainedTreeHash(t, path)
	review, err = ReviewManagedAdmission(ctx, path, c, adopted.After, publication.Commit.CommitHash, nil)
	must(t, err)
	admissionState(t, review.Checks, "commands", "request_seal", "blocked")
	admissionState(t, review.Checks, "intake/signal/"+cursor.Handoff.Target.Account, "cursor", "blocked")
	if len(review.Signal) != 0 || retainedTreeHash(t, path) != before {
		t.Fatal("damaged seal was repaired or exposed as cursor authority")
	}
}

func TestManagedAdmissionTelegramFreshnessAndRenewal(t *testing.T) {
	f := newTelegramRenewalFixture(t)
	ctx := context.Background()
	adopted, err := AdoptPublishedRecoveryVerification(ctx, f.Path, f.Config, f.Publication.Commit.CommitHash)
	must(t, err)
	account := f.Original.Drain.Account
	scope := "intake/telegram/" + account
	check := func(clock telegramRenewalClock, renewals map[string]string, state string) ManagedAdmissionReview {
		t.Helper()
		before := retainedTreeHash(t, f.Path)
		review, err := reviewManagedAdmission(ctx, f.Path, f.Config, adopted.After, f.Publication.Commit.CommitHash, renewals, clock)
		must(t, err)
		admissionState(t, review.Checks, scope, "handoff", state)
		if retainedTreeHash(t, f.Path) != before {
			t.Fatal("review changed Telegram evidence")
		}
		return review
	}
	clock := telegramRenewalClock{func() time.Time { return f.Original.NotBefore.Add(time.Second) }, func() (string, float64, error) { return f.Original.Drain.BootID, f.Original.Drain.Uptime + 6, nil }}
	one := check(clock, nil, "satisfied")
	if one.Telegram[account].Cursor != f.Original {
		t.Fatal("original cursor changed")
	}
	check(f.clock(), nil, "blocked") // original is twenty minutes old
	renewed, err := f.run(nil)
	must(t, err)
	f.At, f.Up = f.At.Add(6*time.Second), f.Up+6
	check(f.clock(), nil, "blocked") // no fallback once renewal exists
	two := check(f.clock(), map[string]string{account: renewed.RenewalHash}, "satisfied")
	if two.Telegram[account].RenewalHash != renewed.RenewalHash || two.Telegram[account].IntakeEnabled {
		t.Fatal(two)
	}
	check(f.clock(), map[string]string{account: protocol.Hash("wrong-renewal")}, "blocked")
	f.Boot = "different-boot"
	check(f.clock(), map[string]string{account: renewed.RenewalHash}, "blocked")
}

func TestManagedAdmissionRejectsChangedState(t *testing.T) {
	for _, kind := range []string{"identity", "schema", "enrollment", "missing-lock", "pending-migration", "hidden-recovery", "ownership-history"} {
		t.Run(kind, func(t *testing.T) {
			s, _, path := fixture(t)
			c, id := s.config, s.Identity()
			must(t, s.Close())
			d, err := privateDirectory(path)
			must(t, err)
			defer d.Close()
			switch kind {
			case "identity":
				id.Recovery = "changed-epoch"
			case "missing-lock":
				must(t, os.Remove(filepath.Join(path, "worker.lock")))
			case "pending-migration":
				must(t, d.WriteJSON("schema-migration.json", struct{}{}, false))
			case "hidden-recovery":
				must(t, d.WriteJSON("recovery.json", struct{}{}, false))
			case "ownership-history":
				must(t, os.Remove(filepath.Join(path, ownershipHistoryPath(producer, 1))))
			default:
				db, err := snapshotDB(d, "state.db", false)
				must(t, err)
				query := "PRAGMA user_version=9"
				if kind == "enrollment" {
					query = "UPDATE producers SET epoch='other'"
				}
				_, err = db.Exec(query)
				must(t, err)
				must(t, db.Close())
			}
			before := retainedTreeHash(t, path)
			if _, err := ReviewManagedAdmission(context.Background(), path, c, id, "", nil); err == nil {
				t.Fatal("changed state accepted")
			}
			if retainedTreeHash(t, path) != before {
				t.Fatal("review repaired changed state")
			}
		})
	}
}

func TestManagedAdmissionDoesNotIgnoreWAL(t *testing.T) {
	s, _, path := fixture(t)
	c, id := s.config, s.Identity()
	must(t, s.Close())
	root, err := privateDirectory(path)
	must(t, err)
	defer root.Close()
	db, err := snapshotDB(root, "state.db", false)
	must(t, err)
	defer db.Close()
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	must(t, err)
	_, err = db.Exec("UPDATE producers SET epoch='uncheckpointed' WHERE id=?", producer.ID)
	must(t, err)
	info, err := os.Stat(filepath.Join(path, "state.db-wal"))
	must(t, err)
	if info.Size() == 0 {
		t.Fatal("fixture did not retain a WAL")
	}
	before := retainedTreeHash(t, path)
	if _, err := ReviewManagedAdmission(context.Background(), path, c, id, "", nil); err == nil {
		t.Fatal("review ignored uncheckpointed enrollment change")
	}
	if retainedTreeHash(t, path) != before {
		t.Fatal("review checkpointed/changed pending WAL")
	}
}
