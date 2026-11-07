package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDiagnosticOnceRetainsAmbiguousOutcome(t *testing.T) {
	for _, boundary := range []string{"prepared", "sending", "sent", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			spec, i, _, _ := managedFixture(t)
			m, err := i.prepare(context.Background(), "setup", spec)
			if err != nil {
				t.Fatal(err)
			}
			sends := 0
			send := func(ctx context.Context, c Runtime, _ ManagedManifest, name string, message protocol.Message) provider.Result {
				if name != "telegram" || message.ID == "" {
					t.Fatal("invalid diagnostic")
				}
				sends++
				return provider.Result{Accepted: true}
			}
			stop := errors.New("lost reply")
			first, err := testDeliveryOnce(context.Background(), spec.Root, protocol.Hash(m), send, func(stage string) error {
				if stage == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal("fault not reached", err)
			}
			result, err := testDeliveryOnce(context.Background(), spec.Root, protocol.Hash(m), send, nil)
			if err != nil {
				t.Fatal(err)
			}
			expected := 1
			status := "provider_accepted"
			if boundary == "sending" {
				expected = 0
				status = "delivery_unknown"
			}
			if boundary == "sent" {
				status = "delivery_unknown"
			}
			if sends != expected || result.Status != status || result.Qualified || result.Operation != first.Operation || result.Identity != m.Identity {
				t.Fatal("diagnostic replay or false qualification", sends, result)
			}
			if _, err = testDeliveryOnce(context.Background(), spec.Root, protocol.Hash(m), send, nil); err != nil || sends != expected {
				t.Fatal("repeat sent again", err)
			}
		})
	}
}

func TestDiagnosticFreshSignalLostReplyDoesNotResend(t *testing.T) {
	spec, i, _, _ := freshSignalGatewayFixture(t, true)
	ctx := context.Background()
	m, err := i.ensureVerification(ctx, spec)
	testutil.Must(t, err)
	sends := 0
	send := func(_ context.Context, c Runtime, installed ManagedManifest, name string, _ protocol.Message) provider.Result {
		if installed.FreshSignal == nil || c.Providers[name].Kind != "signal" {
			t.Fatal("Signal ownership lost")
		}
		sends++
		return provider.Result{Accepted: true}
	}
	_, err = testDeliveryOnce(ctx, spec.Root, protocol.Hash(m), send, func(stage string) error {
		if stage == "sent" {
			return errors.New("lost send response")
		}
		return nil
	})
	if err == nil {
		t.Fatal("interruption lost")
	}
	result, err := testDeliveryOnce(ctx, spec.Root, protocol.Hash(m), send, nil)
	testutil.Must(t, err)
	if sends != 1 || result.Status != "delivery_unknown" || result.Qualified || result.Provider != "signal" {
		t.Fatal("Signal diagnostic replayed or qualified", result)
	}
}

func TestDiagnosticUsesDeclaredPrimaryWithoutFallback(t *testing.T) {
	spec, _, c, signal := freshSignalGatewayFixture(t, true)
	_ = spec
	c.Providers["telegram"] = provider.Profile{Kind: "telegram"}
	m := ManagedManifest{FreshSignal: &FreshSignalReference{Root: signal.Spec.Root, ManifestHash: signal.ManifestHash}}
	route := c.Worker.Routes["alerts"]
	for _, primary := range []string{"signal", "telegram"} {
		route.Primary = primary
		c.Worker.Routes["alerts"] = route
		selected, err := diagnosticProvider(c, m)
		testutil.Must(t, err)
		if selected != primary {
			t.Fatal("wrong primary", selected)
		}
	}
	delete(c.Worker.Routes, "alerts")
	if _, err := diagnosticProvider(c, m); err == nil {
		t.Fatal("ambiguous primary guessed")
	}
}
func TestDiagnosticMissingRecordOrLockCannotResend(t *testing.T) {
	for _, missing := range []string{".json", ".lock"} {
		t.Run(missing, func(t *testing.T) {
			spec, i, _, _ := managedFixture(t)
			m, err := i.prepare(context.Background(), "setup", spec)
			if err != nil {
				t.Fatal(err)
			}
			sends := 0
			send := func(context.Context, Runtime, ManagedManifest, string, protocol.Message) provider.Result {
				sends++
				return provider.Result{Accepted: true}
			}
			if _, err = testDeliveryOnce(context.Background(), spec.Root, protocol.Hash(m), send, nil); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filepath.Join(spec.Root, "diagnostics", protocol.Hash(m)+missing)); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if _, err = testDeliveryOnce(context.Background(), spec.Root, protocol.Hash(m), send, nil); err == nil {
					t.Fatal("lost diagnostic authority regenerated")
				}
			}
			if sends != 1 {
				t.Fatal("replayed unknown diagnostic")
			}
		})
	}
}

