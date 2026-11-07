package backup

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

// InstallationPaths pins resolved target paths. The setup journal and its lease
// stay at their historical home location so losing or changing an XDG environment
// cannot make an existing installation look absent. These are generated execution
// inputs, not another source of desired policy.
type InstallationPaths struct {
	Version          int      `json:"version"`
	Home             string   `json:"home"`
	ConfigHome       string   `json:"config_home"`
	StateHome        string   `json:"state_home"`
	DataHome         string   `json:"data_home"`
	Config           string   `json:"config"`
	Runtime          string   `json:"runtime"`
	State            string   `json:"state"`
	Data             string   `json:"data"`
	Installation     string   `json:"installation"`
	UnitDirectory    string   `json:"unit_directory"`
	QuadletDirectory string   `json:"quadlet_directory"`
	DeniedPaths      []string `json:"denied_paths,omitempty"`
}

func pathInside(child, parent string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

// ResolveInstallationPaths resolves home/XDG on the target, never the operator.
// It performs no I/O and grants no authority to relocate an existing worker.
func ResolveInstallationPaths(home, configHome, stateHome, dataHome string, desired config.WorkerPaths) (InstallationPaths, error) {
	p := InstallationPaths{Version: 1, Home: home, ConfigHome: configHome, StateHome: stateHome, DataHome: dataHome}
	if !config.WorkerAbsolutePath(home) {
		return p, fmt.Errorf("invalid backup target home")
	}
	for _, item := range []struct {
		value    *string
		fallback string
	}{{&p.ConfigHome, ".config"}, {&p.StateHome, ".local/state"}, {&p.DataHome, ".local/share"}} {
		if *item.value == "" {
			*item.value = filepath.Join(home, item.fallback)
		}
		if !config.WorkerAbsolutePath(*item.value) {
			return p, fmt.Errorf("backup XDG roots must be clean absolute paths")
		}
	}
	p.Config = filepath.Join(p.ConfigHome, "deploy/backup-worker")
	p.State = filepath.Join(p.StateHome, "deploy/backup-worker/state")
	p.Data = filepath.Join(p.DataHome, "deploy/backup-worker")
	p.Installation = filepath.Join(home, ".local/lib/deploy/backup-worker")
	p.UnitDirectory = filepath.Join(p.ConfigHome, "systemd/user")
	p.QuadletDirectory = filepath.Join(p.ConfigHome, "containers/systemd")
	resolve := func(value string) (string, error) {
		if !config.TargetPath(value) {
			return "", fmt.Errorf("backup paths must be clean absolute paths or ~/path")
		}
		if strings.HasPrefix(value, "~/") {
			return filepath.Join(home, strings.TrimPrefix(value, "~/")), nil
		}
		return value, nil
	}
	for _, item := range []struct {
		value    *string
		override string
	}{{&p.Config, desired.Config}, {&p.State, desired.State}, {&p.Data, desired.Data}, {&p.Installation, desired.Installation}, {&p.UnitDirectory, desired.UnitDirectory}} {
		if item.override == "" {
			continue
		}
		value, err := resolve(item.override)
		if err != nil {
			return p, err
		}
		*item.value = value
	}
	p.Runtime = filepath.Join(p.Config, "runtime.json")
	if desired.Runtime != "" {
		var err error
		p.Runtime, err = resolve(desired.Runtime)
		if err != nil {
			return p, err
		}
	}
	for _, path := range desired.DeniedPaths {
		value, err := resolve(path)
		if err != nil {
			return p, err
		}
		p.DeniedPaths = append(p.DeniedPaths, value)
	}
	slices.Sort(p.DeniedPaths)
	p.DeniedPaths = slices.Compact(p.DeniedPaths)
	return p, p.Validate()
}

func (p InstallationPaths) Validate() error {
	if p.Version != 1 {
		return fmt.Errorf("unsupported backup path layout")
	}
	for _, path := range []string{p.Home, p.ConfigHome, p.StateHome, p.DataHome, p.Config, p.Runtime, p.State, p.Data, p.Installation, p.UnitDirectory, p.QuadletDirectory} {
		if !config.WorkerAbsolutePath(path) || strings.ContainsAny(path, "$`\\\x00\r\n") {
			return fmt.Errorf("invalid resolved backup path")
		}
	}
	if p.QuadletDirectory != filepath.Join(p.ConfigHome, "containers/systemd") {
		return fmt.Errorf("backup Quadlet directory differs from target config root")
	}
	// The fixed coordinator root may contain the default state and relay socket,
	// but no runtime, key, artifact or unit publication may shadow its authority.
	anchor := filepath.Join(p.Home, ".local/state/deploy/backup-worker")
	for _, path := range []string{p.Config, p.Runtime, p.Installation, p.UnitDirectory, p.QuadletDirectory, p.Data} {
		if pathInside(path, anchor) || pathInside(anchor, path) {
			return fmt.Errorf("backup path overlaps coordinator authority")
		}
	}
	if pathInside(anchor, p.State) || pathInside(p.State, anchor) && p.State != filepath.Join(anchor, "state") {
		return fmt.Errorf("backup state overlaps coordinator records")
	}
	roots := []string{p.Config, p.State, p.Installation, p.UnitDirectory, p.QuadletDirectory}
	for i, a := range roots {
		if a == p.Home || pathInside(p.Home, a) {
			return fmt.Errorf("backup operational directory contains target home")
		}
		for _, b := range roots[i+1:] {
			if pathInside(a, b) || pathInside(b, a) {
				return fmt.Errorf("backup operational directories overlap")
			}
		}
	}
	for _, root := range []string{p.State, p.Installation, p.UnitDirectory, p.QuadletDirectory, p.Keys(), p.RelayState(), p.RelaySocket()} {
		if pathInside(p.Runtime, root) || pathInside(root, p.Runtime) {
			return fmt.Errorf("backup runtime overlaps operational data")
		}
	}
	if p.Runtime == p.Config || p.Runtime == p.ContainerRuntime() || p.Runtime == filepath.Join(p.Config, "images.json") {
		return fmt.Errorf("backup runtime shadows generated configuration")
	}
	for _, root := range []string{p.Config, p.State, p.UnitDirectory, p.QuadletDirectory} {
		if pathInside(p.Data, root) || pathInside(root, p.Data) {
			return fmt.Errorf("backup data overlaps operational directory")
		}
	}
	for _, denied := range p.DeniedPaths {
		if !config.WorkerAbsolutePath(denied) {
			return fmt.Errorf("invalid backup exclusion")
		}
		for _, required := range []string{p.Config, p.Runtime, p.State, p.Installation, p.Keys(), p.RelayState(), p.RelaySocket()} {
			if pathInside(required, denied) || pathInside(denied, required) {
				return fmt.Errorf("backup exclusion overlaps required runtime path")
			}
		}
	}
	if !slices.IsSorted(p.DeniedPaths) || len(slices.Compact(slices.Clone(p.DeniedPaths))) != len(p.DeniedPaths) {
		return fmt.Errorf("backup exclusions are not canonical")
	}
	return nil
}

func (p InstallationPaths) Keys() string   { return filepath.Join(p.Config, "ssh") }
func (p InstallationPaths) Helper() string { return filepath.Join(p.Installation, "deploy") }
func (p InstallationPaths) ContainerRuntime() string {
	return filepath.Join(p.Config, "container.json")
}
func (p InstallationPaths) RelayState() string  { return filepath.Join(p.Data, "signal") }
func (p InstallationPaths) RelaySocket() string { return filepath.Join(filepath.Dir(p.State), "relay") }

// Owned-file keys remain relative for the old home layout. Explicit external
// paths remain absolute and must use descriptor-safe parent access at publication.
func (p InstallationPaths) OwnedPath(path string) string {
	if pathInside(path, p.Home) {
		relative, _ := filepath.Rel(p.Home, path)
		return relative
	}
	return path
}

func (p InstallationPaths) MatchesDesired(desired config.WorkerPaths) error {
	want, err := ResolveInstallationPaths(p.Home, p.ConfigHome, p.StateHome, p.DataHome, desired)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(p, want) {
		return fmt.Errorf("backup resolved paths differ from desired declaration")
	}
	return nil
}

func (p InstallationPaths) UnitPaths(archive string) (WorkerUnitPaths, error) {
	if err := p.Validate(); err != nil {
		return WorkerUnitPaths{}, err
	}
	if !config.WorkerAbsolutePath(archive) {
		return WorkerUnitPaths{}, fmt.Errorf("backup archive must be a clean absolute path")
	}
	for _, root := range []string{p.Config, p.Runtime, p.State, p.Installation, p.UnitDirectory, p.QuadletDirectory, p.Keys(), p.RelayState(), p.RelaySocket(), filepath.Join(p.Home, ".local/state/deploy/backup-worker")} {
		if pathInside(archive, root) || pathInside(root, archive) {
			return WorkerUnitPaths{}, fmt.Errorf("backup archive overlaps operational data")
		}
	}
	paths := p.unitPaths(archive)
	if _, err := paths.nativeExclusions(); err != nil {
		return WorkerUnitPaths{}, err
	}
	return paths, nil
}

func (p InstallationPaths) unitPaths(archive string) WorkerUnitPaths {
	return WorkerUnitPaths{Home: p.Home, Config: p.Runtime, ContainerConfig: p.ContainerRuntime(), State: p.State, Archive: archive, Keys: p.Keys(), RelayState: p.RelayState(), RelaySocket: p.RelaySocket(), Helper: p.Helper(), UnitDirectory: p.UnitDirectory, QuadletDirectory: p.QuadletDirectory, DeniedPaths: slices.Clone(p.DeniedPaths)}
}
