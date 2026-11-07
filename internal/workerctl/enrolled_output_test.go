package workerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestEnrolledLEDOutputLostReplyRetainsOwnership(t *testing.T) {
	for _, boundary := range []string{"intent", "domain-reply", "scope-reply", "aggregate"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			aggregate := boundary == "aggregate"
			if aggregate {
				boundary = "intent"
			}
			f := newMappingFixture(t, false, true, true, aggregate)
			if !aggregate {
				_, err := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
				testutil.Must(t, err)
			}
			raw, err := retainedEnrolledRuntime(f.d, f.paths, f.journal(t))
			testutil.Must(t, err)
			var input install.RuntimeInput
			testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
			c, err := input.Resolve()
			testutil.Must(t, err)
			f.api.states["foreign-fault"] = led.State{ID: "foreign-fault", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}
			original := f.o.Manifest
			child := planTree(t, filepath.Join(f.paths.Control, "signal"))
			enrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			f.r.Desired.Notification.StatusLEDs.Enabled = false
			f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
			testutil.Must(t, err)
			stop := errors.New("lost LED output acknowledgement")
			fired, calls := false, 0
			receipts := map[string]string{}
			l := notificationRemappingLifecycle{notificationMappingLifecycle: notificationMappingLifecycle{inspect: f.inspect}}
			l.delivery.review = func(context.Context, string, string) (install.DeliveryAdmissionRequest, bool, error) {
				return install.DeliveryAdmissionRequest{}, false, nil
			}
			l.scopeInspect = func(_, op, _, _ string) (string, bool, error) {
				receipt := receipts[op]
				return receipt, receipt != "", nil
			}
			l.delivery.scopeInspect = l.scopeInspect
			l.scope = func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, change func(backup.ProducerReservation) (string, error)) (string, error) {
				if receipt := receipts[r.Operation]; receipt != "" {
					return receipt, nil
				}
				receipt, err := change(f.reservation)
				if err != nil {
					return "", err
				}
				if boundary == "scope-reply" && !fired {
					fired = true
					return "", stop
				}
				receipts[r.Operation] = receipt
				return receipt, nil
			}
			output := notificationLEDLifecycle{inspect: f.inspect, set: func(ctx context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
				if expected != protocol.Hash(f.o.Manifest) {
					return f.o.Manifest, errors.New("unexpected repeated domain mutation")
				}
				if boundary == "intent" && !fired {
					fired = true
					return f.o.Manifest, stop
				}
				op := testutil.ID(t, "n")
				_, err := worker.ConfigureLEDOutput(ctx, f.paths.State, c.Worker, worker.LEDOutputRequest{Identity: original.Identity, Operation: op, Socket: c.LEDSocket, Disabled: disabled}, f.api)
				if err != nil {
					return f.o.Manifest, err
				}
				calls++
				f.o.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: op, Disabled: disabled}
				if boundary == "domain-reply" && !fired {
					fired = true
					return f.o.Manifest, stop
				}
				return f.o.Manifest, nil
			}}
			_, handled, err := reconcileEnrolledLEDOutput(ctx, f.r, f.paths, "/helper", l, output)
			if !handled || !errors.Is(err, stop) {
				t.Fatal("boundary missed", handled, err)
			}
			// A damaged pending intent cannot authorize even an independent native restart.
			pending := f.journal(t)
			intentName := "led-output-" + pending.Children[0].Operation + ".json"
			var intent notificationLEDIntent
			testutil.Must(t, f.d.ReadJSON(intentName, &intent))
			damaged := intent
			damaged.DesiredHash = protocol.Hash("other policy")
			testutil.Must(t, f.d.WriteJSON(intentName, damaged, true))
			native := f.native.lifecycle
			native.gateway = f.inspect
			restarts := 0
			stopped := true
			native.inspect = func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
				if stopped {
					return *f.service, false, install.FreshSignalResumeRequired{Reference: *f.o.Manifest.FreshSignal}
				}
				return *f.service, true, nil
			}
			native.resume = func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
				if ref != *original.FreshSignal {
					t.Fatal("foreign native restart")
				}
				restarts++
				stopped = false
				return *f.service, nil
			}
			resume := f.r
			resume.Action = "resume"
			if err = resumeCompletedSignalChild(ctx, resume, f.paths, "/helper", f.native.nativeSource, native); err == nil || restarts != 0 {
				t.Fatal("damaged output restarted native service", err)
			}
			testutil.Must(t, f.d.WriteJSON(intentName, intent, true))
			testutil.Must(t, resumeCompletedSignalChild(ctx, resume, f.paths, "/helper", f.native.nativeSource, native))
			for range 2 {
				result, handled, err := reconcileEnrolledLEDOutput(ctx, f.r, f.paths, "/helper", l, output)
				if err != nil || !handled || result.LEDOutput == nil || !result.LEDOutput.Disabled {
					t.Fatal(result, handled, err)
				}
			}
			if calls != 1 || restarts != 1 || !reflect.DeepEqual(child, planTree(t, filepath.Join(f.paths.Control, "signal"))) {
				t.Fatal("reply replayed output/native ownership", calls, restarts)
			}
			after, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			if string(after) != string(enrollment) {
				t.Fatal("enrollment rewritten")
			}
			if _, ok := f.api.states["foreign-fault"]; !ok {
				t.Fatal("foreign output cleared")
			}
			want := original
			want.LEDOutput = f.o.Manifest.LEDOutput
			if protocol.Hash(want) != protocol.Hash(f.o.Manifest) {
				t.Fatal("output changed runtime/provider/identity")
			}
			if err = verifyEnrolledOutputReceipt(f.d, f.journal(t), f.o.Manifest, l.scopeInspect); err != nil {
				t.Fatal(err)
			}
			testutil.Must(t, os.Remove(filepath.Join(f.paths.Control, intentName)))
			if _, _, err = reconcileEnrolledLEDOutput(ctx, f.r, f.paths, "/helper", l, output); err == nil {
				t.Fatal("missing completed intent recreated")
			}
		})
	}
}

