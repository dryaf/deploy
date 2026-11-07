package workerctl

import (
	"fmt"
	"slices"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
)

// Copy user-selected aliases, audiences and permission scopes. Descriptor
// implementations, generations, revision hashes and admission remain retained
// domain authority and never become YAML instructions.
func importCommandPolicy(input install.RuntimeInput) (config.CommandDeclaration, error) {
	c := input.Worker.Interaction
	result := config.CommandDeclaration{Enabled: true, Mode: "read", Workers: map[string]string{}, Conversations: map[string]config.ConversationDeclaration{}}
	if c == nil {
		return result, fmt.Errorf("command policy unavailable")
	}
	if err := c.Validate(); err != nil {
		return result, err
	}
	aliases := map[string]string{}
	for _, w := range c.Workers {
		result.Workers[w.Alias], aliases[w.Producer.ID] = w.Producer.ID, w.Alias
		for _, command := range w.Commands {
			if command.Effect == "mutation" {
				result.Mode = "mutation"
			}
		}
	}
	for _, d := range c.Destinations {
		if d.Provider != "telegram" || input.Telegram == nil || input.Telegram.Account != d.Account {
			return result, fmt.Errorf("command account import requires its provider ownership/handoff workflow")
		}
		result.Conversations[d.ID] = config.ConversationDeclaration{Provider: d.Provider, Account: d.Account, NativeID: d.NativeID, Thread: d.Thread}
	}
	for _, p := range c.Permissions {
		alias, ok := aliases[p.Worker]
		if !ok {
			return result, fmt.Errorf("command permission has no retained worker alias")
		}
		for _, permission := range p.Permissions {
			if permission != "read" {
				result.Mode = "mutation"
			}
		}
		result.Permissions = append(result.Permissions, config.PermissionDeclaration{Actor: p.Actor, Conversation: p.Conversation, Worker: alias, Permissions: slices.Clone(p.Permissions), Scope: config.PermissionScope{WholeWorker: p.Scope.WholeWorker, Subjects: slices.Clone(p.Scope.Subjects)}})
	}
	return result, nil
}
