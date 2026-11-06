package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func managedCommandRecoveryFixture(t *testing.T) (string, Config, string, ManagedDeliveryRecovery, ManagedCommandRecovery) {
	t.Helper()
	path, c, revision, delivery := recoveredDeliveryFixtureSetup(t, true, true)
	r := ManagedCommandRecovery{1, delivery.Before, delivery.After, delivery.CommitHash, protocol.Hash("history-disabled"), protocol.Hash(delivery.Owner)}
	s, err := OpenManagedRecoveryVerification(path, c, delivery.After, revision, delivery.CommitHash)
	must(t, err)
	must(t, s.EnrollManagedDeliveryRecovery(delivery))
	must(t, s.Close())
	return path, c, revision, delivery, r
}

func TestManagedCommandRecoveryImportsOriginalHistory(t *testing.T) {
	path, c, revision, delivery, r := managedCommandRecoveryFixture(t)
	s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
	must(t, err)
	defer s.Close()
	if s.RequireManagedCommandHistory(r.PreviousHash) == nil {
		t.Fatal("old epoch admitted before adoption")
	}
	bad := r
	bad.PreviousHash = protocol.Hash("history-enabled")
	if s.EnrollManagedCommandRecovery(bad, delivery) == nil {
		t.Fatal("snapshot enable displaced original disable")
	}
	must(t, s.EnrollManagedCommandRecovery(r, delivery))
	must(t, s.RequireManagedCommandHistory(protocol.Hash(r)))
	head, err := s.commandSQLHead()
	must(t, err)
	if head.Mode != "drain" || head.Identity != r.After || !s.VerificationOnly() {
		t.Fatal("recovery enabled execution", head)
	}
	must(t, s.EnrollManagedCommandRecovery(r, delivery))
	if s.RequireManagedCommandHistory(r.PreviousHash) == nil {
		t.Fatal("old installation history restored")
	}
}

func TestManagedCommandRecoveryInterrupted(t *testing.T) {
	for _, at := range []string{"command-recovery-intent", "command-recovery-sql", "command-recovery-head", "command-recovery-complete"} {
		t.Run(at, func(t *testing.T) {
			path, c, revision, delivery, r := managedCommandRecoveryFixture(t)
			s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
			must(t, err)
			stop := errors.New("interrupted")
			err = s.enrollManagedCommandRecovery(r, delivery, func(p string) error {
				if p == at {
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
			must(t, s.EnrollManagedCommandRecovery(r, delivery))
			must(t, s.RequireManagedCommandHistory(protocol.Hash(r)))
		})
	}
}

func TestManagedCommandRecoveryAnchorLoss(t *testing.T) {
	for _, anchor := range []string{"intent", "completion", "prefix", "file-head", "sql-head", "rollback-head"} {
		t.Run(anchor, func(t *testing.T) {
			path, c, revision, delivery, r := managedCommandRecoveryFixture(t)
			s, err := OpenManagedRecoveryVerification(path, c, r.After, revision, r.CommitHash)
			must(t, err)
			defer s.Close()
			must(t, s.EnrollManagedCommandRecovery(r, delivery))
			names := map[string]string{"intent": protocol.Hash(r) + ".json", "completion": protocol.Hash(r) + "-complete.json", "prefix": r.PreviousHash + ".json", "file-head": "head.json"}
			if anchor == "sql-head" {
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", managedCommandHeadKey)
			} else if anchor == "rollback-head" {
				d, e := s.dir.Sub("managed-commands")
				must(t, e)
				var old managedCommandTransition
				must(t, d.ReadJSON(r.PreviousHash+".json", &old))
				err = d.WriteJSON("head.json", old.After, true)
				d.Close()
			} else {
				err = os.Remove(filepath.Join(path, "managed-commands", names[anchor]))
			}
			must(t, err)
			if s.RequireManagedCommandHistory(protocol.Hash(r)) == nil {
				t.Fatal("lost authority admitted")
			}
			if anchor != "completion" && s.EnrollManagedCommandRecovery(r, delivery) == nil {
				t.Fatal("lost committed anchor recreated")
			}
		})
	}
}
