package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestManagedTelegramProfileAdditionRemovalRetainsSignal(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "bootstrap-providers-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, _, native := freshSignalGatewayFixture(t, true)
			i.bootstrapLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapProvidersProbe = i.bootstrapLimitsProbe
			current, err := i.ensureVerification(ctx, spec)
			testutil.Must(t, err)
			original := current
			telegram := provider.Profile{Kind: "telegram", TokenFile: filepath.Join(t.TempDir(), "token"), ChatFile: filepath.Join(t.TempDir(), "chat"), Destination: provider.TelegramIdentity("123:fixture", "456")}
			testutil.Must(t, os.WriteFile(telegram.TokenFile, []byte("123:fixture"), 0600))
			testutil.Must(t, os.WriteFile(telegram.ChatFile, []byte("456"), 0600))
			for _, present := range []bool{true, false, true} {
				raw, err := os.ReadFile(spec.RuntimeFile)
				testutil.Must(t, err)
				var input RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
				if present {
					input.Providers["telegram"] = telegram
				} else {
					delete(input.Providers, "telegram")
				}
				raw, err = input.Bytes()
				testutil.Must(t, err)
				testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
				stop := errors.New("profile publication interrupted")
				i.fault = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(current), "bootstrap-providers"); !errors.Is(err, stop) {
					t.Fatal("missed boundary", boundary, err)
				}
				i.fault = nil
				next, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(current), "bootstrap-providers")
				testutil.Must(t, err)
				services := i.services.(*fakeDeliveryServices)
				starts, stops := services.starts, services.stops
				for range 2 {
					repeated, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(current), "bootstrap-providers")
					testutil.Must(t, err)
					if protocol.Hash(repeated) != protocol.Hash(next) || services.starts != starts || services.stops != stops {
						t.Fatal("completed retry changed service")
					}
				}
				if next.Identity != original.Identity || next.FreshSignal == nil || *next.FreshSignal != *original.FreshSignal {
					t.Fatal("profile migration changed ownership")
				}
				got, err := InspectFreshSignal(ctx, *next.FreshSignal, false)
				testutil.Must(t, err)
				if protocol.Hash(got) != protocol.Hash(native) {
					t.Fatal("native service receipt changed")
				}
				current = next
			}
		})
	}
}

func TestManagedTelegramUpdateRetainsNativeSignal(t *testing.T) {
	for _, boundary := range []string{"prepared", "bootstrap-providers-migrated", "published", "start-returned"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, _, native := freshSignalGatewayFixture(t, true, true)
			i.bootstrapLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapProvidersProbe = i.bootstrapLimitsProbe
			before, err := i.ensureVerification(ctx, spec)
			testutil.Must(t, err)
			// These are real owned account/link/inbox/unit records. Manager calls
			// are offline adapters, and no provider diagnostic is requested.
			files := map[string][]byte{}
			for _, root := range []string{native.Spec.Root, native.Spec.AccountState, native.Spec.Inbox.State} {
				testutil.Must(t, filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !e.Type().IsRegular() {
						return nil
					}
					files[path], err = os.ReadFile(path)
					return err
				}))
			}
			input := changeBootstrapProviderInput(t, spec, boundary != "start-returned")
			stop := errors.New("lost combined provider update reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "bootstrap-providers"); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "bootstrap-providers")
			testutil.Must(t, err)
			if after.Identity != before.Identity || after.FreshSignal == nil || *after.FreshSignal != *before.FreshSignal {
				t.Fatal("native or gateway ownership changed")
			}
			for path, body := range files {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != string(body) {
					t.Fatalf("native history changed: %s: %v", path, err)
				}
			}
			got, err := InspectFreshSignal(ctx, *after.FreshSignal, false)
			testutil.Must(t, err)
			if protocol.Hash(got) != protocol.Hash(native) {
				t.Fatal("native receipt changed")
			}
			services := i.services.(*fakeDeliveryServices)
			starts, stops := services.starts, services.stops
			for range 2 {
				m, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "bootstrap-providers")
				testutil.Must(t, err)
				if protocol.Hash(m) != protocol.Hash(after) || services.starts != starts || services.stops != stops {
					t.Fatal("replay restarted the gateway")
				}
			}
			// A subsequent budget update must keep the same native owner too.
			input.Worker.MaxMessages++
			raw, err := input.Bytes()
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
			m, err := i.migrateBootstrapLimits(ctx, spec, protocol.Hash(after))
			testutil.Must(t, err)
			if m.FreshSignal == nil || *m.FreshSignal != *before.FreshSignal {
				t.Fatal("following update discarded native ownership")
			}
		})
	}
}

func TestManagedTelegramUpdateRefusesSignalChanges(t *testing.T) {
	for _, change := range []string{"destination", "socket", "remove-profile", "remove-owner", "missing-native-record", "remove-used-telegram", "old-helper"} {
		t.Run(change, func(t *testing.T) {
			spec, i, _, native := freshSignalGatewayFixture(t, true, true)
			i.bootstrapLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapProvidersProbe = i.bootstrapLimitsProbe
			before, err := i.ensureVerification(context.Background(), spec)
			testutil.Must(t, err)
			input := changeBootstrapProviderInput(t, spec, false)
			p := input.Providers["signal"]
			switch change {
			case "destination":
				p.Destination = protocol.Hash("another audience")
				input.Providers["signal"] = p
			case "socket":
				p.Socket += ".other"
				input.Providers["signal"] = p
			case "remove-profile":
				delete(input.Providers, "signal")
			case "remove-owner":
				spec.FreshSignal = nil
			case "remove-used-telegram":
				delete(input.Providers, "telegram")
			case "old-helper":
				i.bootstrapProvidersProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old helper") }
			case "missing-native-record":
				testutil.Must(t, os.Remove(filepath.Join(native.Spec.Root, "complete.json")))
			}
			raw, err := input.Bytes()
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
			services := i.services.(*fakeDeliveryServices)
			starts, stops := services.starts, services.stops
			if _, err = i.migrateBootstrapPolicy(context.Background(), spec, protocol.Hash(before), "bootstrap-providers"); err == nil {
				t.Fatal("native change accepted as Telegram update")
			}
			if services.starts != starts || services.stops != stops {
				t.Fatal("invalid update changed service activity")
			}
		})
	}
}
