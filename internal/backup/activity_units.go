package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

// Ownership is checked on the loaded unit, not inferred from a familiar name.
// Generated services must name the exact retained Quadlet source, while ordinary
// units must use the retained user-unit file. Drop-ins and aliases require review.
func backupActivityUnitStopped(ctx context.Context, home string, unit backupActivityUnit) (bool, error) {
	cmd := exec.Command("systemctl", "--user", "show", unit.Name, "--property=LoadState,ActiveState,UnitFileState,FragmentPath,SourcePath,DropInPaths,Names")
	var out, errs process.Buffer
	out.Limit, errs.Limit = 8192, 8192
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := process.RunContext(ctx, cmd, 10*time.Second, time.Second); err != nil {
		return false, fmt.Errorf("backup activity unit inspection unavailable")
	}
	values := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; exists {
			return false, fmt.Errorf("ambiguous backup activity unit state")
		}
		values[key] = value
	}
	if len(values) != 7 || values["LoadState"] != "loaded" || values["DropInPaths"] != "" || values["Names"] != unit.Name {
		return false, fmt.Errorf("backup activity unit is missing or has foreign aliases/drop-ins")
	}
	source := unit.Source
	if !filepath.IsAbs(source) {
		source = filepath.Join(home, source)
	}
	if unit.Generated {
		parent := filepath.Dir(values["FragmentPath"])
		base := filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "systemd")
		if filepath.Base(values["FragmentPath"]) != unit.Name || (parent != filepath.Join(base, "generator") && parent != filepath.Join(base, "generator.early") && parent != filepath.Join(base, "generator.late")) || values["SourcePath"] != source || values["UnitFileState"] != "generated" {
			return false, fmt.Errorf("backup activity generated unit has foreign ownership")
		}
	} else {
		if values["FragmentPath"] != source || values["SourcePath"] != "" {
			return false, fmt.Errorf("backup activity unit fragment changed")
		}
		switch values["UnitFileState"] {
		case "enabled", "disabled", "static":
		default:
			return false, fmt.Errorf("backup activity unit enablement needs review")
		}
		if unit.Activation && values["UnitFileState"] == "static" || !unit.Activation && values["UnitFileState"] == "enabled" {
			return false, fmt.Errorf("backup activity unit has unexpected activation ownership")
		}
	}
	if values["ActiveState"] == "" {
		return false, fmt.Errorf("backup activity unit state missing")
	}
	inactive := values["ActiveState"] == "inactive" || values["ActiveState"] == "failed"
	if unit.Activation {
		inactive = inactive && values["UnitFileState"] == "disabled"
	}
	return inactive, nil
}

func inspectBackupActivityUnits(ctx context.Context, home string, units []backupActivityUnit) (bool, error) {
	all := true
	for _, unit := range units {
		stopped, err := backupActivityUnitStopped(ctx, home, unit)
		if err != nil {
			return false, err
		}
		all = all && stopped
	}
	return all, nil
}

func stopBackupActivityUnits(ctx context.Context, home string, units []backupActivityUnit) error {
	for _, unit := range units {
		stopped, err := backupActivityUnitStopped(ctx, home, unit)
		if err != nil {
			return err
		}
		if stopped {
			continue
		}
		args := []string{"--user", "stop", unit.Name}
		if unit.Activation {
			args = []string{"--user", "disable", "--now", unit.Name}
		}
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout, cmd.Stderr = &process.Buffer{Limit: 8192}, &process.Buffer{Limit: 8192}
		if err = process.RunContext(ctx, cmd, 2*time.Minute, 5*time.Second); err != nil {
			return fmt.Errorf("backup stop outcome unconfirmed; resume the retained YAML activity")
		}
	}
	return nil
}
