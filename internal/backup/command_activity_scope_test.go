//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandNotificationScopeKeepsReportingAndResumesLostReply(t *testing.T) {
	n := &config.NotificationDeclaration{Commands: config.CommandDeclaration{Enabled: true, Mode: "read"}}
	home, before, _, _ := sharedSourceExpansionFixtureWithNotification(t, n)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var initial WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &initial))
	cutoverPath := filepath.Join(home, ".local/state/deploy/backup-worker/state", backupCutoverFile)
	cutover, err := os.ReadFile(cutoverPath)
	testutil.Must(t, err)
	raw, err := json.Marshal(before.Declaration.Desired)
	testutil.Must(t, err)
	var desired config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(raw, &desired))
	stateDir, err := root.Sub(".local/state/deploy/backup-worker/state")
	testutil.Must(t, err)
	lease, err := stateDir.Lock("schema.lock", true, true)
	testutil.Must(t, err)
	lease.Close()
	stateDir.Close()
	ctx := context.Background()
	for _, enabled := range []bool{false, true, false} {
		var prior WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &prior))
		desired.Notification.Commands.Enabled = enabled
		request := VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: prior.Declaration.NotificationDesiredHash, BeforeGateway: notify.Hash(prior), Desired: desired}
		gateway := notify.Hash(request)
		lost := errors.New("gateway committed but reply lost")
		calls := 0
		callback := func(p ProducerReservation) (string, error) {
			if p != *initial.Producer {
				t.Fatal("command activity replaced producer")
			}
			calls++
			if calls == 1 {
				return "", lost
			}
			return gateway, nil
		}
		if _, err = ReconcileCommandNotificationScope(ctx, home, request, callback); !errors.Is(err, lost) {
			t.Fatal("lost command response not retained", err)
		}
		for range 2 {
			receipt, err := ReconcileCommandNotificationScope(ctx, home, request, callback)
			testutil.Must(t, err)
			if receipt != gateway {
				t.Fatal("command scope receipt changed")
			}
		}
		if calls != 2 {
			t.Fatal("completed command scope replayed gateway", calls)
		}
		var current WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &current))
		origin, err := commandScopeOrigin(root, current, initial.Declaration.NotificationDesiredHash)
		testutil.Must(t, err)
		if origin.Declaration.DesiredHash != initial.Declaration.DesiredHash {
			t.Fatal("command scope lost original reporting declaration")
		}
		state, err := openInstalledBackupState(home, false)
		testutil.Must(t, err)
		testutil.Must(t, requireCommandScopeOwner(state, current))
		if requireLegacyNotificationOwner(state) == nil {
			t.Fatal("command toggle revived direct sending")
		}
		owner, err := currentSharedBackupOwner(state)
		testutil.Must(t, err)
		state.Close()
		var retained backupCutoverIntent
		testutil.Must(t, json.Unmarshal(cutover, &retained))
		manifest := install.ManagedManifest{Root: initial.Declaration.NotificationRoot, Mode: "delivery", Identity: owner.Binding.Identity, Commands: &install.ManagedCommandAdmission{Mode: "drain", Hash: gateway}}
		if enabled {
			manifest.Commands.Mode = "read"
		}
		_, spec, err := prepareFreshSharedReportingUsing(ctx, home, current.Declaration.DesiredHash, notify.Hash(manifest), func(_ context.Context, root, now, original string) (install.ManagedManifest, error) {
			if root != manifest.Root || now != notify.Hash(manifest) || original != retained.Spec.Managed.ManifestHash {
				t.Fatal("reporting continuation lost exact pins")
			}
			return manifest, nil
		})
		testutil.Must(t, err)
		if spec.Version != 5 || notify.Hash(spec) != notify.Hash(retained.Spec) {
			t.Fatal("command toggle replaced reporting admission")
		}
		testutil.Must(t, requireFreshBackupReportingSetup(root, home, current, spec))
		testutil.Must(t, validateSetupDesired(current.Declaration))
		after, err := os.ReadFile(cutoverPath)
		testutil.Must(t, err)
		if string(after) != string(cutover) {
			t.Fatal("command activity rewrote reporting cutover")
		}
		pin, complete, err := InspectVerificationNotificationScope(home, request.Operation, request.BeforeGateway, current.Declaration.NotificationDesiredHash)
		testutil.Must(t, err)
		if !complete || pin != gateway {
			t.Fatal("command scope inspection lost completion")
		}
	}
}

