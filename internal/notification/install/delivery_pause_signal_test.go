package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestOwnedSignalDeliveryPauseAndRenewalKeepsNativeAccount(t *testing.T) {
	for _, combined := range []bool{false, true} {
		t.Run(map[bool]string{false: "signal", true: "combined"}[combined], func(t *testing.T) {
			ctx := context.Background()
			spec, i, runtime, native := freshSignalGatewayFixture(t, false, combined)
			before, err := i.ensureVerification(ctx, spec)
			testutil.Must(t, err)
			qualify := func(m ManagedManifest) string {
				session, err := BeginQualification(ctx, spec.Root, protocol.Hash(m))
				testutil.Must(t, err)
				for _, check := range session.Checks {
					_, err = ObserveQualification(ctx, spec.Root, protocol.Hash(m), QualificationObservation{Session: session.ID, Scope: check.Scope, ID: check.ID, Result: "passed", EvidenceHash: protocol.Hash("synthetic offline observation")})
					testutil.Must(t, err)
				}
				_, err = CommitQualification(ctx, spec.Root, protocol.Hash(m), session.ID)
				testutil.Must(t, err)
				review, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(m))
				testutil.Must(t, err)
				if !ready {
					t.Fatal("complete synthetic observations did not qualify fixture")
				}
				return review.QualificationHash
			}
			record := qualify(before)
			active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), record, false)
			testutil.Must(t, err)
			store, err := openManagedStore(active, runtime)
			testutil.Must(t, err)
			producer := runtime.Worker.Producers[0].Producer
			testutil.Must(t, store.Activate(producer, "one"))
			testutil.Must(t, store.Close())
			retained := map[string][]byte{}
			for _, root := range []string{native.Spec.Root, native.Spec.AccountState, native.Spec.Inbox.State} {
				testutil.Must(t, filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !e.Type().IsRegular() {
						return nil
					}
					retained[path], err = os.ReadFile(path)
					return err
				}))
			}
			current := active
			for range 2 {
				paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(current), "", false)
				testutil.Must(t, err)
				if paused.FreshSignal == nil || *paused.FreshSignal != *active.FreshSignal {
					t.Fatal("pause changed native ownership")
				}
				record = qualify(paused)
				current, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(paused), record, false)
				testutil.Must(t, err)
				_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash(active))
				testutil.Must(t, err)
				store, err = openManagedStore(current, runtime)
				testutil.Must(t, err)
				status, err := store.OwnershipStatus(producer, "one")
				testutil.Must(t, err)
				testutil.Must(t, store.Close())
				if !status.Active {
					t.Fatal("renewal replaced producer generation")
				}
			}
			for path, body := range retained {
				actual, err := os.ReadFile(path)
				testutil.Must(t, err)
				if string(actual) != string(body) {
					t.Fatal("renewal changed native history", path)
				}
			}
			got, err := InspectFreshSignal(ctx, *current.FreshSignal, false)
			testutil.Must(t, err)
			if protocol.Hash(got) != protocol.Hash(native) {
				t.Fatal("native receipt changed")
			}
		})
	}
}