func TestDeliveryLEDOutputResumesNativeSignalWithoutRelinking(t *testing.T) {
	ctx := context.Background()
	f := newMappingFixture(t, false, true, true)
	_, err := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
	testutil.Must(t, err)
	prior := f.journal(t)
	testutil.Must(t, f.d.WriteJSON("history/"+prior.ID+".json", prior, false))
	f.o.Manifest.Mode = "delivery"
	f.o.Manifest.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: testutil.ID(t, "n"), Hash: protocol.Hash("synthetic delivery proof")}
	pins := prior.Pins
	pins.PreviousOperation, pins.Previous = prior.ID, prior.Digest()
	delivery := OperationJournal{Version: 1, ID: testutil.ID(t, "n"), Pins: pins, Complete: true, Children: []ChildReference{{Name: "notification-delivery", Operation: testutil.ID(t, "n"), Complete: true, Receipt: protocol.Hash(f.o.Manifest)}}}
	testutil.Must(t, f.d.WriteJSON("operation.json", delivery, true))
	original := f.o.Manifest
	nativeTree := planTree(t, filepath.Join(f.paths.Control, "signal"))
	stopped, restarts := false, 0
	native := f.native.lifecycle
	native.gateway = f.inspect
	native.inspect = func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
		if stopped {
			return *f.service, false, install.FreshSignalResumeRequired{Reference: *original.FreshSignal}
		}
		return *f.service, true, nil
	}
	native.resume = func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
		if ref != *original.FreshSignal {
			t.Fatal("native ownership changed")
		}
		stopped = false
		restarts++
		return *f.service, nil
	}
	receipts := map[string]string{}
	lost := true
	calls := 0
	l := deliveryOutputLifecycle{
		inputs: func(r ControlRequest, p ResolvedPaths, d *safefs.Directory, b config.DesiredWorkers) error {
			return verifyLEDOutputInputs(r, p, d, b)
		},
		review: func(context.Context, string, string, bool) (install.ManagedManifest, error) { return f.o.Manifest, nil },
		nativeResume: func(ctx context.Context, r ControlRequest, p ResolvedPaths) error {
			return resumeCompletedSignalChild(ctx, r, p, "/helper", f.native.nativeSource, native)
		},
		output: notificationLEDLifecycle{inspect: f.inspect, set: func(context.Context, string, string, bool) (install.ManagedManifest, error) {
			if lost {
				lost = false
				return f.o.Manifest, errors.New("lost output reply")
			}
			calls++
			f.o.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: testutil.ID(t, "n"), Disabled: true}
			return f.o.Manifest, nil
		}},
		scope: func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, change func(backup.ProducerReservation) (string, error)) (string, error) {
			if receipt := receipts[r.Operation]; receipt != "" {
				return receipt, nil
			}
			receipt, err := change(f.reservation)
			if err == nil {
				receipts[r.Operation] = receipt
			}
			return receipt, err
		},
		scopeInspect: func(_, op, _, _ string) (string, bool, error) { v := receipts[op]; return v, v != "", nil },
		connect:      func(context.Context, string, string, string) error { return nil },
	}
	f.r.Desired.Notification.StatusLEDs.Enabled = false
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	_, handled, err := reconcileDeliveryLEDOutput(ctx, f.r, f.paths, l)
	if !handled || err == nil {
		t.Fatal("missed pending output", handled, err)
	}
	pending := f.journal(t)
	name := "led-output-" + pending.Children[0].Operation + ".json"
	var intent notificationLEDIntent
	testutil.Must(t, f.d.ReadJSON(name, &intent))
	damaged := intent
	damaged.DesiredHash = protocol.Hash("other policy")
	testutil.Must(t, f.d.WriteJSON(name, damaged, true))
	f.r.Action = "resume"
	stopped = true
	if _, _, err = reconcileDeliveryLEDOutput(ctx, f.r, f.paths, l); err == nil || restarts != 0 {
		t.Fatal("damaged intent restarted native service", err)
	}
	testutil.Must(t, f.d.WriteJSON(name, intent, true))
	for range 2 {
		result, handled, err := reconcileDeliveryLEDOutput(ctx, f.r, f.paths, l)
		if !handled || err != nil || result.State != "delivery_admitted" {
			t.Fatal(result, handled, err)
		}
	}
	if restarts != 1 || calls != 1 || !reflect.DeepEqual(nativeTree, planTree(t, filepath.Join(f.paths.Control, "signal"))) {
		t.Fatal("output replaced or replayed native ownership", restarts, calls)
	}
	testutil.Must(t, verifyDeliveryOutputReceipt(f.d, f.journal(t), f.o.Manifest, l.scopeInspect))
}
