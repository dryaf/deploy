package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDisabledCommandsSurviveDeliveryQualificationRenewal(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, runtime, active, report := managedCommandsFixture(t)
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

func TestCommandRenewalRetainsAndCanFurtherReducePermissions(t *testing.T) {
	ctx := context.Background()
	spec, i, runtime, m, report := managedCommandsFixture(t)
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.commandPauseProbe, i.commandPolicyProbe = nop, nop
	var err error
	m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(report), "read", false)
	testutil.Must(t, err)
	m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
	testutil.Must(t, err)
	reduced := *runtime.Worker.Interaction
	reduced.Permissions = append([]interaction.Permission(nil), reduced.Permissions...)
	reduced.Permissions[0].Permissions = []string{"read"}
	raw, err := json.Marshal(reduced.Permissions)
	testutil.Must(t, err)
	m, err = i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(m), "", "", false, string(raw))
	testutil.Must(t, err)
	m, err = i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(m), "", false)
	testutil.Must(t, err)
	fresh := freshDeliveryQualification(t, spec.Root)
	m, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), protocol.Hash(fresh), false)
	testutil.Must(t, err)
	policy, err := InspectManagedCommandPolicy(ctx, spec.Root, protocol.Hash(m), runtime.Worker.Producers[0].Producer)
	testutil.Must(t, err)
	if !reflect.DeepEqual(policy.Permissions, reduced.Permissions) {
		t.Fatal("renewal lost permission reductions")
	}
	review, err := ReviewCommandNarrowing(ctx, spec.Root, protocol.Hash(m), "[]")
	testutil.Must(t, err)
	m, err = i.prepareCommands(ctx, review.Operation, spec.Root, review.ManifestHash, "", "", false, review.Policy)
	testutil.Must(t, err)
	m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(fresh), "read", false)
	testutil.Must(t, err)
	store, err := openManagedStore(m, runtime)
	testutil.Must(t, err)
	defer store.Close()
	testutil.Must(t, store.BindManagedCommandAuthority(func(context.Context) error { return nil }))
	destination := runtime.Worker.Interaction.Destinations[0]
	input := interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "denied", Actor: "789", Conversation: destination.ID, DestinationFingerprint: destination.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"}
	if _, err = store.AdmitCommand(ctx, input); err == nil {
		t.Fatal("renewal restored retired permissions")
	}
}

func TestCommandPauseRefusesIncapableExecutablesBeforeRetainingIntent(t *testing.T) {
	for _, failure := range []int{1, 2} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			ctx := context.Background()
			spec, i, _, m, report := managedCommandsFixture(t)
			var err error
			m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(report), "read", false)
			testutil.Must(t, err)
			m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
			testutil.Must(t, err)
			before, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
			testutil.Must(t, err)
			calls := 0
			i.commandPauseProbe = func(context.Context, *safefs.Directory, string, string) error {
				calls++
				if calls == failure {
					return errors.New("unsupported command pause")
				}
				return nil
			}
			if _, err = i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(m), "", false); err == nil {
				t.Fatal("accepted incapable executable")
			}
			after, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
			testutil.Must(t, err)
			if calls != failure || string(before) != string(after) {
				t.Fatal("unsupported pause retained an intent")
			}
		})
	}
}

func TestCommandQualificationRenewalRetainsLEDOutput(t *testing.T) {
	ctx := context.Background()
	spec, i, _, m, q, api := productionOutputFixture(t, true, false)
	i.commandPauseProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	var err error
	m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(q), "read", false)
	testutil.Must(t, err)
	m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
	testutil.Must(t, err)
	m, err = i.ensureLEDOutput(ctx, spec.Root, protocol.Hash(m), true)
	testutil.Must(t, err)
	original, foreign := m, api.states["foreign"]
	m, err = i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(m), "", false)
	testutil.Must(t, err)
	fresh := qualifyPausedLED(t, m)
	m, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), fresh.QualificationHash, false)
	testutil.Must(t, err)
	if !reflect.DeepEqual(m.LEDOutput, original.LEDOutput) || m.CommandHistoryHash != original.CommandHistoryHash || api.states["foreign"] != foreign {
		t.Fatal("command renewal changed LED or command ownership")
	}
	_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(m), protocol.Hash(original))
	testutil.Must(t, err)
	m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), fresh.QualificationHash, "read", false)
	testutil.Must(t, err)
	if !reflect.DeepEqual(m.LEDOutput, original.LEDOutput) || api.states["foreign"] != foreign {
		t.Fatal("command re-enable changed LED output")
	}
}
