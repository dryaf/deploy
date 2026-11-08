package install

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"reflect"
	"testing"
	"time"
)

func TestSharedActivityServiceResumesWithoutChangingQueuedAuthority(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "disable-returned", "unit-published", "manifest-published", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, services, runtime, before, record := deliveryFixture(t)
			i.sharedActivityProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
			testutil.Must(t, err)
			store, err := openManagedStore(active, runtime)
			testutil.Must(t, err)
			producer := runtime.Worker.Producers[0].Producer
			testutil.Must(t, store.Activate(producer, "one"))
			now := time.Now().UTC()
			message := protocol.Message{Producer: producer, Sequence: 1, ID: "activity-retained", Generation: "one", Subject: "subject", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "retained shared activity work", Route: "ops", ReportKey: "day", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
			queued, err := store.Enqueue(ctx, message)
			testutil.Must(t, err)
			testutil.Must(t, store.Close())
			paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(active), "", false)
			testutil.Must(t, err)
			current := paused
			lost := errors.New("lost service activity reply")
			for _, disabled := range []bool{true, false} {
				faulted := false
				i.fault = func(at string) error {
					if at == boundary {
						faulted = true
						return lost
					}
					return nil
				}
				_, err = i.setSharedActivityService(ctx, spec.Root, protocol.Hash(current), disabled)
				// Start has no return boundary during removal; disable has none on restore.
				applicable := disabled && boundary != "start-returned" || !disabled && boundary != "disable-returned"
				if applicable && (!faulted || !errors.Is(err, lost)) {
					t.Fatal("missed activity boundary", disabled, boundary, err)
				}
				if !applicable {
					testutil.Must(t, err)
				}
				i.fault = nil
				next, err := i.setSharedActivityService(ctx, spec.Root, protocol.Hash(current), disabled)
				testutil.Must(t, err)
				starts, stops := services.starts, services.stops
				again, err := i.setSharedActivityService(ctx, spec.Root, protocol.Hash(current), disabled)
				testutil.Must(t, err)
				if protocol.Hash(again) != protocol.Hash(next) || starts != services.starts || stops != services.stops {
					t.Fatal("completed activity repeated effects")
				}
				current = next
			}
			if current.Mode != "verification" || current.Identity != active.Identity || current.DeliveryOwnerHash != active.DeliveryOwnerHash {
				t.Fatal("restoration changed owner")
			}
			store, err = openManagedStore(current, runtime)
			testutil.Must(t, err)
			retained, err := store.Get(ctx, producer, message.ID)
			testutil.Must(t, err)
			state, err := store.OwnershipStatus(producer, "one")
			testutil.Must(t, err)
			if !state.Active || protocol.Hash(retained) != protocol.Hash(queued) || !store.VerificationOnly() {
				t.Fatal("activity changed generation/queue or enabled dispatch")
			}
			testutil.Must(t, store.Close())
			_, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(current))
			testutil.Must(t, err)
			if ready {
				t.Fatal("old qualification restored delivery")
			}
			fresh := freshDeliveryQualification(t, spec.Root)
			renewed, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(current), protocol.Hash(fresh), false)
			testutil.Must(t, err)
			_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(renewed), protocol.Hash(active))
			testutil.Must(t, err)
		})
	}
}

func TestSharedActivityPreservesLEDPermissionAndForeignClients(t *testing.T) {
	for _, originallyDisabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "disabled"}[originallyDisabled], func(t *testing.T) {
			ctx := context.Background()
			spec, i, _, current, _, api := productionOutputFixture(t, false, originallyDisabled)
			i.sharedActivityProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			foreign := api.states["foreign"]
			if !originallyDisabled {
				var err error
				current, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(current), true)
				testutil.Must(t, err)
			}
			paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(current), "", false)
			testutil.Must(t, err)
			suspended, err := i.setSharedActivityService(ctx, spec.Root, protocol.Hash(paused), true)
			testutil.Must(t, err)
			// Report isolation still includes all credentials while the gateway is off.
			c, err := ReadRuntime(suspended.Paths.Config)
			testutil.Must(t, err)
			_, err = InspectManagedReportProfile(spec.Root, suspended.Identity, c.Worker.Producers[0].Producer, c.Worker.Producers[0].Routes[0])
			testutil.Must(t, err)
			restored, err := i.setSharedActivityService(ctx, spec.Root, protocol.Hash(suspended), false)
			testutil.Must(t, err)
			request := qualifyPausedLED(t, restored)
			renewed, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(restored), request.QualificationHash, false)
			testutil.Must(t, err)
			if originallyDisabled {
				if _, err = ReviewDeliveryLEDOutput(ctx, spec.Root, protocol.Hash(renewed), false); err == nil {
					t.Fatal("activity inferred new output permission")
				}
			} else {
				_, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(renewed), false)
				testutil.Must(t, err)
			}
			if api.states["foreign"] != foreign {
				t.Fatal("activity touched a foreign LED client")
			}
		})
	}
}