func TestCommandNotificationScopeRejectsUnrelatedPolicyAndLostHistory(t *testing.T) {
	for _, damage := range []string{"mode", "backup", "receipt", "source", "owner"} {
		t.Run(damage, func(t *testing.T) {
			home, before, _, _ := sharedSourceExpansionFixtureWithNotification(t, &config.NotificationDeclaration{Commands: config.CommandDeclaration{Enabled: true, Mode: "read"}})
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			var initial WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &initial))
			raw, err := json.Marshal(before.Declaration.Desired)
			testutil.Must(t, err)
			var desired config.DesiredWorkers
			testutil.Must(t, json.Unmarshal(raw, &desired))
			desired.Notification.Commands.Enabled = false
			switch damage {
			case "mode":
				desired.Notification.Commands.Mode = "mutation"
			case "backup":
				desired.Backup.Schedule = "weekly"
			case "source":
				for path := range initial.OwnedFiles {
					if path != workerRuntimeConfig {
						testutil.Must(t, os.WriteFile(filepath.Join(home, path), []byte("changed source pin"), 0600))
						break
					}
				}
			case "owner":
				state, e := openInstalledBackupState(home, false)
				testutil.Must(t, e)
				_, e = state.db.Exec("DELETE FROM meta WHERE key='notification_cutover_complete'")
				testutil.Must(t, e)
				state.Close()
			}
			request := VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: initial.Declaration.NotificationDesiredHash, BeforeGateway: notify.Hash("gateway"), Desired: desired}
			calls := 0
			_, err = ReconcileCommandNotificationScope(context.Background(), home, request, func(ProducerReservation) (string, error) { calls++; return notify.Hash("after"), nil })
			if damage != "receipt" {
				if err == nil || calls != 0 {
					t.Fatal("changed authority reached gateway", damage, err, calls)
				}
				return
			}
			testutil.Must(t, err)
			var current WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &current))
			testutil.Must(t, os.Remove(filepath.Join(home, ".local/state/deploy/backup-worker/notification-scope-"+request.Operation+".json")))
			if _, err = commandScopeOrigin(root, current, initial.Declaration.NotificationDesiredHash); err == nil {
				t.Fatal("lost scope receipt accepted")
			}
			if _, err = ReconcileCommandNotificationScope(context.Background(), home, request, func(ProducerReservation) (string, error) { calls++; return notify.Hash("after"), nil }); err == nil || calls != 1 {
				t.Fatal("missing history replayed gateway", err, calls)
			}
		})
	}
}

func TestCommandPermissionScopePreservesReportingHistory(t *testing.T) {
	n := &config.NotificationDeclaration{Commands: config.CommandDeclaration{Mode: "read", Permissions: []config.PermissionDeclaration{{Actor: "actor", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}}}
	home, before, _, _ := sharedSourceExpansionFixtureWithNotification(t, n)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var original WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &original))
	raw, err := json.Marshal(before.Declaration.Desired)
	testutil.Must(t, err)
	var desired config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(raw, &desired))
	for _, permissions := range [][]config.PermissionDeclaration{
		{{Actor: "actor", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{Subjects: []string{"database"}}}},
		{},
	} {
		var prior WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &prior))
		desired.Notification.Commands.Permissions = permissions
		r := VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: prior.Declaration.NotificationDesiredHash, BeforeGateway: notify.Hash(prior), Desired: desired}
		gateway := notify.Hash(r)
		calls := 0
		for range 2 {
			got, err := ReconcileCommandPolicyScope(context.Background(), home, r, func(p ProducerReservation) (string, error) {
				calls++
				if p != *original.Producer {
					t.Fatal("permission change replaced producer")
				}
				return gateway, nil
			})
			testutil.Must(t, err)
			if got != gateway {
				t.Fatal("permission receipt changed")
			}
		}
		if calls != 1 {
			t.Fatal("permission update repeated gateway effect")
		}
		var current WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &current))
		origin, err := commandScopeOrigin(root, current, original.Declaration.NotificationDesiredHash)
		testutil.Must(t, err)
		if origin.Declaration.DesiredHash != original.Declaration.DesiredHash {
			t.Fatal("permission update lost reporting origin")
		}
		pin, complete, err := InspectVerificationNotificationScope(home, r.Operation, r.BeforeGateway, current.Declaration.NotificationDesiredHash)
		testutil.Must(t, err)
		if !complete || pin != gateway {
			t.Fatal("missing permission scope completion")
		}
	}
}

