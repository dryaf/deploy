package workerctl

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func declaredLEDFixture(t *testing.T) (ControlRequest, ResolvedPaths, led.Endpoint, led.DaemonStatus) {
	t.Helper()
	r, paths := bootstrapControlFixture(t)
	r.Facts.Board = "Raspberry Pi 4 Model B"
	r.Facts.LEDs = []string{"ACT", "PWR"}
	r.Desired.Notification.StatusLEDs = config.LEDDeclaration{Enabled: true, Socket: "/run/pi-status-led.sock", Daemon: config.LEDDaemon{Mode: "existing"}}
	r.DesiredHash, _ = r.Desired.Fingerprint()
	p := led.Endpoint{Version: 1, Socket: "/run/pi-status-led.sock", OwnerUID: 0, PeerUID: 995}
	status := led.DaemonStatus{Daemon: "pistatusd", Version: "0.1.0", LEDs: map[string]json.RawMessage{"green": json.RawMessage(`{"enabled":true,"available":true,"device":"ACT","default_pattern":"off"}`)}, Patterns: map[string]json.RawMessage{}}
	for _, pattern := range []string{"blink-slow", "blink-fast", "heartbeat", "blink-1", "blink-2"} {
		status.Patterns[pattern] = json.RawMessage(`[]`)
	}
	return r, paths, p, status
}

func TestDeclaredLEDDiscoveryAndAggregateBootstrap(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths, endpoint, status := declaredLEDFixture(t)
	calls := 0
	actual, err := inspectDeclaredLED(context.Background(), r, func(_ context.Context, path string) (led.Endpoint, led.DaemonStatus, error) {
		calls++
		if path != endpoint.Socket {
			t.Fatal("changed selected daemon")
		}
		return endpoint, status, nil
	})
	if err != nil || actual == nil || *actual != endpoint || calls != 1 {
		t.Fatal(actual, err)
	}
	if err = directNotificationSupport(r.Desired); err != nil {
		t.Fatal(err)
	}
	unbound, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	if err != nil || !unbound.Bootstrap || unbound.LEDSocket != endpoint.Socket || len(unbound.Worker.Producers) != 0 || len(unbound.Worker.LEDBindings) != 0 {
		t.Fatal("bootstrap invented consumers", unbound, err)
	}
	binding := NotificationBindings{Providers: unbound.Providers, LEDSocket: endpoint.Socket, LEDEndpoint: actual, Producers: map[string]ProducerBinding{"backup-worker": {Producer: protocol.Producer{ID: "backup-reserved", Epoch: "one"}, Generation: "one"}}}
	bound, err := CompileNotification(r.Desired, r.Facts, paths, binding)
	if err != nil || bound.Input.Bootstrap || len(bound.Input.Worker.Producers) != 1 || len(bound.Input.Worker.LEDBindings) != 0 || bound.Input.LEDEndpoint == nil || *bound.Input.LEDEndpoint != endpoint {
		t.Fatal("enrollment changed aggregate output or lost daemon identity", bound, err)
	}
	if _, err = bound.Input.Resolve(); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredLEDDiscoveryRefusesUnprovenHardware(t *testing.T) {
	for _, kind := range []string{"board", "device", "default", "degraded", "version", "pattern", "socket", "power"} {
		t.Run(kind, func(t *testing.T) {
			r, _, p, status := declaredLEDFixture(t)
			switch kind {
			case "board":
				r.Facts.Board = "ordinary-linux-host"
			case "device":
				r.Facts.LEDs = []string{"different"}
			case "default":
				status.LEDs["green"] = json.RawMessage(`{"enabled":true,"available":true,"device":"ACT","default_pattern":"heartbeat"}`)
			case "degraded":
				status.Degraded = true
			case "version":
				status.Version = "unreviewed"
			case "pattern":
				delete(status.Patterns, "heartbeat")
			case "socket":
				p.Socket = "/different/socket"
			case "power":
				r.Desired.Notification.StatusLEDs.Bindings = map[string]config.LEDBindingDeclaration{"power": {LED: "power"}}
			}
			if _, err := inspectDeclaredLED(context.Background(), r, func(context.Context, string) (led.Endpoint, led.DaemonStatus, error) { return p, status, nil }); err == nil {
				t.Fatal("unproven endpoint accepted")
			}
		})
	}
}

func TestDisabledLEDDoesNotContactDaemon(t *testing.T) {
	r, _, _, _ := declaredLEDFixture(t)
	r.Desired.Notification.StatusLEDs.Enabled = false
	actual, err := inspectDeclaredLED(context.Background(), r, func(context.Context, string) (led.Endpoint, led.DaemonStatus, error) {
		t.Fatal("disabled declaration contacted daemon")
		return led.Endpoint{}, led.DaemonStatus{}, nil
	})
	if err != nil || actual != nil {
		t.Fatal(actual, err)
	}
}
