package ledhost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/ledartifact"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// Discover is read-only. A sysfs class link is expected, but its resolved device
// must be a real kernel device directory, never a caller-supplied arbitrary path.
func Discover(device string) (string, error) {
	if !deviceName.MatchString(device) {
		return "", fmt.Errorf("invalid activity LED name")
	}
	model, err := os.ReadFile("/proc/device-tree/model")
	if err != nil || !strings.Contains(strings.ToLower(string(model)), "raspberry pi") {
		return "", fmt.Errorf("managed LEDs require an observed Raspberry Pi board")
	}
	path, err := filepath.EvalSymlinks("/sys/class/leds/" + device)
	if err != nil || !devicePath.MatchString(path) {
		return "", fmt.Errorf("activity LED device is unavailable")
	}
	for _, name := range []string{"brightness", "max_brightness", "trigger"} {
		info, e := os.Lstat(filepath.Join(path, name))
		if e != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("activity LED lacks the required sysfs interface")
		}
	}
	return path, nil
}

func validateHost(ctx context.Context, s Spec) error {
	if os.Geteuid() != 0 || os.Getuid() != 0 {
		return fmt.Errorf("LED host installation is an explicit administrator operation")
	}
	if _, err := user.LookupId(strconv.Itoa(s.UID)); err != nil {
		return fmt.Errorf("selected LED client UID is unavailable")
	}
	if err := validateDevices(s, Discover); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-c", "import sys;sys.exit(0 if sys.version_info >= (3,11) else 1)")
	if _, err := process.LimitedOutput(cmd, 5*time.Second, 1024); err != nil {
		return fmt.Errorf("managed LEDs require /usr/bin/python3 version 3.11 or later")
	}
	return nil
}

func validateDevices(s Spec, discover func(string) (string, error)) error {
	if err := s.Validate(); err != nil {
		return err
	}
	for _, device := range []struct{ name, path string }{{s.Device, s.DevicePath}, {s.PowerDevice, s.PowerDevicePath}} {
		if device.name == "" {
			continue
		}
		actual, err := discover(device.name)
		if err != nil {
			return err
		}
		if actual != device.path {
			return fmt.Errorf("LED device moved since the prerequisite plan was reviewed")
		}
	}
	return nil
}

func verifyDaemon(ctx context.Context, s Spec) error {
	// Type=notify start completion means the daemon has loaded its devices.
	p, status, err := led.InspectEndpoint(ctx, Socket(s.UID))
	if err != nil {
		return fmt.Errorf("managed LED daemon not ready: %w", err)
	}
	if p.OwnerUID != s.UID || p.PeerUID != 0 || status.Daemon != "pistatusd" || status.Version != "0.1.0" || status.Degraded {
		return fmt.Errorf("managed LED daemon identity or health differs")
	}
	return validateDaemonDevices(s, status.LEDs)
}

func validateDaemonDevices(s Spec, devices map[string]json.RawMessage) error {
	for name, selected := range map[string]string{"green": s.Device, "red": s.PowerDevice} {
		var actual struct {
			Enabled   bool   `json:"enabled"`
			Available bool   `json:"available"`
			Device    string `json:"device"`
			Default   string `json:"default_pattern"`
		}
		if json.Unmarshal(devices[name], &actual) != nil {
			return fmt.Errorf("managed LED device configuration unavailable")
		}
		if selected == "" {
			if actual.Enabled {
				return fmt.Errorf("power LED enabled without declared opt-in")
			}
		} else if !actual.Enabled || !actual.Available || actual.Device != selected || actual.Default != "off" {
			return fmt.Errorf("managed LED device configuration differs")
		}
	}
	return nil
}

type systemServices struct{}

