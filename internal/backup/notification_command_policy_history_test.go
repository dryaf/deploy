//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestRetainedCommandPolicyScopeFollowsDeclarationAndCommandActivity(t *testing.T) {
	ctx := context.Background()
	home, before, _, _ := sharedSourceExpansionFixtureWithNotification(t, &config.NotificationDeclaration{Commands: config.CommandDeclaration{Enabled: true, Mode: "read"}})
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	state, err := root.Sub(".local/state/deploy/backup-worker/state")
	testutil.Must(t, err)
	lease, err := state.Lock("schema.lock", true, true)
	testutil.Must(t, err)
	lease.Close()
	state.Close()
	raw, err := json.Marshal(before.Declaration.Desired)
	testutil.Must(t, err)
	var desired config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(raw, &desired))
	gateway := notify.Hash("initial gateway")
	current := func() WorkerSetupJournal {
		var j WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
		return j
	}
	toggle := func(enabled bool) {
		prior := current()
		desired.Notification.Commands.Enabled = enabled
		r := VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: prior.Declaration.NotificationDesiredHash, BeforeGateway: gateway, Desired: desired}
		next := notify.Hash(r.Operation)
		_, err := ReconcileCommandNotificationScope(ctx, home, r, func(ProducerReservation) (string, error) { return next, nil })
		testutil.Must(t, err)
		gateway = next
	}
	toggle(false)
	prior := current()
	desired.Notification.Limits.MaxMessages = 10000
	declaration := VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: prior.Declaration.NotificationDesiredHash, BeforeGateway: gateway, Desired: desired}
	_, err = ReconcileDeliveryDeclarationScope(ctx, home, declaration, nil, func(ProducerReservation) (string, error) { return gateway, nil })
	testutil.Must(t, err)
	scope := current().Declaration.NotificationDesiredHash
	toggle(true)
	toggle(false)
	tree := policyReviewTree(t, home)
	if _, _, err = InspectVerificationNotificationScope(home, declaration.Operation, declaration.BeforeGateway, scope); err == nil {
		t.Fatal("latest-only inspector accepted an older receipt")
	}
	for range 2 {
		got, done, err := InspectRetainedCommandPolicyScope(home, declaration.Operation, declaration.BeforeGateway, scope)
		testutil.Must(t, err)
		if !done || got != declaration.BeforeGateway {
			t.Fatal("declaration/command ancestry lost its exact gateway")
		}
	}
	if !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("ancestry inspection modified retained records")
	}
	testutil.Must(t, os.Remove(filepath.Join(home, ".local/state/deploy/backup-worker/notification-scope-"+declaration.Operation+".json")))
	if _, _, err = InspectRetainedCommandPolicyScope(home, declaration.Operation, declaration.BeforeGateway, scope); err == nil {
		t.Fatal("lost history accepted")
	}
}
