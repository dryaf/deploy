package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeliveryPauseTypedRetryRetainsOwnerAndRequiresFreshQualification(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, services, runtime, before, record := deliveryFixture(t)
			active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
			testutil.Must(t, err)
			store, err := openManagedStore(active, runtime)
			testutil.Must(t, err)
			producer := runtime.Worker.Producers[0].Producer
			testutil.Must(t, store.Activate(producer, "one"))
			now := time.Now().UTC()
			message := protocol.Message{Producer: producer, Sequence: 1, ID: "pause-retained", Generation: "one", Subject: "subject", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "retained queued work", Route: "ops", ReportKey: "day", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
			queued, err := store.Enqueue(ctx, message)
			testutil.Must(t, err)
			testutil.Must(t, store.Close())
			request := DeliveryPauseRequest{spec.Root, protocol.Hash(active)}
			_, done, err := i.inspectDeliveryPause(ctx, request)
			testutil.Must(t, err)
			if done {
				t.Fatal("active installation reported paused")
			}
			stop := errors.New("lost pause reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.prepareDelivery(ctx, "pause-delivery", request.Root, request.ManifestHash, "", false); !errors.Is(err, stop) {
				t.Fatal("missed boundary", boundary, err)
			}
			_, done, err = i.inspectDeliveryPause(ctx, request)
			testutil.Must(t, err)
			if done && boundary != "complete" {
				t.Fatal("unfinished pause reported complete")
			}
			i.fault = nil
			paused, err := i.prepareDelivery(ctx, "pause-delivery", request.Root, request.ManifestHash, "", false)
			testutil.Must(t, err)
			starts, stops := services.starts, services.stops
			for range 2 {
				got, done, err := i.inspectDeliveryPause(ctx, request)
				testutil.Must(t, err)
				if !done || protocol.Hash(got) != protocol.Hash(paused) || services.starts != starts || services.stops != stops {
					t.Fatal("pause replayed service effects")
				}
			}
			if paused.DeliveryOwnerHash != active.DeliveryOwnerHash || paused.Identity != active.Identity || paused.ConfigHash != active.ConfigHash || paused.Mode != "verification" || paused.DeliveryActivation != nil {
				t.Fatal("pause lost production authority")
			}
			store, err = openManagedStore(paused, runtime)
			testutil.Must(t, err)
			state, err := store.OwnershipStatus(producer, "one")
			testutil.Must(t, err)
			retained, err := store.Get(ctx, producer, message.ID)
			testutil.Must(t, err)
			if !state.Active || state.State != "active" || protocol.Hash(retained) != protocol.Hash(queued) {
				t.Fatal("pause changed active generation or queue")
			}
			if !store.VerificationOnly() {
				t.Fatal("pause left provider delivery enabled")
			}
			testutil.Must(t, store.Close())
			changed := request
			changed.ManifestHash = protocol.Hash("other installation")
			if _, _, err = i.inspectDeliveryPause(ctx, changed); err == nil {
				t.Fatal("retargeted pause accepted")
			}
			// The old qualification remains evidence for its old verification manifest;
			// the renewed verification manifest retains delivery ownership and differs.
			_, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(paused))
			testutil.Must(t, err)
			if ready {
				t.Fatal("old qualification silently renewed admission")
			}
			fresh := freshDeliveryQualification(t, spec.Root)
			renewed, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(paused), protocol.Hash(fresh), false)
			testutil.Must(t, err)
			if renewed.DeliveryOwnerHash != active.DeliveryOwnerHash || renewed.Identity != active.Identity {
				t.Fatal("renewal replaced delivery owner")
			}
			_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(renewed), protocol.Hash(active))
			testutil.Must(t, err)
			store, err = openManagedStore(renewed, runtime)
			testutil.Must(t, err)
			state, err = store.OwnershipStatus(producer, "one")
			testutil.Must(t, err)
			retained, err = store.Get(ctx, producer, message.ID)
			testutil.Must(t, err)
			if !state.Active || protocol.Hash(retained) != protocol.Hash(queued) {
				t.Fatal("renewal lost producer or queued message")
			}
			testutil.Must(t, store.Close())
			if _, _, err = i.inspectDeliveryPause(ctx, request); err == nil {
				t.Fatal("old pause receipt accepted new active revision")
			}
			// Another pause can have the same verification manifest, but its
			// distinct immutable pause record must remain part of reporting ancestry.
			second, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(renewed), "", false)
			testutil.Must(t, err)
			secondReport := freshDeliveryQualification(t, spec.Root)
			latest, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(second), protocol.Hash(secondReport), false)
			testutil.Must(t, err)
			_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(latest), protocol.Hash(active))
			testutil.Must(t, err)
			d, err := privateManagedDir(spec.Root, true)
			testutil.Must(t, err)
			proof, verification, err := readDeliveryAdmission(d, renewed, runtime)
			testutil.Must(t, err)
			wrong := proof
			wrong.Version = 1
			if _, err = deliveryPausePredecessor(d, wrong, verification); err == nil {
				t.Fatal("pause proof downgraded")
			}
			d.Close()
			path := filepath.Join(spec.Root, "operations", proof.PauseOperation, "intent.json")
			body, err := os.ReadFile(path)
			testutil.Must(t, err)
			testutil.Must(t, os.Remove(path))
			if _, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(latest), protocol.Hash(active)); err == nil {
				t.Fatal("missing original pause history accepted")
			}
			testutil.Must(t, os.WriteFile(path, body, 0600))
		})
	}
}