func TestCommandEnrollmentScopePreservesReportingHistory(t *testing.T) {
	n := &config.NotificationDeclaration{Commands: config.CommandDeclaration{Mode: "read", Workers: map[string]string{"backup": "backup-worker"}, Conversations: map[string]config.ConversationDeclaration{"ops": {Provider: "telegram", Account: "123", NativeID: "456"}, "retired": {Provider: "telegram", Account: "123", NativeID: "457"}}, Permissions: []config.PermissionDeclaration{{Actor: "actor", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}}}
	home, before, _, _ := sharedSourceExpansionFixtureWithNotification(t, n)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var original WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &original))
	raw, err := json.Marshal(before.Declaration.Desired)
	testutil.Must(t, err)
	var desired config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(raw, &desired))
	for _, permissions := range [][]config.PermissionDeclaration{
		{{Actor: "actor", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{Subjects: []string{"database"}}}},
		{},
	} {
		var prior WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &prior))
		desired.Notification.Commands.Permissions = permissions
		desired.Notification.Commands.Workers = map[string]string{"archive": "backup-worker"}
		delete(desired.Notification.Commands.Conversations, "retired")
		for index := range desired.Notification.Commands.Permissions {
			desired.Notification.Commands.Permissions[index].Worker = "archive"
		}
		r := VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: prior.Declaration.NotificationDesiredHash, BeforeGateway: notify.Hash(prior), Desired: desired}
		gateway := notify.Hash(r)
		calls := 0
		for range 2 {
			got, err := ReconcileCommandEnrollmentScope(context.Background(), home, r, func(p ProducerReservation) (string, error) {
				calls++
				if p != *original.Producer {
					t.Fatal("permission change replaced producer")
				}
				return gateway, nil
			})
			testutil.Must(t, err)
			if got != gateway {
				t.Fatal("permission receipt changed")
			}
		}
		if calls != 1 {
			t.Fatal("permission update repeated gateway effect")
		}
		var current WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &current))
		origin, err := commandScopeOrigin(root, current, original.Declaration.NotificationDesiredHash)
		testutil.Must(t, err)
		if origin.Declaration.DesiredHash != original.Declaration.DesiredHash {
			t.Fatal("permission update lost reporting origin")
		}
		pin, complete, err := InspectVerificationNotificationScope(home, r.Operation, r.BeforeGateway, current.Declaration.NotificationDesiredHash)
		testutil.Must(t, err)
		if !complete || pin != gateway {
			t.Fatal("missing permission scope completion")
		}
	}
}

