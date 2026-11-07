package workerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func combinedProviderUpdateFixture(t *testing.T) (ControlRequest, ResolvedPaths, *install.PolicyReview, notificationLimitsLifecycle) {
	t.Helper()
	r, paths, review, native := signalAdoptionFixture(t, true)
	l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }}
	if _, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l); err != nil || !handled {
		t.Fatal(handled, err)
	}
	r.Action, r.SignalSHA256 = "apply", review.Native.Spec.CLISHA256
	return r, paths, review, notificationLimitsLifecycle{inspect: native.gateway, native: func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
		if ref != *review.Manifest.FreshSignal {
			t.Fatal("native inspection changed reference")
		}
		return *review.Native, nil
	}}
}

func TestCombinedProviderUpdateResumesWithoutNativeMutation(t *testing.T) {
	for _, boundary := range []string{"limits-intent", "domain-reply", "limits-domain-complete", "limits-runtime-published", "limits-inputs-published"} {
		t.Run(boundary, func(t *testing.T) {
			r, paths, review, l := combinedProviderUpdateFixture(t)
			account, inbox := planTree(t, review.Native.Spec.AccountState), planTree(t, review.Native.Spec.Inbox.State)
			child := planTree(t, filepath.Join(paths.Control, "signal"))
			identity, nativeOwner := review.Manifest.Identity, *review.Manifest.FreshSignal
			changeDeclaredTelegramFiles(t, &r, paths, boundary != "domain-reply")
			r.DesiredHash, _ = r.Desired.NotificationFingerprint()
			stop := errors.New("lost combined provider reply")
			l.boundary = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			calls := 0
			l.migrate = func(_ context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
				calls++
				if expected != protocol.Hash(review.Manifest) || spec.FreshSignal == nil || *spec.FreshSignal != nativeOwner {
					t.Fatal("unreviewed domain request")
				}
				raw, err := readLimitsRuntime(spec.RuntimeFile)
				if err != nil {
					t.Fatal(err)
				}
				var input install.RuntimeInput
				if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
					t.Fatal(err)
				}
				c, err := input.Resolve()
				if err != nil {
					t.Fatal(err)
				}
				review.Manifest.RuntimeHash, review.Manifest.ConfigHash = inputDigest(raw), protocol.Hash(c)
				review.Manifest.Revision, _ = safefs.NewID("n")
				review.Manifest.HelperHash, review.Manifest.HelperRevision = review.Manifest.BinaryHash, review.Manifest.Revision
				if boundary == "domain-reply" {
					return review.Manifest, stop
				}
				return review.Manifest, nil
			}
			if _, handled, err := reconcileNotificationProviders(context.Background(), r, paths, "/helper", l); !handled || !errors.Is(err, stop) {
				t.Fatal(handled, err)
			}
			l.boundary = nil
			r.Action = "resume"
			stopped := boundary == "limits-intent"
			restarts := 0
			native := signalSetupLifecycle{gateway: l.inspect, inspect: func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
				if stopped {
					return *review.Native, false, install.FreshSignalResumeRequired{Reference: nativeOwner}
				}
				return *review.Native, true, nil
			}, resume: func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
				if !stopped || ref != nativeOwner {
					t.Fatal("unowned native service restarted")
				}
				restarts++
				stopped = false
				return *review.Native, nil
			}}
			// A missing provider intent must refuse before repairing even an
			// independently stopped native child. Restore the retained receipt
			// to exercise legitimate explicit resumption afterward.
			var pending OperationJournal
			pd, err := safefs.Open(paths.Control)
			if err != nil {
				t.Fatal(err)
			}
			if err = pd.ReadJSON("operation.json", &pending); err != nil {
				t.Fatal(err)
			}
			pd.Close()
			intentPath := filepath.Join(paths.Control, "limits-"+pending.Children[0].Operation+".json")
			if err = os.Rename(intentPath, intentPath+".retained"); err != nil {
				t.Fatal(err)
			}
			if err = resumeCompletedSignalChild(context.Background(), r, paths, "/helper", "", native); err == nil || restarts != 0 {
				t.Fatal("missing intent authorized native restart", err)
			}
			if err = os.Rename(intentPath+".retained", intentPath); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				if err := resumeCompletedSignalChild(context.Background(), r, paths, "/helper", "", native); err != nil {
					t.Fatal("public native resume ordering", err)
				}
				got, handled, err := reconcileNotificationProviders(context.Background(), r, paths, "/helper", l)
				if err != nil || !handled || got.Identity == nil || *got.Identity != identity || calls != 1 {
					t.Fatal(got, handled, err, calls)
				}
			}
			if (boundary == "limits-intent" && restarts != 1) || (boundary != "limits-intent" && restarts != 0) {
				t.Fatal("native restart count", restarts)
			}
			d, err := safefs.Open(paths.Control)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = verifyLEDOutputInputs(r, paths, d, r.Desired); err != nil {
				t.Fatal("later lifecycle inputs", err)
			}
			if got, err := inspectNotificationControl(context.Background(), r, paths, l.inspect, nil); err != nil || got.State != "waiting_for_producers" {
				t.Fatal("status", got, err)
			}
			if got, err := planNotificationControl(context.Background(), r, paths, l.inspect); err != nil || got.State != "up_to_date" {
				t.Fatal("plan", got, err)
			}
			if !reflect.DeepEqual(account, planTree(t, review.Native.Spec.AccountState)) || !reflect.DeepEqual(inbox, planTree(t, review.Native.Spec.Inbox.State)) || !reflect.DeepEqual(child, planTree(t, filepath.Join(paths.Control, "signal"))) {
				t.Fatal("Telegram migration changed retained native history")
			}
			var j OperationJournal
			if err = d.ReadJSON("operation.json", &j); err != nil {
				t.Fatal(err)
			}
			var intent notificationLimitsIntent
			if err = d.ReadJSON("limits-"+j.Children[0].Operation+".json", &intent); err != nil {
				t.Fatal(err)
			}
			if intent.Version != 4 || intent.Inputs.SignalAccount != intent.NextInputs.SignalAccount || intent.Inputs.SignalRecipient != intent.NextInputs.SignalRecipient {
				t.Fatal("native pins not retained")
			}
			changed := intent
			changed.Version = 3
			if validateLimitsIntent(changed) == nil {
				t.Fatal("legacy intent accepted native authority")
			}
			changed = intent
			nextPins := *intent.NextInputs
			nextPins.SignalRecipient = inputDigest([]byte("another recipient"))
			changed.NextInputs = &nextPins
			if validateLimitsIntent(changed) == nil {
				t.Fatal("native credential mutation accepted")
			}
			// Native drift after gateway completion must refuse a replay rather
			// than relink, recreate the child, or accept the completed gateway alone.
			if err = os.Remove(filepath.Join(paths.Control, "signal", "service.json")); err != nil {
				t.Fatal(err)
			}
			if _, _, err = reconcileNotificationProviders(context.Background(), r, paths, "/helper", l); err == nil || calls != 1 {
				t.Fatal("lost native authority accepted", err)
			}
		})
	}
}

