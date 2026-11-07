package ledhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func powerSpec(s Spec) Spec {
	s.Version, s.PowerDevice, s.PowerDevicePath = 2, "PWR", "/sys/devices/platform/leds/leds/PWR"
	return s
}

func TestManagedPowerLEDRequiresExplicitUnambiguousDevices(t *testing.T) {
	discovered := []string{}
	discover := func(name string) (string, error) {
		discovered = append(discovered, name)
		return "/sys/devices/platform/leds/leds/" + name, nil
	}
	legacy, err := ResolveSpec(1000, []string{"ACT", "PWR"}, "", false, strings.Repeat("a", 64), discover)
	if err != nil || legacy.Version != 1 || legacy.PowerDevice != "" || strings.Join(discovered, ",") != "ACT" {
		t.Fatal("implicit power discovery", legacy, discovered, err)
	}
	p, err := Build(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Files[Root(1000)+"/config.toml"], "allow_power_led_override = false") || strings.Contains(p.Files["/etc/systemd/system/"+Unit(1000)+".service"], "/leds/PWR") {
		t.Fatal("default plan grants power access")
	}
	selected, err := ResolveSpec(1000, []string{"ACT", "PWR"}, "", true, legacy.ArtifactSHA256, discover)
	if err != nil || selected != powerSpec(legacy) {
		t.Fatal(selected, err)
	}
	next, err := Build(selected)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hash == next.Hash || !strings.Contains(next.Files[Root(1000)+"/config.toml"], "allow_power_led_override = true") || !strings.Contains(next.Files[Root(1000)+"/config.toml"], "[led.red]\nenabled = true\ndevice = \"PWR\"") || !strings.Contains(next.Files["/etc/systemd/system/"+Unit(1000)+".service"], selected.DevicePath+" "+selected.PowerDevicePath+"\n") {
		t.Fatal("review omits explicit power grant", next)
	}
	for _, names := range [][]string{{"ACT"}, {"ACT", "PWR", "board:power"}, {"ACT", "led1"}} {
		if _, err := ResolveSpec(1000, names, "", true, legacy.ArtifactSHA256, discover); err == nil {
			t.Fatal("guessed power device", names)
		}
	}
	for _, bad := range []Spec{func() Spec { s := selected; s.Version = 1; return s }(), func() Spec { s := selected; s.PowerDevicePath = s.DevicePath; return s }(), func() Spec { s := selected; s.PowerDevice = "ACT"; return s }(), func() Spec { s := selected; s.PowerDevicePath = "/sys/devices"; return s }()} {
		if bad.Validate() == nil {
			t.Fatal("unsafe power prerequisite", bad)
		}
	}
}

func TestManagedPowerLEDRechecksHardwareAndDaemonBeforeAcknowledging(t *testing.T) {
	_, legacy, _, _ := fixture(t)
	s := powerSpec(legacy)
	if err := validateDevices(s, func(name string) (string, error) {
		if name == "PWR" {
			return "/sys/devices/changed/PWR", nil
		}
		return s.DevicePath, nil
	}); err == nil {
		t.Fatal("changed power device accepted")
	}
	devices := map[string]json.RawMessage{"green": []byte(`{"enabled":true,"available":true,"device":"ACT","default_pattern":"off"}`), "red": []byte(`{"enabled":true,"available":true,"device":"PWR","default_pattern":"off"}`)}
	if err := validateDaemonDevices(s, devices); err != nil {
		t.Fatal(err)
	}
	if err := validateDaemonDevices(legacy, devices); err == nil {
		t.Fatal("undeclared power override accepted")
	}
	for _, invalid := range []string{`{"enabled":false}`, `{"enabled":true,"available":false,"device":"PWR","default_pattern":"off"}`, `{"enabled":true,"available":true,"device":"other","default_pattern":"off"}`, `{"enabled":true,"available":true,"device":"PWR","default_pattern":"solid"}`} {
		devices["red"] = []byte(invalid)
		if err := validateDaemonDevices(s, devices); err == nil {
			t.Fatal("incorrect power hardware acknowledged", invalid)
		}
	}
}

func TestManagedPowerLEDInitialInstallResumesAndRefusesInPlaceConversion(t *testing.T) {
	for _, at := range []string{"prepared", "published:config.toml", "files", "started", "receipt", "complete"} {
		t.Run(at, func(t *testing.T) {
			m, legacy, artifact, services := fixture(t)
			s := powerSpec(legacy)
			p, err := Build(s)
			if err != nil {
				t.Fatal(err)
			}
			m.boundary = func(point string) error {
				if point == at {
					return errors.New("lost response")
				}
				return nil
			}
			if _, err := m.install(context.Background(), s, p.Hash, artifact); err == nil {
				t.Fatal("boundary not reached")
			}
			m.boundary = nil
			r, err := m.install(context.Background(), s, p.Hash, artifact)
			if err != nil || !r.Complete || services.starts != 1 || r.Spec != s {
				t.Fatal(r, err)
			}
			if _, err = m.install(context.Background(), s, p.Hash, artifact); err != nil || services.starts != 1 {
				t.Fatal("completed retry replayed", err)
			}
			before, err := os.ReadFile(m.path(Root(s.UID) + "/config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			old, _ := Build(legacy)
			if _, err := m.install(context.Background(), legacy, old.Hash, artifact); err == nil {
				t.Fatal("silently removed power hardware")
			}
			after, _ := os.ReadFile(m.path(Root(s.UID) + "/config.toml"))
			if string(before) != string(after) {
				t.Fatal("changed configuration despite refused migration")
			}
		})
	}
}
