package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestManagedCommandGrantRestoresOnlyOriginalRights(t *testing.T) {
	for _, point := range []string{"command-intent", "command-sql", "command-head", "command-complete"} {
		t.Run(point, func(t *testing.T) {
			s := managedCommandHistoryFixture(t)
			initial, reduced, restored := protocol.Hash("drain"), protocol.Hash("narrow"), protocol.Hash("restore")
			must(t, s.CommitManagedCommandAdmission("drain", initial, ""))
			must(t, s.CommitManagedCommandPolicy(reduced, initial, "[]"))
			next := *s.config.Interaction
			next.Revision = "restored"
			raw := commandEnrollmentJSON(t, &next)
			if s.CommitManagedCommandPolicy(restored, reduced, raw) == nil {
				t.Fatal("ordinary migration granted rights")
			}
			destination := next.Destinations[0]
			input := interaction.Inbound{Provider: "telegram", Account: "bot-1", NativeID: "pending", Actor: "actor-1", Conversation: destination.ID, DestinationFingerprint: destination.Fingerprint(), SentAt: s.now(), Text: "backup/storage status"}
			body, err := json.Marshal(input)
			must(t, err)
			_, err = s.db.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,?,?,?,?)", input.Account, 1, protocol.Hash(input), string(body), "pending")
			must(t, err)
			stop := errors.New("interrupted")
			err = s.commitManagedCommandPolicy("drain", restored, reduced, raw, func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}, true)
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if s.RequireManagedCommandHistory(reduced) == nil {
				t.Fatal("old authority survived grant intent")
			}
			if s.CommitManagedCommandPolicy(restored, reduced, raw) == nil {
				t.Fatal("retry silently changed grant into narrowing")
			}
			must(t, s.CommitManagedCommandGrant(restored, reduced, raw))
			must(t, s.CommitManagedCommandGrant(restored, reduced, raw))
			var state, mapping string
			must(t, s.db.QueryRow("SELECT state FROM provider_inbox WHERE account=?", input.Account).Scan(&state))
			must(t, s.db.QueryRow("SELECT command_id FROM command_inbox WHERE request_key=?", input.RequestKey()).Scan(&mapping))
			if state != "rejected" || mapping != "Q-"+input.RequestKey() {
				t.Fatal("grant reinterpreted earlier input", state, mapping)
			}
			// A later ordinary reduction and explicit restoration retain all predecessors.
			empty := next
			empty.Revision = "reduced-again"
			empty.Permissions = []interaction.Permission{}
			narrowed := protocol.Hash("reduced-again")
			must(t, s.CommitManagedCommandPolicy(narrowed, restored, commandEnrollmentJSON(t, &empty)))
			next.Revision = "restored-again"
			last := protocol.Hash(next)
			must(t, s.CommitManagedCommandGrant(last, narrowed, commandEnrollmentJSON(t, &next)))
			enabled := protocol.Hash("enabled")
			must(t, s.CommitManagedCommandAdmission("all", enabled, last))
			must(t, s.ConfigureManagedCommands("all", enabled))
			must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
			if _, err = s.AdmitCommand(context.Background(), input); err == nil {
				t.Fatal("old input revived")
			}
			input.NativeID = "fresh"
			admitted, err := s.AdmitCommand(context.Background(), input)
			must(t, err)
			if admitted.Command.Producer != producer {
				t.Fatal(admitted)
			}
			if protocol.Hash(s.config.Interaction) == protocol.Hash(&next) {
				t.Fatal("canonical owner config rewritten")
			}
			// The grant cannot disappear from its independent history.
			must(t, os.Remove(filepath.Join(s.dir.File().Name(), "managed-commands", restored+"-complete.json")))
			if s.RequireManagedCommandHistory(enabled) == nil {
				t.Fatal("missing grant history accepted")
			}
		})
	}
}

func TestManagedCommandGrantRefusesExpansionAndEnrollmentChanges(t *testing.T) {
	for _, kind := range []string{"actor", "scope", "audience", "alias", "revision", "worker", "live"} {
		t.Run(kind, func(t *testing.T) {
			s := managedCommandHistoryFixture(t)
			first, reduced := protocol.Hash("drain"), protocol.Hash("reduce")
			must(t, s.CommitManagedCommandAdmission("drain", first, ""))
			must(t, s.CommitManagedCommandPolicy(reduced, first, "[]"))
			var next interaction.Config
			must(t, json.Unmarshal([]byte(commandEnrollmentJSON(t, s.config.Interaction)), &next))
			next.Revision = "restore"
			switch kind {
			case "actor":
				next.Permissions[0].Actor = "new-actor"
			case "scope":
				next.Permissions[0].Scope = interaction.Scope{WholeWorker: true}
			case "audience":
				next.Destinations[0].NativeID = "other"
			case "alias":
				next.Workers[0].Alias = "renamed"
			case "revision":
				next.Revision = s.config.Interaction.Revision
			case "worker":
				next.Workers[0].Generation = "other"
			case "live":
				enabled := protocol.Hash("enable")
				must(t, s.CommitManagedCommandAdmission("all", enabled, reduced))
				reduced = enabled
			}
			if s.CommitManagedCommandGrant(protocol.Hash(next), reduced, commandEnrollmentJSON(t, &next)) == nil {
				t.Fatal("unsupported grant admitted")
			}
			must(t, s.RequireManagedCommandHistory(reduced))
		})
	}
}
