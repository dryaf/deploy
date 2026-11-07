// Package workerctl coordinates declared worker policy. Domain installation and
// delivery records remain the authority for ownership and production admission.
package workerctl

import (
	"fmt"
	"path"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

type TargetFacts struct {
	OS          string            `json:"os"`
	Arch        string            `json:"arch"`
	UID         int               `json:"uid"`
	User        string            `json:"user"`
	Home        string            `json:"home"`
	ConfigHome  string            `json:"config_home"`
	StateHome   string            `json:"state_home"`
	DataHome    string            `json:"data_home"`
	RuntimeDir  string            `json:"runtime_dir"`
	UserManager bool              `json:"user_manager"`
	Linger      bool              `json:"linger"`
	Tools       map[string]bool   `json:"tools"`
	Board       string            `json:"board,omitempty"`
	LEDPaths    map[string]string `json:"led_paths,omitempty"`
	LEDs        []string          `json:"leds,omitempty"`
}

type ResolvedPaths struct {
	targetUID     int
	backupSetup   string
	Config        string   `json:"config"`
	State         string   `json:"state"`
	Data          string   `json:"data"`
	Runtime       string   `json:"runtime"`
	Installation  string   `json:"installation"`
	UnitDirectory string   `json:"unit_directory"`
	Credentials   string   `json:"credentials"`
	Archive       string   `json:"archive,omitempty"`
	Control       string   `json:"control"`
	Socket        string   `json:"socket,omitempty"`
	DeniedPaths   []string `json:"denied_paths,omitempty"`
}

func ResolveTargetPath(value, home string) (string, error) {
	if !config.WorkerAbsolutePath(home) {
		return "", fmt.Errorf("invalid target home")
	}
	if !config.TargetPath(value) {
		return "", fmt.Errorf("target path must be clean absolute or ~/path")
	}
	if strings.HasPrefix(value, "~/") {
		return path.Join(home, strings.TrimPrefix(value, "~/")), nil
	}
	return value, nil
}

func ResolvePaths(kind string, desired config.WorkerPaths, f TargetFacts) (ResolvedPaths, error) {
	p := ResolvedPaths{targetUID: f.UID}
	if kind != "backup-worker" && kind != "notification-worker" {
		return p, fmt.Errorf("unknown worker kind")
	}
	if f.OS != "linux" || (f.Arch != "amd64" && f.Arch != "arm64") || f.UID <= 0 || !config.UserPattern.MatchString(f.User) || !config.WorkerAbsolutePath(f.Home) {
		return p, fmt.Errorf("workers require a rootless Linux amd64/arm64 target with a trusted home")
	}
	base := []string{f.ConfigHome, f.StateHome, f.DataHome}
	defaults := []string{".config", ".local/state", ".local/share"}
	for i := range base {
		if base[i] == "" {
			base[i] = path.Join(f.Home, defaults[i])
		}
		if !config.WorkerAbsolutePath(base[i]) {
			return p, fmt.Errorf("target XDG directories must be clean absolute non-root paths")
		}
	}
	p.Config = path.Join(base[0], "deploy", kind)
	p.State = path.Join(base[1], "deploy", kind, "state")
	p.Data = path.Join(base[2], "deploy", kind)
	p.UnitDirectory = path.Join(base[0], "systemd/user")
	p.Credentials = path.Join(base[0], "deploy/secrets")
	p.Control = path.Join(base[1], "deploy/worker-control", kind)
	for _, v := range []struct {
		declared string
		resolved *string
	}{{desired.Config, &p.Config}, {desired.State, &p.State}, {desired.Data, &p.Data}, {desired.UnitDirectory, &p.UnitDirectory}} {
		if v.declared != "" {
			resolved, err := ResolveTargetPath(v.declared, f.Home)
			if err != nil {
				return p, err
			}
			*v.resolved = resolved
		}
	}
	p.Installation = path.Join(p.Data, "installation")
	if kind == "backup-worker" {
		p.Installation = path.Join(f.Home, ".local/lib/deploy/backup-worker")
		p.backupSetup = path.Join(f.Home, ".local/state/deploy/backup-worker/setup.json")
	}
	p.Runtime = path.Join(p.Config, "runtime.json")
	for _, v := range []struct {
		declared string
		resolved *string
	}{{desired.Installation, &p.Installation}, {desired.Runtime, &p.Runtime}} {
		if v.declared != "" {
			resolved, err := ResolveTargetPath(v.declared, f.Home)
			if err != nil {
				return p, err
			}
			*v.resolved = resolved
		}
	}
	if kind == "backup-worker" {
		p.Archive = path.Join(p.Data, "archive")
	} else {
		p.Socket = path.Join(p.State, "api.sock")
		if len(p.Socket) >= 108 {
			return p, fmt.Errorf("notification state path exceeds the Unix socket limit; set paths.state")
		}
	}
	// Data is an intentional parent of installation/archive. Operational stores,
	// configuration and unit roots must never overlap or shadow each other.
	roots := []string{p.Config, p.State, p.Installation, p.Control, p.UnitDirectory, p.Credentials}
	for i, a := range roots {
		for _, b := range roots[i+1:] {
			if within(a, b) || within(b, a) {
				return p, fmt.Errorf("configuration, state, installation, control, credentials and unit directories must not overlap")
			}
		}
	}
	if within(p.Runtime, p.Installation) || within(p.Runtime, p.State) || within(p.Runtime, p.UnitDirectory) || within(p.Runtime, p.Control) {
		return p, fmt.Errorf("runtime input overlaps managed state or installed artifacts")
	}
	for _, v := range desired.DeniedPaths {
		resolved, err := ResolveTargetPath(v, f.Home)
		if err != nil {
			return p, err
		}
		p.DeniedPaths = append(p.DeniedPaths, resolved)
	}
	return p, nil
}
func within(child, parent string) bool {
	return child == parent || strings.HasPrefix(child, parent+"/")
}