func TestSharedActivityRetainsQueuedCommandsAndDisabledIntake(t *testing.T) {
	for _, boundary := range []string{"prepared"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, runtime, active, report := managedCommandsFixture(t)
			i.sharedActivityProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.commandPauseProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(report), "read", false)
			testutil.Must(t, err)
			if _, err = i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(enabled), "", false); err == nil {
				t.Fatal("paused active command intake")
			}
			store, err := openManagedStore(enabled, runtime)
			testutil.Must(t, err)
			testutil.Must(t, store.BindManagedCommandAuthority(func(context.Context) error { return nil }))
			destination := runtime.Worker.Interaction.Destinations[0]
			input := interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "pending", Actor: "789", Conversation: destination.ID, DestinationFingerprint: destination.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"}
			_, err = store.AdmitCommand(ctx, input)
			testutil.Must(t, err)
			testutil.Must(t, store.Close())
			drained, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
			testutil.Must(t, err)
			store, err = openManagedStore(drained, runtime)
			testutil.Must(t, err)
			producer := runtime.Worker.Producers[0].Producer
			testutil.Must(t, store.Activate(producer, "one"))
			pending, err := store.Commands(ctx, producer)
			testutil.Must(t, err)
			if len(pending) != 1 {
				t.Fatal("missing queued command", pending)
			}
			testutil.Must(t, store.Close())
			stopped := errors.New("lost pause reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stopped
				}
				return nil
			}
			_, err = i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(drained), "", false)
			if !errors.Is(err, stopped) {
				t.Fatal("missed pause boundary", err)
			}
			i.fault = nil
			paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(drained), "", false)
			testutil.Must(t, err)
			if paused.CommandHistoryHash != drained.CommandHistoryHash || paused.Commands != nil || paused.Identity != drained.Identity || paused.DeliveryOwnerHash != drained.DeliveryOwnerHash {
				t.Fatal("pause changed retained authority")
			}
			_, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(paused))
			testutil.Must(t, err)
			if ready {
				t.Fatal("old qualification renewed delivery")
			}
			suspended, e := i.setSharedActivityService(ctx, spec.Root, protocol.Hash(paused), true)
			testutil.Must(t, e)
			paused, e = i.setSharedActivityService(ctx, spec.Root, protocol.Hash(suspended), false)
			testutil.Must(t, e)
			fresh := freshDeliveryQualification(t, spec.Root)
			renewed, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(paused), protocol.Hash(fresh), false)
			testutil.Must(t, err)
			if renewed.Commands != nil || renewed.CommandHistoryHash != drained.CommandHistoryHash {
				t.Fatal("renewal admitted commands or lost history")
			}
			store, err = openManagedStore(renewed, runtime)
			testutil.Must(t, err)
			retained, err := store.Commands(ctx, producer)
			testutil.Must(t, err)
			if !reflect.DeepEqual(pending, retained) {
				t.Fatal("renewal changed pending requests")
			}
			input.NativeID = "after-renewal"
			if _, err = store.AdmitCommand(ctx, input); err == nil {
				t.Fatal("renewal admitted intake")
			}
			testutil.Must(t, store.Close())
			_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(renewed), protocol.Hash(drained))
			testutil.Must(t, err)
			reenabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(renewed), protocol.Hash(fresh), "read", false)
			testutil.Must(t, err)
			store, err = openManagedStore(reenabled, runtime)
			testutil.Must(t, err)
			testutil.Must(t, store.BindManagedCommandAuthority(func(context.Context) error { return nil }))
			_, err = store.AdmitCommand(ctx, input)
			testutil.Must(t, err)
			testutil.Must(t, store.Close())
		})
	}
}

func TestSharedActivityVerificationRestorationCannotReuseQualification(t *testing.T) {
	ctx := context.Background()
	spec, i, _, _, before, record := deliveryFixture(t)
	i.sharedActivityProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	current := before
	for _, removeService := range []bool{false, true, false} {
		var err error
		if removeService {
			current, err = i.setSharedActivityService(ctx, spec.Root, protocol.Hash(current), true)
			testutil.Must(t, err)
		}
		restored, err := i.setSharedActivityService(ctx, spec.Root, protocol.Hash(current), false)
		testutil.Must(t, err)
		if restored.ActivityRestoration == nil || protocol.Hash(restored) == protocol.Hash(current) {
			t.Fatal("verification restoration has no new observation cycle")
		}
		_, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(restored))
		testutil.Must(t, err)
		if ready {
			t.Fatal("activity reused pre-disable verification qualification")
		}
		if _, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(restored), protocol.Hash(record), false); err == nil {
			t.Fatal("old observation record admitted restored gateway")
		}
		record = freshDeliveryQualification(t, spec.Root)
		current = restored
	}
}

func TestSharedActivityRejectsExplicitRemovalAndIncapableExecutables(t *testing.T) {
	ctx := context.Background()
	spec, i, services, _, before, _ := deliveryFixture(t)
	original, err := i.inspectInstallation(ctx, spec.Root)
	testutil.Must(t, err)
	stops := services.stops
	i.sharedActivityProbe = func(context.Context, *safefs.Directory, string, string) error {
		return errors.New("missing shared activity capability")
	}
	if _, err = i.setSharedActivityService(ctx, spec.Root, protocol.Hash(before), true); err == nil {
		t.Fatal("incapable executable retained service intent")
	}
	observed, err := i.inspectInstallation(ctx, spec.Root)
	testutil.Must(t, err)
	if protocol.Hash(observed) != protocol.Hash(original) || services.stops != stops {
		t.Fatal("capability refusal changed installation")
	}
	removed, err := i.uninstallExpected(ctx, spec.Root, protocol.Hash(before))
	testutil.Must(t, err)
	i.sharedActivityProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	if _, err = i.setSharedActivityService(ctx, spec.Root, protocol.Hash(removed), false); err == nil {
		t.Fatal("explicit uninstall was adopted as shared disable")
	}
}
