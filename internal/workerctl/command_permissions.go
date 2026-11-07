package workerctl

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func commandPermissionsJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-command-permissions"
}

func validateCommandPermissionsPolicy(before, after config.DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || before.Notification.Commands.Enabled || after.Notification.Commands.Enabled {
		return fmt.Errorf("disable commands with the existing policy before changing permissions")
	}
	n := *after.Notification
	if reflect.DeepEqual(n.Commands.Permissions, before.Notification.Commands.Permissions) {
		return fmt.Errorf("command permissions are unchanged")
	}
	n.Commands.Permissions = before.Notification.Commands.Permissions
	if !reflect.DeepEqual(n, *before.Notification) || !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("change command permissions separately from other worker settings")
	}
	return nil
}

func commandEnrollmentJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-command-enrollment"
}

func validateCommandEnrollmentPolicy(before, after config.DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || before.Notification.Commands.Enabled || after.Notification.Commands.Enabled {
		return fmt.Errorf("disable commands before changing command enrollment")
	}
	n := *after.Notification
	if reflect.DeepEqual(n.Commands, before.Notification.Commands) {
		return fmt.Errorf("command enrollment requires changed mode, aliases, conversations or permissions")
	}
	n.Commands = before.Notification.Commands
	if !reflect.DeepEqual(n, *before.Notification) || !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("change command enrollment separately from other worker settings")
	}
	return nil
}

func commandPermissionsRequest(paths ResolvedPaths, prior OperationJournal, desired config.DesiredWorkers, receiver backup.CommandReceiverRequest) (install.CommandPolicyRequest, error) {
	return commandPolicyRequest(paths, prior, desired, receiver, false)
}

func commandPolicyRequest(paths ResolvedPaths, prior OperationJournal, desired config.DesiredWorkers, receiver backup.CommandReceiverRequest, enrollment bool, previous ...config.DesiredWorkers) (install.CommandPolicyRequest, error) {
	b := NotificationBindings{Producers: map[string]ProducerBinding{}, Conversations: map[string]interaction.Destination{}}
	for _, worker := range receiver.Policy.Workers {
		if worker.Producer == receiver.Producer {
			b.Producers["backup-worker"] = ProducerBinding{Producer: worker.Producer, Generation: worker.Generation, Commands: worker.Commands}
		}
	}
	for _, destination := range receiver.Policy.Destinations {
		b.Conversations[destination.ID] = destination
	}
	declaration := desired.Notification.Commands
	restore := false
	if len(previous) == 1 {
		old := previous[0].Notification.Commands
		old.Mode = "mutation"
		priorPolicy, e := compileCommandPolicy(old, b)
		if e != nil {
			return install.CommandPolicyRequest{}, e
		}
		next := declaration
		next.Mode = "mutation"
		nextPolicy, e := compileCommandPolicy(next, b)
		if e != nil {
			return install.CommandPolicyRequest{}, e
		}
		restore = !commandRightsSubset(&priorPolicy, &nextPolicy)
		if restore && !commandRightsSubset(&receiver.Policy, &nextPolicy) {
			return install.CommandPolicyRequest{}, fmt.Errorf("new command authority requires a receiver permission migration; only originally admitted rights can be restored")
		}
	}
	enrollment = enrollment || restore
	if declaration.Mode != "read" && declaration.Mode != "mutation" {
		return install.CommandPolicyRequest{}, fmt.Errorf("command mode must be read or mutation")
	}
	if declaration.Mode == "mutation" && receiver.Version != 2 {
		return install.CommandPolicyRequest{}, fmt.Errorf("mutation mode requires a version-2 receiver originally admitted for incident commands; receiver upgrade admission is still required")
	}
	if declaration.Mode == "read" {
		for _, permission := range declaration.Permissions {
			for _, right := range permission.Permissions {
				if right != "read" {
					return install.CommandPolicyRequest{}, fmt.Errorf("remove mutation permissions before selecting read mode")
				}
			}
		}
	}
	// Modes restrict gateway intake; they never replace the specialist's
	// immutable command contract. Keep its full descriptors in an overlay.
	if enrollment {
		declaration.Mode = "mutation"
	}
	compiled, err := compileCommandPolicy(declaration, b)
	if err != nil {
		return install.CommandPolicyRequest{}, err
	}
	if compiled.Permissions == nil {
		compiled.Permissions = []interaction.Permission{}
	}
	before, _ := canonicalNotificationReceipt(prior)
	operation := "narrow-commands"
	var policy any = compiled.Permissions
	if enrollment {
		// Include the predecessor so returning to a former alias does not reuse
		// a retired policy revision. Retry reconstruction uses the same pin.
		compiled.Revision = protocol.Hash(struct{ Previous, Policy string }{before, compiled.Revision})
		operation, policy = "migrate-commands", compiled
		if restore {
			operation = "restore-command-permissions"
		}
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return install.CommandPolicyRequest{}, err
	}
	return install.CommandPolicyRequest{Root: paths.Installation, ManifestHash: before, Operation: operation, Policy: string(raw)}, nil
}

func commandRightsSubset(before, after *interaction.Config) bool {
	for _, permission := range after.Permissions {
		for _, right := range permission.Permissions {
			scope, ok := before.Authorize(permission.Actor, permission.Conversation, permission.Worker, right)
			if !ok || !scope.Allows(permission.Scope) {
				return false
			}
		}
	}
	return true
}
