// Package ledhost owns the administrator-managed LED daemon prerequisite.
// It is separate from notification identities, delivery and qualification.
package ledhost

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const Version = 1

// Spec contains only reviewed host policy, never caller-selected destination
// files, service commands or privilege settings. DevicePath is rediscovered
// immediately before installation, not trusted because it was sent by a client.
type Spec struct {
	PowerDevice     string `json:"power_device,omitempty"`
	PowerDevicePath string `json:"power_device_path,omitempty"`
	Version         int    `json:"version"`
	UID             int    `json:"uid"`
	Device          string `json:"device"`
	DevicePath      string `json:"device_path"`
	ArtifactSHA256  string `json:"artifact_sha256"`
}

type Plan struct {
	Spec    Spec              `json:"spec"`
	Hash    string            `json:"hash"`
	Socket  string            `json:"socket"`
	Files   map[string]string `json:"files"`
	Actions []string          `json:"actions"`
}

var deviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var devicePath = regexp.MustCompile(`^/sys/devices/[A-Za-z0-9/_.:+-]+$`)

func (s Spec) Validate() error {
	if (s.Version != 1 && s.Version != 2) || s.UID <= 0 || uint64(s.UID) > 1<<31-1 || !deviceName.MatchString(s.Device) || !devicePath.MatchString(s.DevicePath) || filepath.Clean(s.DevicePath) != s.DevicePath || strings.Contains(strings.ToLower(s.Device), "pwr") || strings.Contains(strings.ToLower(s.Device), "power") || !safefs.DigestPattern.MatchString(s.ArtifactSHA256) {
		return fmt.Errorf("invalid managed LED prerequisite")
	}
	if s.Version == 1 && (s.PowerDevice != "" || s.PowerDevicePath != "") {
		return fmt.Errorf("power LED requires an explicit version-2 prerequisite")
	}
	if s.Version == 2 && (!deviceName.MatchString(s.PowerDevice) || !powerLabel(s.PowerDevice) || !devicePath.MatchString(s.PowerDevicePath) || filepath.Clean(s.PowerDevicePath) != s.PowerDevicePath || s.PowerDevice == s.Device || s.PowerDevicePath == s.DevicePath || strings.HasPrefix(s.PowerDevicePath, s.DevicePath+"/") || strings.HasPrefix(s.DevicePath, s.PowerDevicePath+"/")) {
		return fmt.Errorf("power LED requires a distinct discovered power device")
	}
	return nil
}

func powerLabel(name string) bool {
	name = strings.ToLower(name)
	return name == "pwr" || name == "power" || strings.HasSuffix(name, ":pwr") || strings.HasSuffix(name, ":power")
}

// ResolveSpec selects only unambiguous observed devices. Power hardware is
// included only on explicit opt-in, and both paths are resolved by the caller's
// read-only discovery implementation. The administrator rechecks them on apply.
func ResolveSpec(uid int, devices []string, activity string, power bool, artifact string, discover func(string) (string, error)) (Spec, error) {
	s := Spec{Version: 1, UID: uid, ArtifactSHA256: artifact}
	var err error
	s.Device, err = SelectDevice(devices, activity)
	if err != nil {
		return s, err
	}
	s.DevicePath, err = discover(s.Device)
	if err != nil {
		return s, err
	}
	if power {
		s.PowerDevice, err = SelectPowerDevice(devices)
		if err != nil {
			return s, err
		}
		s.Version = 2
		s.PowerDevicePath, err = discover(s.PowerDevice)
		if err != nil {
			return s, err
		}
	}
	return s, s.Validate()
}

func SelectPowerDevice(devices []string) (string, error) {
	selected := ""
	for _, name := range devices {
		if !powerLabel(name) || !deviceName.MatchString(name) {
			continue
		}
		if selected != "" {
			return "", fmt.Errorf("multiple power LEDs discovered; use an existing daemon with an explicitly configured red device")
		}
		selected = name
	}
	if selected == "" {
		return "", fmt.Errorf("power LED opt-in requires one discovered PWR/power device")
	}
	return selected, nil
}

