package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func commandEnrollmentJSON(t *testing.T, c *interaction.Config) string {
	t.Helper()
	b, err := json.Marshal(c)
	must(t, err)
	return string(b)
}

func TestManagedCommandEnrollmentFencesPendingAndRetainsAliases(t *testing.T) {
	for _, point := range []string{"command-intent", "command-sql", "command-head", "command-complete"} {
		t.Run(point, func(t *testing.T) {
			s := managedCommandHistoryFixture(t)
			ctx := context.Background()
			before := s.config.Interaction
			var next interaction.Config
			must(t, json.Unmarshal([]byte(commandEnrollmentJSON(t, before)), &next))
			next.Revision, next.Workers[0].Alias, next.Destinations[0].Generation = "two", "backup/renamed", "two"
			raw := commandEnrollmentJSON(t, &next)
			in := interaction.Inbound{Provider: "telegram", Account: "bot-1", NativeID: "update-1", Actor: "actor-1", Conversation: "private", DestinationFingerprint: before.Destinations[0].Fingerprint(), SentAt: s.now(), Text: "backup/storage status"}
			body, err := json.Marshal(in)
			must(t, err)
			_, err = s.db.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,?,?,?,?)", in.Account, 1, protocol.Hash(in), string(body), "pending")
			must(t, err)
			first, nextHash := protocol.Hash("disabled"), protocol.Hash("migrated")
			must(t, s.CommitManagedCommandAdmission("drain", first, ""))
			stop := errors.New("interrupted")
			err = s.commitManagedCommandPolicy("drain", nextHash, first, raw, func(at string) error {
				if at == point {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if s.RequireManagedCommandHistory(first) == nil {
				t.Fatal("old runtime accepted migration intent")
			}
			must(t, s.CommitManagedCommandPolicy(nextHash, first, raw))
			var state, mapping string
			must(t, s.db.QueryRow("SELECT state FROM provider_inbox WHERE account=?", in.Account).Scan(&state))
			must(t, s.db.QueryRow("SELECT command_id FROM command_inbox WHERE request_key=?", in.RequestKey()).Scan(&mapping))
			if state != "rejected" || mapping != "Q-"+in.RequestKey() {
				t.Fatal("pending input was not durably fenced", state, mapping)
			}
			for _, change := range []func(*interaction.Config){
				func(c *interaction.Config) { c.Destinations[0].Generation = "one" },
				func(c *interaction.Config) { c.Destinations[0].NativeID = "another-audience" },
				func(c *interaction.Config) { c.Revision = "two" },
				func(c *interaction.Config) { c.Permissions[0].Scope = interaction.Scope{WholeWorker: true} },
			} {
				var bad interaction.Config
				must(t, json.Unmarshal([]byte(raw), &bad))
				bad.Revision = "three"
				change(&bad)
				if s.CommitManagedCommandPolicy(protocol.Hash(bad), nextHash, commandEnrollmentJSON(t, &bad)) == nil {
					t.Fatal("invalid enrollment transition accepted", bad)
				}
			}
			enable := protocol.Hash("reenable")
			must(t, s.CommitManagedCommandAdmission("all", enable, nextHash))
			must(t, s.ConfigureManagedCommands("all", enable))
			must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
			if _, err = s.AdmitCommand(ctx, in); err == nil {
				t.Fatal("old input executed")
			}
			in.NativeID = "update-2"
			in.Text = "backup/renamed status"
			in.DestinationFingerprint = next.Destinations[0].Fingerprint()
			in.SentAt = s.now().Add(-time.Millisecond)
			v, err := s.AdmitCommand(ctx, in)
			must(t, err)
			if v.Command.Producer != producer || v.Command.Destination != next.Destinations[0] {
				t.Fatal(v)
			}
			var reserved int
			must(t, s.db.QueryRow("SELECT count(*) FROM command_aliases WHERE alias IN ('backup/storage','backup/renamed')").Scan(&reserved))
			if reserved != 2 {
				t.Fatal("alias history lost", reserved)
			}
		})
	}
}
