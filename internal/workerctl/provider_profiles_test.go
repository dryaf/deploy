package workerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestUnboundTelegramProfileRemovalAndAdditionRetainsSignal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, boundary := range []string{"limits-intent", "limits-domain-complete", "limits-runtime-published", "limits-inputs-published", "domain-reply"} {
		t.Run(boundary, func(t *testing.T) {
			f := newMappingFixtureWithOptions(t, mappingFixtureOptions{signalPrimary: true, unbound: true}, false, true, true, false, true)
			ctx := context.Background()
			nativeBefore := planTree(t, filepath.Join(f.paths.Control, "signal"))
			telegram := f.r.Desired.Notification.Providers["telegram"]
			telegram.TokenFile, telegram.ChatIDFile = f.input.Providers["telegram"].TokenFile, f.input.Providers["telegram"].ChatFile
			telegram.TokenInput, telegram.ChatIDInput = "", ""
			f.r.Inputs = nil
			stop := errors.New("lost profile reply")
			calls := 0
			l := notificationLimitsLifecycle{inspect: f.inspect, native: func(context.Context, install.FreshSignalReference) (install.FreshSignalManifest, error) {
				return *f.service, nil
			}}
			l.migrate = func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
				if expected != protocol.Hash(f.o.Manifest) {
					t.Fatal("changed predecessor")
				}
				old, err := install.ReadRuntime(f.paths.Runtime)
				testutil.Must(t, err)
				next, err := install.ReadRuntime(spec.RuntimeFile)
				testutil.Must(t, err)
				testutil.Must(t, install.ValidateBootstrapProvidersChange(old, next))
				testutil.Must(t, worker.PreflightBootstrapRoutes(ctx, f.paths.State, f.o.Manifest.Identity, old.Worker, next.Worker))
				raw, err := readLimitsRuntime(spec.RuntimeFile)
				testutil.Must(t, err)
				f.o.Manifest.ConfigHash, f.o.Manifest.RuntimeHash = protocol.Hash(next), inputDigest(raw)
				f.o.Manifest.Revision = testutil.ID(t, "n")
				f.o.Manifest.HelperRevision, f.o.Manifest.HelperHash = f.o.Manifest.Revision, f.o.Manifest.BinaryHash
				calls++
				if boundary == "domain-reply" {
					return f.o.Manifest, stop
				}
				return f.o.Manifest, nil
			}
			for _, present := range []bool{false, true, false, true} {
				profiles := config.ProviderDeclarations{"signal": f.r.Desired.Notification.Providers["signal"]}
				if present {
					profiles["telegram"] = telegram
				}
				f.r.Desired.Notification.Providers = profiles
				var err error
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				l.boundary = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				priorCalls := calls
				if _, handled, err := reconcileNotificationProviders(ctx, f.r, f.paths, "/helper", l); !handled || !errors.Is(err, stop) {
					t.Fatal("missed interruption", handled, err)
				}
				l.boundary = nil
				for range 2 {
					got, handled, err := reconcileNotificationProviders(ctx, f.r, f.paths, "/helper", l)
					if err != nil || !handled || got.State != "waiting_for_producers" || calls != priorCalls+1 {
						t.Fatal("resume", got, handled, err, calls)
					}
				}
				testutil.Must(t, verifyLEDOutputInputs(f.r, f.paths, f.d, f.r.Desired))
				got, err := planNotificationControl(ctx, f.r, f.paths, f.inspect)
				if err != nil || got.State != "up_to_date" {
					t.Fatal("plan", got, err)
				}
				raw, err := readLimitsRuntime(f.paths.Runtime)
				testutil.Must(t, err)
				var pins notificationInputPins
				testutil.Must(t, f.d.ReadJSON("notification-inputs.json", &pins))
				testutil.Must(t, verifyProviderRuntimeInputs(raw, pins))
			}
			if !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal"))) {
				t.Fatal("native account history changed")
			}
		})
	}
}

func TestTelegramProfileChangeRejectsMixedAuthority(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, damage := range []string{"used-route", "signal-removed", "signal-recipient", "commands", "mixed-budget", "new-secret-mode", "foreign-profile", "native-artifact", "native-owner"} {
		t.Run(damage, func(t *testing.T) {
			f := newMappingFixtureWithOptions(t, mappingFixtureOptions{signalPrimary: damage != "used-route", unbound: true}, false, true, true, false, true)
			f.r.Inputs = nil
			profiles := config.ProviderDeclarations{"signal": f.r.Desired.Notification.Providers["signal"]}
			switch damage {
			case "signal-removed":
				profiles = config.ProviderDeclarations{}
			case "signal-recipient":
				p := profiles["signal"]
				p.RecipientFile += ".other"
				profiles["signal"] = p
			case "commands":
				f.r.Desired.Notification.Commands.Enabled = true
			case "mixed-budget":
				f.r.Desired.Notification.Limits.MaxMessages++
			case "new-secret-mode":
				p := f.r.Desired.Notification.Providers["telegram"]
				p.TokenFile, p.ChatIDFile = filepath.Join(t.TempDir(), "token"), f.input.Providers["telegram"].ChatFile
				p.TokenInput, p.ChatIDInput = "", ""
				testutil.Must(t, os.WriteFile(p.TokenFile, []byte("123:new"), 0644))
				profiles["telegram"] = p
			case "native-artifact":
				f.r.SignalSHA256 = inputDigest([]byte("different artifact"))
			case "native-owner":
				f.o.Manifest.FreshSignal = nil
			case "foreign-profile":
				profiles["other"] = config.ProviderDeclaration{}
			}
			f.r.Desired.Notification.Providers = profiles
			f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
			tree := planTree(t, f.paths.Control)
			l := notificationLimitsLifecycle{inspect: f.inspect, native: func(context.Context, install.FreshSignalReference) (install.FreshSignalManifest, error) {
				return *f.service, nil
			}, migrate: func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error) {
				t.Fatal("unreviewed migration")
				return install.ManagedManifest{}, nil
			}}
			if _, handled, err := reconcileNotificationProviders(context.Background(), f.r, f.paths, "/helper", l); !handled || err == nil {
				t.Fatal("accepted invalid profile update", err)
			}
			if !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
				t.Fatal("rejection changed coordinator authority")
			}
		})
	}
}

// An absent Telegram profile has the same empty-input digests as fresh Signal
// bootstrap. Arbitrary retained Telegram hashes must not become an absent profile.
func TestSignalOnlyProviderPinsRejectResidualTelegramAuthority(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	f := newMappingFixtureWithOptions(t, mappingFixtureOptions{signalPrimary: true, unbound: true}, false, true, true, false, true)
	raw, err := readLimitsRuntime(f.paths.Runtime)
	testutil.Must(t, err)
	var input install.RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
	delete(input.Providers, "telegram")
	raw, err = input.Bytes()
	testutil.Must(t, err)
	var pins notificationInputPins
	testutil.Must(t, f.d.ReadJSON("notification-inputs.json", &pins))
	pins.Runtime = inputDigest(raw)
	if verifyProviderRuntimeInputs(raw, pins) == nil {
		t.Fatal("residual credential pins accepted")
	}
	pins.Token, pins.Chat = inputDigest(nil), inputDigest(nil)
	testutil.Must(t, verifyProviderRuntimeInputs(raw, pins))
}
