package backup

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestVerificationCommandActivityScopeRetainsReservation(t *testing.T) {
	home, request, original := verificationScopeFixture(t, true)
	request.Desired.Notification.StatusLEDs = original.Declaration.Desired.Notification.StatusLEDs
	p := request.Desired.Notification.Providers["telegram"]
	p.Account, p.BotName = "123", "fixture_bot"
	request.Desired.Notification.Providers = config.ProviderDeclarations{"telegram": p}
	request.Desired.Notification.Commands = config.CommandDeclaration{Enabled: true, Mode: "read", Workers: map[string]string{"backup": "backup-worker"}, Conversations: map[string]config.ConversationDeclaration{"ops": {Provider: "telegram", Account: "123", NativeID: "456"}}, Permissions: []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}}
	ctx := context.Background()
	receipt, err := ReconcileVerificationCommandsScope(ctx, home, request, func(ProducerReservation) (string, error) { return notify.Hash("commands"), nil })
	testutil.Must(t, err)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	for _, enabled := range []bool{false, true} {
		var before WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &before))
		request.Operation = testutil.ID(t, "n")
		request.BeforeScope = before.Declaration.NotificationDesiredHash
		request.BeforeGateway = receipt
		request.Desired.Notification.Commands.Enabled = enabled
		calls := 0
		stop := errors.New("lost gateway reply")
		callback := func(p ProducerReservation) (string, error) {
			calls++
			if p != *original.Producer {
				t.Fatal("producer changed")
			}
			if calls == 1 {
				return "", stop
			}
			return notify.Hash(request.Operation), nil
		}
		_, err = ReconcileVerificationCommandActivityScope(ctx, home, request, callback)
		if !errors.Is(err, stop) {
			t.Fatal(enabled, err)
		}
		if _, _, err = InspectReservedProducer(home, request.BeforeScope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
			t.Fatal("pending scope exposed reservation")
		}
		receipt, err = ReconcileVerificationCommandActivityScope(ctx, home, request, callback)
		testutil.Must(t, err)
		var after WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
		if after.NotificationScope != nil || after.WorkerID != original.WorkerID || after.ArchiveID != original.ArchiveID || !reflect.DeepEqual(after.Producer, original.Producer) || !reflect.DeepEqual(after.OwnedFiles, original.OwnedFiles) || !reflect.DeepEqual(after.SourceKeys, original.SourceKeys) {
			t.Fatal("activity changed installed ownership")
		}
		tree := policyReviewTree(t, home)
		again, err := ReconcileVerificationCommandActivityScope(ctx, home, request, callback)
		if err != nil || again != receipt || calls != 2 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
			t.Fatal("retry changed completed activity", err, calls)
		}
	}
}
