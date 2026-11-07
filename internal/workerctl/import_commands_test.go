package workerctl

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func TestCommandImportPreservesAliasesAudiencesAndPermissions(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "mutation"}[mutation], func(t *testing.T) {
			producer := protocol.Producer{ID: "durable-backup", Epoch: "private-producer-epoch"}
			commands := map[string]interaction.Descriptor{"status": {ID: "backup.status", Version: 1, Permission: "read", Effect: "read", Scoped: true}}
			permission := "read"
			if mutation {
				commands["refresh"] = interaction.Descriptor{ID: "backup.refresh", Version: 1, Permission: "job.request", Effect: "mutation", Scoped: true}
				permission = "job.request"
			}
			conversation := interaction.Destination{ID: "ops", Generation: "private-conversation-generation", Provider: "telegram", Account: "bot", NativeID: "-123", Thread: "45"}
			before := interaction.Config{Revision: "private-policy-revision", Workers: []interaction.Worker{{Alias: "backups", Producer: producer, Generation: "private-worker-generation", Commands: commands}}, Destinations: []interaction.Destination{conversation}, Permissions: []interaction.Permission{{Actor: "operator", Conversation: "ops", Worker: producer.ID, Permissions: []string{permission}, Scope: interaction.Scope{Subjects: []string{"db"}}}}}
			input := install.RuntimeInput{Telegram: &provider.TelegramAccount{Account: "bot", TokenFile: "/private/token", BotName: "fixture_bot"}}
			input.Worker.Interaction = &before
			candidate, err := importCommandPolicy(input)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Mode != map[bool]string{false: "read", true: "mutation"}[mutation] || candidate.Workers["backups"] != producer.ID || candidate.Permissions[0].Worker != "backups" {
				t.Fatal("command policy changed")
			}
			after, err := compileCommandPolicy(candidate, NotificationBindings{Producers: map[string]ProducerBinding{producer.ID: {Producer: producer, Generation: before.Workers[0].Generation, Commands: commands}}, Conversations: map[string]interaction.Destination{"ops": conversation}})
			if err != nil {
				t.Fatal(err)
			}
			after.Revision = before.Revision
			if !reflect.DeepEqual(before, after) {
				t.Fatal("import changed command audiences, manifests or permissions")
			}
			raw, _ := json.Marshal(candidate)
			if bytes.Contains(raw, []byte("private-")) {
				t.Fatal("generated command authority exported")
			}
			input.Telegram.Account = "different-account"
			if _, err = importCommandPolicy(input); err == nil {
				t.Fatal("unowned conversation accepted")
			}
		})
	}
}