func TestSelectedDiagnosticRetainsPrimaryAndFallbackIndependently(t *testing.T) {
	for _, boundary := range []string{"prepared", "sending", "sent", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			spec, i, _, _ := freshSignalGatewayFixture(t, true)
			telegramSpec, _, _, _ := managedFixture(t)
			telegram, err := ReadRuntime(telegramSpec.RuntimeFile)
			testutil.Must(t, err)
			raw, err := os.ReadFile(spec.RuntimeFile)
			testutil.Must(t, err)
			var input RuntimeInput
			testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
			input.Providers["telegram"] = telegram.Providers["telegram"]
			route := input.Worker.Routes["alerts"]
			route.Primary, route.Fallback = "telegram", "signal"
			route.Revision = "diagnostic-fixture"
			input.Worker.Routes["alerts"] = route
			c, err := input.Resolve()
			testutil.Must(t, err)
			raw, err = input.Bytes()
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
			spec.State = filepath.Join(filepath.Dir(spec.State), "combined-state")
			_, err = worker.BootstrapGateway(context.Background(), spec.State, testutil.ID(t, "n"), c.Worker)
			testutil.Must(t, err)
			ctx := context.Background()
			m, err := i.ensureVerification(ctx, spec)
			testutil.Must(t, err)
			receipt := protocol.Hash(m)
			sends := map[string]int{}
			send := func(_ context.Context, c Runtime, installed ManagedManifest, selected string, _ protocol.Message) provider.Result {
				if c.Worker.Routes["alerts"].Primary != "telegram" || c.Worker.Routes["alerts"].Fallback != "signal" || installed.FreshSignal == nil {
					t.Fatal("diagnostic changed installed route or account ownership")
				}
				sends[selected]++
				return provider.Result{Accepted: true}
			}
			primary, err := testDeliveryOnce(ctx, spec.Root, receipt, send, nil)
			testutil.Must(t, err)
			explicit, err := testDeliveryOnceFor(ctx, spec.Root, receipt, "telegram", send, nil)
			testutil.Must(t, err)
			if primary != explicit || sends["telegram"] != 1 || sends["signal"] != 0 {
				t.Fatal("explicit primary bypassed prior diagnostic or sent fallback", sends)
			}
			lost := errors.New("lost diagnostic reply")
			first, err := testDeliveryOnceFor(ctx, spec.Root, receipt, "signal", send, func(at string) error {
				if at == boundary {
					return lost
				}
				return nil
			})
			if !errors.Is(err, lost) {
				t.Fatal("boundary not reached", err)
			}
			got, err := testDeliveryOnceFor(ctx, spec.Root, receipt, "signal", send, nil)
			testutil.Must(t, err)
			wantSends, wantStatus := 1, "provider_accepted"
			if boundary == "sending" {
				wantSends = 0
			}
			if boundary == "sending" || boundary == "sent" {
				wantStatus = "delivery_unknown"
			}
			if sends["signal"] != wantSends || sends["telegram"] != 1 || got.Status != wantStatus || got.Operation != first.Operation || got.Operation == primary.Operation || got.Qualified || got.Provider != "signal" {
				t.Fatal("selected diagnostic replayed or claimed qualification", got, sends)
			}
			if _, err = testDeliveryOnceFor(ctx, spec.Root, receipt, "unknown", send, nil); err == nil {
				t.Fatal("unknown provider accepted")
			}
			// Missing selected-provider authority cannot be recreated under the
			// default provider's independent diagnostic record.
			key := receipt + "-" + protocol.Hash("signal")
			testutil.Must(t, os.Remove(filepath.Join(spec.Root, "diagnostics", key+".json")))
			if _, err = testDeliveryOnceFor(ctx, spec.Root, receipt, "signal", send, nil); err == nil || sends["signal"] != wantSends {
				t.Fatal("missing fallback receipt allowed another send", err, sends)
			}
			retained, err := testDeliveryOnce(ctx, spec.Root, receipt, send, nil)
			testutil.Must(t, err)
			if retained != primary || sends["telegram"] != 1 {
				t.Fatal("fallback damaged primary authority")
			}
		})
	}
}