func (systemServices) command(ctx context.Context, args ...string) ([]byte, error) {
	return process.LimitedOutput(exec.CommandContext(ctx, "/usr/bin/systemctl", append([]string{"--system", "--no-pager"}, args...)...), 45*time.Second, 16<<10)
}
func (systemServices) Inspect(ctx context.Context, s Spec, owned bool) error {
	for _, ext := range []string{".socket", ".service"} {
		name := Unit(s.UID) + ext
		out, err := (systemServices{}).command(ctx, "show", name, "--property=LoadState,ActiveState,FragmentPath,DropInPaths,UnitFileState,Names")
		props := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				return fmt.Errorf("incomplete LED systemd inspection")
			}
			if _, ok = props[key]; ok {
				return fmt.Errorf("duplicate LED systemd property")
			}
			props[key] = value
		}
		if len(props) != 6 || props["LoadState"] == "" || props["ActiveState"] == "" || props["DropInPaths"] != "" {
			return fmt.Errorf("LED unit has unexpected systemd configuration")
		}
		if props["LoadState"] == "not-found" && props["ActiveState"] == "inactive" && props["FragmentPath"] == "" {
			continue
		}
		if err != nil {
			return fmt.Errorf("LED service inspection failed")
		}
		if !owned || props["LoadState"] != "loaded" || props["FragmentPath"] != "/etc/systemd/system/"+name || strings.TrimSpace(props["Names"]) != name {
			return fmt.Errorf("LED system service is not owned by this installation")
		}
		if state := props["UnitFileState"]; state != "enabled" && state != "disabled" {
			return fmt.Errorf("LED service has unexpected enablement")
		}
	}
	return nil
}
func (s systemServices) Reload(ctx context.Context) error {
	_, err := s.command(ctx, "daemon-reload")
	return err
}
func (s systemServices) Start(ctx context.Context, spec Spec) error {
	for _, ext := range []string{".socket", ".service"} {
		if _, err := s.command(ctx, "enable", "--now", Unit(spec.UID)+ext); err != nil {
			return fmt.Errorf("LED host start is incomplete; resume the same approved plan")
		}
	}
	return nil
}

func (s systemServices) Stop(ctx context.Context, spec Spec) error {
	if err := s.Inspect(ctx, spec, true); err != nil {
		return err
	}
	// Stop socket activation first. Removing a unit never recursively removes
	// the shared hardware lock directory or daemon state.
	for _, ext := range []string{".socket", ".service"} {
		if _, err := s.command(ctx, "disable", "--now", Unit(spec.UID)+ext); err != nil {
			return fmt.Errorf("LED host stop is incomplete; resume the reviewed removal")
		}
	}
	return s.VerifyStopped(ctx, spec)
}

func (s systemServices) VerifyStopped(ctx context.Context, spec Spec) error {
	if err := s.Inspect(ctx, spec, true); err != nil {
		return err
	}
	for _, ext := range []string{".socket", ".service"} {
		out, err := s.command(ctx, "show", Unit(spec.UID)+ext, "--property=LoadState,ActiveState,UnitFileState")
		props := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				return fmt.Errorf("incomplete stopped LED service inspection")
			}
			if _, duplicate := props[key]; duplicate {
				return fmt.Errorf("duplicate stopped LED service property")
			}
			props[key] = value
		}
		absent := props["LoadState"] == "not-found"
		if len(props) != 3 || props["ActiveState"] != "inactive" || (!absent && (err != nil || props["LoadState"] != "loaded" || props["UnitFileState"] != "disabled")) || (absent && props["UnitFileState"] != "") {
			return fmt.Errorf("LED service is still active, enabled or unverifiable")
		}
	}
	return nil
}

func (s systemServices) Verify(ctx context.Context, spec Spec) error {
	if err := s.Inspect(ctx, spec, true); err != nil {
		return err
	}
	for _, ext := range []string{".socket", ".service"} {
		out, err := s.command(ctx, "show", Unit(spec.UID)+ext, "--property=LoadState,ActiveState,UnitFileState")
		if err != nil {
			return fmt.Errorf("LED service health unavailable")
		}
		got := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(got) != 3 {
			return fmt.Errorf("incomplete LED service health")
		}
		seen := map[string]bool{}
		for _, line := range got {
			seen[line] = true
		}
		if !seen["LoadState=loaded"] || !seen["ActiveState=active"] || !seen["UnitFileState=enabled"] {
			return fmt.Errorf("LED system service is not active and enabled; inspect the retained host installation")
		}
	}
	return nil
}

type Request struct {
	Spec     Spec   `json:"spec"`
	Artifact string `json:"artifact"`
}

// Apply performs only the reviewed host prerequisite. The ordinary worker
// controller never calls sudo or silently invokes this administrator entry point.
func Apply(ctx context.Context, r Request, approved string) (Receipt, error) {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return Receipt{}, fmt.Errorf("run the reviewed LED host prerequisite as administrator")
	}
	if !filepath.IsAbs(r.Artifact) || filepath.Clean(r.Artifact) != r.Artifact {
		return Receipt{}, fmt.Errorf("LED prerequisite artifact path must be absolute")
	}
	d, err := safefs.Open(filepath.Dir(r.Artifact))
	if err != nil {
		return Receipt{}, err
	}
	defer d.Close()
	artifact, err := safefs.ReadFile(d, filepath.Base(r.Artifact), ledartifact.MaxBytes)
	if err != nil {
		return Receipt{}, err
	}
	m := manager{owner: 0, services: systemServices{}, validateHost: validateHost, verifyDaemon: verifyDaemon}
	return m.install(ctx, r.Spec, approved, artifact)
}