func TestCombinedProviderUpdateRejectsNativeDriftBeforeIntent(t *testing.T) {
	for _, change := range []string{"account", "recipient", "child-record", "artifact", "declaration", "native-receipt", "missing-owner", "signal-pins", "legacy-intent"} {
		t.Run(change, func(t *testing.T) {
			r, paths, review, l := combinedProviderUpdateFixture(t)
			changeDeclaredTelegramFiles(t, &r, paths, false)
			switch change {
			case "account":
				if err := os.WriteFile(review.Native.Spec.AccountFile, []byte("+12025550123"), 0600); err != nil {
					t.Fatal(err)
				}
			case "recipient":
				if err := os.WriteFile(review.Native.Spec.RecipientFile, []byte("+12025550124"), 0600); err != nil {
					t.Fatal(err)
				}
			case "child-record":
				if err := os.Remove(filepath.Join(paths.Control, "signal", "service.json")); err != nil {
					t.Fatal(err)
				}
			case "artifact":
				r.SignalSHA256 = inputDigest([]byte("replacement native CLI"))
			case "declaration":
				p := r.Desired.Notification.Providers["signal"]
				p.Data += ".other"
				r.Desired.Notification.Providers["signal"] = p
			case "native-receipt":
				review.Native.ManifestHash = inputDigest([]byte("foreign receipt"))
			case "missing-owner":
				review.Manifest.FreshSignal = nil
			case "signal-pins", "legacy-intent":
				d, err := safefs.Open(paths.Control)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				var pins notificationInputPins
				if err = d.ReadJSON("notification-inputs.json", &pins); err != nil {
					t.Fatal(err)
				}
				if change == "signal-pins" {
					pins.SignalAccount = inputDigest([]byte("foreign account"))
				} else {
					pins.Version = 1
					pins.SignalAccount, pins.SignalRecipient = "", ""
				}
				if err = d.WriteJSON("notification-inputs.json", pins, true); err != nil {
					t.Fatal(err)
				}
			}
			r.DesiredHash, _ = r.Desired.NotificationFingerprint()
			before := planTree(t, paths.Control)
			l.migrate = func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error) {
				t.Fatal("invalid native migration")
				return install.ManagedManifest{}, nil
			}
			if _, handled, err := reconcileNotificationProviders(context.Background(), r, paths, "/helper", l); !handled || err == nil {
				t.Fatal(handled, err)
			}
			if !reflect.DeepEqual(before, planTree(t, paths.Control)) {
				t.Fatal("preflight rejection changed coordinator authority")
			}
		})
	}
}