func TestDeliveryPauseRequiresBothCapableExecutablesBeforeStopping(t *testing.T) {
	for _, failure := range []int{1, 2} {
		t.Run(string(rune('0'+failure)), func(t *testing.T) {
			spec, i, services, _, before, record := deliveryFixture(t)
			active, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
			testutil.Must(t, err)
			original, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
			testutil.Must(t, err)
			starts, stops, calls := services.starts, services.stops, 0
			i.deliveryPauseProbe = func(context.Context, *safefs.Directory, string, string) error {
				calls++
				if calls == failure {
					return errors.New("old helper")
				}
				return nil
			}
			if _, err = i.prepareDelivery(context.Background(), "pause-delivery", spec.Root, protocol.Hash(active), "", false); err == nil {
				t.Fatal("incapable helper accepted")
			}
			actual, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
			testutil.Must(t, err)
			if string(actual) != string(original) || services.starts != starts || services.stops != stops || calls != failure {
				t.Fatal("rejected pause changed service or authority")
			}
		})
	}
}

func TestGuidedQualificationStartsFreshForEachDeliveryPause(t *testing.T) {
	ctx := context.Background()
	spec, installer, _, _, before, record := deliveryFixture(t)
	previousHash := protocol.Hash(record)
	active, err := installer.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), previousHash, false)
	testutil.Must(t, err)
	var previousSession string
	for range 2 {
		paused, err := installer.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(active), "", false)
		testutil.Must(t, err)
		if _, err = installer.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(paused), previousHash, true); err == nil {
			t.Fatal("prior cycle admitted without fresh observations")
		}
		_, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(paused))
		testutil.Must(t, err)
		if ready {
			t.Fatal("review selected the previous pause's report")
		}
		if previousSession != "" {
			if _, err = CommitQualification(ctx, spec.Root, protocol.Hash(paused), previousSession); err == nil {
				t.Fatal("prior cycle draft accepted in a new pause")
			}
		}
		session, err := BeginQualification(ctx, spec.Root, protocol.Hash(paused))
		testutil.Must(t, err)
		if session.Recorded || session.EvidenceComplete || session.ID == previousSession {
			t.Fatal("new pause reused previous observations")
		}
		resumed, err := BeginQualification(ctx, spec.Root, protocol.Hash(paused))
		testutil.Must(t, err)
		if resumed.ID != session.ID {
			t.Fatal("same pause failed to resume its draft")
		}
		for _, check := range session.Checks {
			if check.Result != "pending" {
				t.Fatal("old observation copied into fresh cycle")
			}
			_, err = ObserveQualification(ctx, spec.Root, protocol.Hash(paused), QualificationObservation{Session: session.ID, Scope: check.Scope, ID: check.ID, Result: "passed", EvidenceHash: protocol.Hash("synthetic offline evidence")})
			testutil.Must(t, err)
		}
		_, err = CommitQualification(ctx, spec.Root, protocol.Hash(paused), session.ID)
		testutil.Must(t, err)
		resumed, err = BeginQualification(ctx, spec.Root, protocol.Hash(paused))
		testutil.Must(t, err)
		if resumed.ID != session.ID || !resumed.Recorded || !resumed.EvidenceComplete {
			t.Fatal("completed current cycle was not idempotent")
		}
		review, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(paused))
		testutil.Must(t, err)
		if !ready || review.QualificationHash == previousHash {
			t.Fatal("fresh observations not selected")
		}
		active, err = installer.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(paused), review.QualificationHash, false)
		testutil.Must(t, err)
		previousHash, previousSession = review.QualificationHash, session.ID
	}
}
