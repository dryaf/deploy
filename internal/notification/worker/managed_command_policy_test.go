package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestManagedCommandPolicyInterruptedAndAnchored(t *testing.T) {
	for _, point := range []string{"command-intent", "command-sql", "command-head", "command-complete"} {
		t.Run(point, func(t *testing.T) {
			s := managedCommandHistoryFixture(t)
			first, next, enable := protocol.Hash("disabled"), protocol.Hash("narrow"), protocol.Hash("enable")
			must(t, s.CommitManagedCommandAdmission("drain", first, ""))
			stop := errors.New("interrupted")
			err := s.commitManagedCommandPolicy("drain", next, first, "[]", func(at string) error {
				if at == point {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if s.RequireManagedCommandHistory(first) == nil {
				t.Fatal("old authority accepted narrowing intent")
			}
			must(t, s.CommitManagedCommandPolicy(next, first, "[]"))
			must(t, s.CommitManagedCommandAdmission("all", enable, next))
			must(t, s.ConfigureManagedCommands("all", enable))
			if len(s.commandConfig().Permissions) != 0 || len(s.config.Interaction.Permissions) == 0 {
				t.Fatal("policy missing or canonical config modified")
			}
			head, err := s.commandSQLHead()
			must(t, err)
			if head.Version != 2 || head.Policy != "[]" {
				t.Fatal(head)
			}
			// An old runtime's strict head decoder cannot silently ignore the policy.
			raw, err := json.Marshal(head)
			must(t, err)
			var old struct {
				Version                    int
				Identity                   protocol.Identity
				Hash, Mode, ActivationHash string
			}
			if protocol.Decode(raw, &old, 65536) == nil {
				t.Fatal("old head reader ignored policy")
			}
		})
	}
	for _, loss := range []string{"sql", "head", "completion", "rollback"} {
		t.Run(loss, func(t *testing.T) {
			s := managedCommandHistoryFixture(t)
			first, next := protocol.Hash("disabled"), protocol.Hash("narrow")
			must(t, s.CommitManagedCommandAdmission("drain", first, ""))
			old, err := s.commandSQLHead()
			must(t, err)
			must(t, s.CommitManagedCommandPolicy(next, first, "[]"))
			pin := next
			d, err := s.dir.Sub("managed-commands")
			must(t, err)
			defer d.Close()
			switch loss {
			case "sql":
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", managedCommandHeadKey)
			case "head":
				err = os.Remove(filepath.Join(d.File().Name(), "head.json"))
			case "completion":
				err = os.Remove(filepath.Join(d.File().Name(), next+"-complete.json"))
			case "rollback":
				must(t, d.WriteJSON("head.json", old, true))
				raw, _ := json.Marshal(old)
				_, err = s.db.Exec("UPDATE metadata SET value=? WHERE key=?", string(raw), managedCommandHeadKey)
				pin = first
			}
			must(t, err)
			if s.RequireManagedCommandHistory(pin) == nil {
				t.Fatal("lost narrowing anchors accepted")
			}
		})
	}
}
