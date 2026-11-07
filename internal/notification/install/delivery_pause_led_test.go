package install

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeliveryPauseAfterLEDOutputRetainsHistoryAndForeignOutput(t *testing.T) {
	ctx := context.Background()
	spec, i, c, original, _, api := productionOutputFixture(t, false, false)
	current := original
	foreign := api.states["foreign"]
	for _, disabled := range []bool{true, false, true} {
		toggled, err := i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(current), disabled)
		testutil.Must(t, err)
		paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(toggled), "", false)
		testutil.Must(t, err)
		if paused.LEDOutput == nil || *paused.LEDOutput != *toggled.LEDOutput {
			t.Fatal("pause changed output authority")
		}
		review := qualifyPausedLED(t, paused)
		current, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(paused), review.QualificationHash, false)
		testutil.Must(t, err)
		if *current.LEDOutput != *toggled.LEDOutput || current.DeliveryOwnerHash != original.DeliveryOwnerHash || current.ConfigHash != original.ConfigHash || api.states["foreign"] != foreign {
			t.Fatal("renewal changed output, runtime or owner")
		}
		_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash(original))
		testutil.Must(t, err)
		_, err = InspectCapturedDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash(toggled), protocol.Hash(original))
		testutil.Must(t, err)
		if _, err = InspectCapturedDeliveryContinuation(ctx, spec.Root, protocol.Hash(current), protocol.Hash(original), protocol.Hash(toggled)); err == nil {
			t.Fatal("report snapshot order reversed")
		}
		store, err := openManagedStore(current, c)
		testutil.Must(t, err)
		testutil.Must(t, requireManagedLEDOutput(ctx, store, current))
		status, err := store.OwnershipStatus(c.Worker.Producers[0].Producer, "one")
		testutil.Must(t, err)
		testutil.Must(t, store.Close())
		if !status.Active {
			t.Fatal("renewal lost producer")
		}
	}
}

func qualifyPausedLED(t *testing.T, paused ManagedManifest) DeliveryAdmissionRequest {
	t.Helper()
	ctx := context.Background()
	session, err := BeginQualification(ctx, paused.Root, protocol.Hash(paused))
	testutil.Must(t, err)
	if !session.LEDEnvironmentRequired {
		t.Fatal("renewal omitted LED observations")
	}
	_, err = ObserveQualification(ctx, paused.Root, protocol.Hash(paused), QualificationObservation{Session: session.ID, LEDVersion: "synthetic-v1", LEDConfigHash: protocol.Hash("offline daemon configuration")})
	testutil.Must(t, err)
	for _, check := range session.Checks {
		_, err = ObserveQualification(ctx, paused.Root, protocol.Hash(paused), QualificationObservation{Session: session.ID, Scope: check.Scope, ID: check.ID, Result: "passed", EvidenceHash: protocol.Hash("offline observation")})
		testutil.Must(t, err)
	}
	_, err = CommitQualification(ctx, paused.Root, protocol.Hash(paused), session.ID)
	testutil.Must(t, err)
	review, ready, err := ReviewDeliveryAdmission(ctx, paused.Root, protocol.Hash(paused))
	testutil.Must(t, err)
	if !ready {
		t.Fatal("fixture observations did not qualify")
	}
	return review
}

func TestDeliveryRenewalCannotInventOriginallyDisabledLEDPermission(t *testing.T) {
	ctx := context.Background()
	spec, i, _, current, _, _ := productionOutputFixture(t, false, true)
	for range 2 {
		paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(current), "", false)
		testutil.Must(t, err)
		review := qualifyPausedLED(t, paused)
		current, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(paused), review.QualificationHash, false)
		testutil.Must(t, err)
		if _, err = ReviewDeliveryLEDOutput(ctx, spec.Root, protocol.Hash(current), false); err == nil {
			t.Fatal("renewal invented original output permission")
		}
	}
}
