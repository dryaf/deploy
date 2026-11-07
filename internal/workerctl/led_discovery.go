package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/ledhost"
)

func declaredLEDSocket(d config.LEDDeclaration, home string, uid ...int) (string, error) {
	if !d.Enabled {
		return "", nil
	}
	if d.Daemon.Mode == "managed" {
		if len(uid) != 1 || uid[0] <= 0 {
			return "", fmt.Errorf("managed LED socket requires resolved target UID")
		}
		socket := ledhost.Socket(uid[0])
		if d.Socket != "" && d.Socket != socket {
			return "", fmt.Errorf("managed LED socket override requires a daemon migration; use the default or an explicitly existing daemon")
		}
		return socket, nil
	}
	if d.Daemon.Mode != "existing" || d.Socket == "" {
		return "", fmt.Errorf("LED daemon provisioning required")
	}
	return ResolveTargetPath(d.Socket, home)
}

// inspectDeclaredLED authenticates the selected existing or provisioned daemon.
// Facts come from target inspection; daemon status must name the actual discovered
// sysfs device. No inferred pathname, permission edits, or physical qualification.
func inspectDeclaredLED(ctx context.Context, r ControlRequest, probe func(context.Context, string) (led.Endpoint, led.DaemonStatus, error)) (*led.Endpoint, error) {
	if r.Desired.Notification == nil || !r.Desired.Notification.StatusLEDs.Enabled {
		return nil, nil
	}
	d := r.Desired.Notification.StatusLEDs
	socket, err := declaredLEDSocket(d, r.Facts.Home, r.Facts.UID)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(r.Facts.Board), "raspberry pi") || len(r.Facts.LEDs) == 0 {
		return nil, fmt.Errorf("unsupported_status_led: target inspection found no supported Raspberry Pi LEDs")
	}
	p, status, err := probe(ctx, socket)
	if err != nil {
		return nil, fmt.Errorf("led_daemon_unavailable: %w", err)
	}
	if d.Daemon.Mode == "managed" && (p.OwnerUID != r.Facts.UID || p.PeerUID != 0) {
		return nil, fmt.Errorf("managed LED daemon endpoint has unexpected ownership")
	}
	if p.Validate() != nil || p.Socket != socket {
		return nil, fmt.Errorf("LED endpoint inspection changed selected socket")
	}
	if status.Daemon != "pistatusd" || status.Version != "0.1.0" || status.Degraded {
		return nil, fmt.Errorf("LED daemon is incompatible or degraded")
	}
	targets := map[string]bool{"green": true}
	expected := map[string]string{}
	if d.Daemon.Mode == "managed" {
		expected["green"], err = ledhost.SelectDevice(r.Facts.LEDs, d.Daemon.Device)
		if err != nil {
			return nil, err
		}
		if d.PowerLED {
			expected["red"], err = ledhost.SelectPowerDevice(r.Facts.LEDs)
			if err != nil {
				return nil, err
			}
			targets["red"] = true
		}
	}
	patterns := map[string]bool{"blink-slow": true, "blink-fast": true, "heartbeat": true, "blink-1": true, "blink-2": true}
	for _, b := range d.Bindings {
		name := b.LED
		if name == "power" {
			name = "red"
		}
		if name == "red" && !d.PowerLED {
			return nil, fmt.Errorf("power LED requires explicit opt-in")
		}
		targets[name] = true
		for _, style := range b.Styles {
			patterns[style.Pattern] = true
		}
	}
	for name := range targets {
		var actual struct {
			Enabled   bool   `json:"enabled"`
			Available bool   `json:"available"`
			Device    string `json:"device"`
			Default   string `json:"default_pattern"`
		}
		if json.Unmarshal(status.LEDs[name], &actual) != nil || !actual.Enabled || !actual.Available || !slices.Contains(r.Facts.LEDs, actual.Device) || expected[name] != "" && actual.Device != expected[name] || actual.Default != "off" && actual.Default != "blink-slow" {
			return nil, fmt.Errorf("LED %s is unavailable, not a discovered device, or has a positive default; review the existing daemon configuration", name)
		}
	}
	for pattern := range patterns {
		if _, ok := status.Patterns[pattern]; !ok {
			return nil, fmt.Errorf("LED daemon lacks required pattern %s", pattern)
		}
	}
	return &p, nil
}
