//go:build linux

package backup

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDeliveryDeclarationScopeRetainsReportingWithoutServiceEffects(t *testing.T) {
	for _, point := range []string{"scope-intent", "scope-receipt", "scope-complete"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			home, r, before, _, services, profile := providerScopeFixture(t)
			r.Desired.Notification.Providers = before.Declaration.Desired.Notification.Providers
			r.Desired.Notification.Limits.MaxMessages = 10000
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			cutoverPath := filepath.Join(home, ".local/state/deploy/backup-worker/state", backupCutoverFile)
			original, err := os.ReadFile(cutoverPath)
			testutil.Must(t, err)
			change := func(p ProducerReservation) (string, error) {
				if p != *before.Producer || services.stopped || !services.active {
					t.Fatal("declaration disrupted reporting")
				}
				return profile.ManifestHash, nil
			}
			lost := errors.New("lost declaration reply")
			_, err = reconcileNotificationScope(ctx, home, r, change, 22, func(at string) error {
				if at == point {
					return lost
				}
				return nil
			})
			if !errors.Is(err, lost) {
				t.Fatal("missed boundary", err)
			}
			for range 2 {
				receipt, e := ReconcileDeliveryDeclarationScope(ctx, home, r, nil, change)
				testutil.Must(t, e)
				if receipt != r.BeforeGateway {
					t.Fatal("declaration changed receipt")
				}
			}
			var after WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
			if services.stopped || !services.active || !reflect.DeepEqual(before.Shared, after.Shared) || !reflect.DeepEqual(before.Producer, after.Producer) || !reflect.DeepEqual(before.SourceKeys, after.SourceKeys) {
				t.Fatal("declaration changed reporting or source authority")
			}
			got, err := os.ReadFile(cutoverPath)
			testutil.Must(t, err)
			if string(got) != string(original) {
				t.Fatal("declaration replaced original handoff")
			}
			_, err = commandScopeOrigin(root, after, r.BeforeScope)
			testutil.Must(t, err)
			scope, err := r.Desired.NotificationFingerprint()
			testutil.Must(t, err)
			receipt, done, err := InspectRetainedDeliveryPolicyScope(home, r.Operation, r.BeforeGateway, scope)
			testutil.Must(t, err)
			if !done || receipt != r.BeforeGateway {
				t.Fatal("lost declaration proof")
			}
		})
	}
}