func TestDeliveryLEDOutputScopeKeepsReportingAndResumesLostReply(t *testing.T) {
	n := &config.NotificationDeclaration{StatusLEDs: config.LEDDeclaration{Enabled: true, Socket: "/private/led.sock", Daemon: config.LEDDaemon{Mode: "existing"}}}
	home, before, _, _ := sharedSourceExpansionFixtureWithNotification(t, n)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var initial WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &initial))
	cutoverPath := filepath.Join(home, ".local/state/deploy/backup-worker/state", backupCutoverFile)
	cutover, err := os.ReadFile(cutoverPath)
	testutil.Must(t, err)
	raw, err := json.Marshal(before.Declaration.Desired)
	testutil.Must(t, err)
	var desired config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(raw, &desired))
	stateDir, err := root.Sub(".local/state/deploy/backup-worker/state")
	testutil.Must(t, err)
	lease, err := stateDir.Lock("schema.lock", true, true)
	testutil.Must(t, err)
	lease.Close()
	stateDir.Close()
	ctx := context.Background()
	var retainedScopes [][4]string
	for _, enabled := range []bool{false, true, false} {
		var prior WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &prior))
		desired.Notification.StatusLEDs.Enabled = enabled
		request := VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: prior.Declaration.NotificationDesiredHash, BeforeGateway: notify.Hash(prior), Desired: desired}
		gateway := notify.Hash(request)
		lost := errors.New("gateway committed but reply lost")
		calls := 0
		callback := func(p ProducerReservation) (string, error) {
			if p != *initial.Producer {
				t.Fatal("command activity replaced producer")
			}
			calls++
			if calls == 1 {
				return "", lost
			}
			return gateway, nil
		}
		if _, err = ReconcileDeliveryLEDOutputScope(ctx, home, request, callback); !errors.Is(err, lost) {
			t.Fatal("lost command response not retained", err)
		}
		for range 2 {
			receipt, err := ReconcileDeliveryLEDOutputScope(ctx, home, request, callback)
			testutil.Must(t, err)
			if receipt != gateway {
				t.Fatal("command scope receipt changed")
			}
		}
		if calls != 2 {
			t.Fatal("completed command scope replayed gateway", calls)
		}
		scope, err := desired.NotificationFingerprint()
		testutil.Must(t, err)
		retainedScopes = append(retainedScopes, [4]string{request.Operation, request.BeforeGateway, scope, gateway})
		for _, recorded := range retainedScopes {
			receipt, done, err := InspectRetainedDeliveryOutputScope(home, recorded[0], recorded[1], recorded[2])
			testutil.Must(t, err)
			if !done || receipt != recorded[3] {
				t.Fatal("lost retained output receipt")
			}
			if _, _, err = InspectRetainedDeliveryOutputScope(home, recorded[0], notify.Hash("changed predecessor"), recorded[2]); err == nil {
				t.Fatal("changed output predecessor accepted")
			}
			if recorded[0] != request.Operation {
				if _, _, err = InspectVerificationNotificationScope(home, recorded[0], recorded[1], recorded[2]); err == nil {
					t.Fatal("ordinary scope inspection accepted historical output")
				}
			}
		}
		if len(retainedScopes) > 1 {
			old := retainedScopes[0]
			name := filepath.Join(home, ".local/state/deploy/backup-worker/notification-scope-"+old[0]+".json")
			raw, err := os.ReadFile(name)
			testutil.Must(t, err)
			testutil.Must(t, os.Remove(name))
			if _, _, err = InspectRetainedDeliveryOutputScope(home, old[0], old[1], old[2]); err == nil {
				t.Fatal("missing history accepted")
			}
			testutil.Must(t, os.WriteFile(name, raw, 0600))
		}
		var current WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &current))
		origin, err := commandScopeOrigin(root, current, initial.Declaration.NotificationDesiredHash)
		testutil.Must(t, err)
		if origin.Declaration.DesiredHash != initial.Declaration.DesiredHash {
			t.Fatal("command scope lost original reporting declaration")
		}
		state, err := openInstalledBackupState(home, false)
		testutil.Must(t, err)
		testutil.Must(t, requireCommandScopeOwner(state, current))
		if requireLegacyNotificationOwner(state) == nil {
			t.Fatal("command toggle revived direct sending")
		}
		owner, err := currentSharedBackupOwner(state)
		testutil.Must(t, err)
		state.Close()
		var retained backupCutoverIntent
		testutil.Must(t, json.Unmarshal(cutover, &retained))
		manifest := install.ManagedManifest{Root: initial.Declaration.NotificationRoot, Mode: "delivery", Identity: owner.Binding.Identity, Commands: &install.ManagedCommandAdmission{Mode: "drain", Hash: gateway}}
		if enabled {
			manifest.Commands.Mode = "read"
		}
		_, spec, err := prepareFreshSharedReportingUsing(ctx, home, current.Declaration.DesiredHash, notify.Hash(manifest), func(_ context.Context, root, now, original string) (install.ManagedManifest, error) {
			if root != manifest.Root || now != notify.Hash(manifest) || original != retained.Spec.Managed.ManifestHash {
				t.Fatal("reporting continuation lost exact pins")
			}
			return manifest, nil
		})
		testutil.Must(t, err)
		if spec.Version != 5 || notify.Hash(spec) != notify.Hash(retained.Spec) {
			t.Fatal("command toggle replaced reporting admission")
		}
		testutil.Must(t, requireFreshBackupReportingSetup(root, home, current, spec))
		testutil.Must(t, validateSetupDesired(current.Declaration))
		after, err := os.ReadFile(cutoverPath)
		testutil.Must(t, err)
		if string(after) != string(cutover) {
			t.Fatal("command activity rewrote reporting cutover")
		}
		pin, complete, err := InspectVerificationNotificationScope(home, request.Operation, request.BeforeGateway, current.Declaration.NotificationDesiredHash)
		testutil.Must(t, err)
		if !complete || pin != gateway {
			t.Fatal("command scope inspection lost completion")
		}
	}
}
