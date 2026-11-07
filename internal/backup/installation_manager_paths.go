package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dryaf/deploy/internal/safefs"
)

// The SSH environment is not the user manager's environment. Check the actual
// manager before writing units or arming the rollback supervisor. A directory
// override must already be in that manager's search path; setup never rewrites
// a user's manager environment or creates untracked unit symlinks.
func requireBackupManagerLayout(ctx context.Context, p InstallationPaths, run func(context.Context, string, ...string) ([]byte, error)) error {
	if err := p.Validate(); err != nil {
		return err
	}
	read := func(property string) ([]string, error) {
		raw, err := run(ctx, "busctl", "--user", "--no-pager", "--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", property)
		if err != nil {
			return nil, fmt.Errorf("backup path preflight requires busctl and an accessible user manager (%s)", property)
		}
		var value struct {
			Type string   `json:"type"`
			Data []string `json:"data"`
		}
		if err := safefs.DecodeJSON(raw, &value, 64<<10); err != nil || value.Type != "as" || value.Data == nil {
			return nil, fmt.Errorf("invalid user manager path response (%s)", property)
		}
		return value.Data, nil
	}
	paths, err := read("UnitPath")
	if err != nil {
		return err
	}
	if !slices.Contains(paths, p.UnitDirectory) {
		return fmt.Errorf("backup paths.unit_directory is outside the target user manager search path; select a directory already configured for that manager")
	}
	environment, err := read("Environment")
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return fmt.Errorf("invalid user manager environment response")
		}
		if _, duplicate := values[name]; duplicate {
			return fmt.Errorf("ambiguous user manager environment")
		}
		values[name] = value
	}
	configHome := values["XDG_CONFIG_HOME"]
	if configHome == "" {
		configHome = filepath.Join(p.Home, ".config")
	}
	if configHome != p.ConfigHome {
		return fmt.Errorf("backup target XDG_CONFIG_HOME differs from its user manager; align the SSH and manager configuration before setup")
	}
	if dirs := values["QUADLET_UNIT_DIRS"]; dirs != "" && !slices.Contains(strings.Split(dirs, ":"), p.QuadletDirectory) {
		return fmt.Errorf("backup Quadlet directory is outside the manager's configured generator search path")
	}
	return nil
}