func Socket(uid int) string { return "/run/deploy-led-host-" + strconv.Itoa(uid) + ".sock" }
func Root(uid int) string   { return "/var/lib/deploy-led-host/" + strconv.Itoa(uid) }
func Unit(uid int) string   { return "deploy-led-host-" + strconv.Itoa(uid) }

// SelectDevice chooses only an observed ACT label. Ambiguous/generic device
// names require an explicit selection, rather than guessing a sysfs path.
func SelectDevice(devices []string, selected string) (string, error) {
	var matches []string
	for _, name := range devices {
		if !deviceName.MatchString(name) {
			continue
		}
		lower := strings.ToLower(name)
		if strings.Contains(lower, "power") || strings.Contains(lower, "pwr") {
			continue
		}
		if selected != "" && selected == name || selected == "" && (lower == "act" || strings.HasSuffix(lower, ":act")) {
			matches = append(matches, name)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("select one discovered activity LED with status_leds.daemon.device; no unambiguous ACT device was found")
	}
	return matches[0], nil
}

func Build(s Spec) (Plan, error) {
	p := Plan{}
	if err := s.Validate(); err != nil {
		return p, err
	}
	root, unit := Root(s.UID), Unit(s.UID)
	power, red, writable := s.Version == 2, "PWR", s.DevicePath
	if power {
		red, writable = s.PowerDevice, writable+" "+s.PowerDevicePath
	}
	config := fmt.Sprintf(`[api]
socket_path = %q
[hardware]
driver = "sysfs"
allow_power_led_override = %t
[led.green]
enabled = true
device = %q
default_pattern = "off"
[led.red]
enabled = %t
device = %q
default_pattern = "off"
[persistence]
enabled = true
path = %q
`, Socket(s.UID), power, s.Device, power, red, root+"/state/states.json")
	service := fmt.Sprintf(`[Unit]
Description=Deploy managed activity LED for UID %d
Requires=%s.socket
After=%s.socket
[Service]
Type=notify
ExecStart=/usr/bin/python3 -I %s/daemon.pyz --config %s/config.toml
Restart=on-failure
RestartSec=3
WatchdogSec=30s
TimeoutStopSec=15s
NotifyAccess=main
RuntimeDirectory=pi-status-led
RuntimeDirectoryMode=0700
RuntimeDirectoryPreserve=yes
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=%s/state %s
RestrictAddressFamilies=AF_UNIX
RestrictSUIDSGID=true
CapabilityBoundingSet=
UMask=0077
[Install]
WantedBy=multi-user.target
`, s.UID, unit, unit, root, root, root, writable)
	socket := fmt.Sprintf(`[Unit]
Description=Deploy managed LED API for UID %d
[Socket]
ListenStream=%s
SocketUser=%d
SocketMode=0600
Accept=no
RemoveOnStop=yes
[Install]
WantedBy=sockets.target
`, s.UID, Socket(s.UID), s.UID)
	p = Plan{Spec: s, Hash: "", Socket: Socket(s.UID), Files: map[string]string{
		root + "/config.toml":                      config,
		"/etc/systemd/system/" + unit + ".service": service,
		"/etc/systemd/system/" + unit + ".socket":  socket,
	}, Actions: []string{
		"install the exact verified offline daemon archive under " + root,
		"create private persistent daemon state; preserve the common hardware lock inodes",
		"grant this root service write access only to the selected LED device directory",
		"enable and start the owned system socket and daemon; grant API access only to the target UID",
		"start with output off; retain physical qualification as a separate observation",
	}}
	if power {
		p.Actions = append(p.Actions, "explicitly enable the selected power LED override; grant access only to that additional device and retain its physical qualification")
	}
	p.Hash = protocol.Hash(struct {
		Spec   Spec
		Socket string
		Files  map[string]string
	}{p.Spec, p.Socket, p.Files})
	return p, nil
}
