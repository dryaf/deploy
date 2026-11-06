package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func managedCommandHistoryFixture(t *testing.T) *Store {
	t.Helper()
	s, _, _, _ := commandFixture(t)
	s.installationRevision = "n-01234567890123456789012345678901"
	s.deliveryActivationHash = protocol.Hash("delivery")
	t.Cleanup(func() { s.Close() })
	return s
}

func TestManagedCommandHistoryLostResponseAndRollback(t *testing.T) {
	for _, point := range []string{"command-intent", "command-sql", "command-head", "command-complete"} {
		t.Run(point, func(t *testing.T) {
			s := managedCommandHistoryFixture(t)
			first, next := protocol.Hash("read"), protocol.Hash("disable")
			must(t, s.CommitManagedCommandAdmission("read", first, ""))
			stop := errors.New("interrupted")
			err := s.commitManagedCommandAdmission("drain", next, first, func(at string) error {
				if at == point {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if s.RequireManagedCommandHistory(first) == nil {
				t.Fatal("old admission accepted retained disable intent")
			}
			must(t, s.CommitManagedCommandAdmission("drain", next, first))
			must(t, s.RequireManagedCommandHistory(next))
			if s.CommitManagedCommandAdmission("read", first, "") == nil {
				t.Fatal("old enable operation undid disable")
			}
			if s.ConfigureManagedCommands("all", next) == nil {
				t.Fatal("mode changed without matching durable authority")
			}
			must(t, s.ConfigureManagedCommands("drain", next))
		})
	}
}

func TestManagedCommandHistoryIndependentAnchorsAndTail(t *testing.T) {
	for _, loss := range []string{"sql", "head", "prefix", "completion", "rolled-back-head"} {
		t.Run(loss, func(t *testing.T) {
			s := managedCommandHistoryFixture(t)
			first, next := protocol.Hash("read"), protocol.Hash("disable")
			must(t, s.CommitManagedCommandAdmission("read", first, ""))
			old, err := s.commandSQLHead()
			must(t, err)
			must(t, s.CommitManagedCommandAdmission("drain", next, first))
			d, err := s.dir.Sub("managed-commands")
			must(t, err)
			defer d.Close()
			pin := next
			switch loss {
			case "sql":
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", managedCommandHeadKey)
				must(t, err)
			case "head":
				must(t, os.Remove(filepath.Join(d.File().Name(), "head.json")))
			case "prefix":
				must(t, os.Remove(filepath.Join(d.File().Name(), first+".json")))
			case "completion":
				must(t, os.Remove(filepath.Join(d.File().Name(), next+"-complete.json")))
			case "rolled-back-head":
				must(t, d.WriteJSON("head.json", old, true))
				raw, err := json.Marshal(old)
				must(t, err)
				_, err = s.db.Exec("UPDATE metadata SET value=? WHERE key=?", string(raw), managedCommandHeadKey)
				must(t, err)
				pin = first
			}
			if s.RequireManagedCommandHistory(pin) == nil {
				t.Fatal("lost or rolled-back authority accepted")
			}
		})
	}
}
