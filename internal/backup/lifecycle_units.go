package backup

import (
	"os/exec"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

// Only ordinary host units can be enabled. Quadlet services are generated and
// are started through dependencies of these units.
var workerActivationUnits = []string{"deploy-backup.timer", "deploy-backup-notifier.timer", "deploy-backup-health.timer", "deploy-backup-notifier.path", "deploy-backup-maintenance.timer"}

func enabledWorkerUnits() []string {
	var enabled []string
	for _, unit := range workerActivationUnits {
		out, err := process.LimitedOutput(exec.Command("systemctl", "--user", "is-enabled", unit), 5*time.Second, 1024)
		if err == nil && strings.TrimSpace(string(out)) == "enabled" {
			enabled = append(enabled, unit)
		}
	}
	return enabled
}

func stopWorkerUnits() error {
	names := append([]string{}, workerActivationUnits...)
	names = append(names, "deploy-backup.service", "deploy-backup-notifier.service", "deploy-backup-health.service", "deploy-backup-maintenance.service", "deploy-backup-signal.service")
	for _, unit := range names {
		out, err := process.LimitedOutput(exec.Command("systemctl", "--user", "show", "--property=LoadState", "--value", unit), 5*time.Second, 1024)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) == "not-found" {
			continue
		}
		if _, err = process.LimitedOutput(exec.Command("systemctl", "--user", "stop", unit), 2*time.Minute, 4096); err != nil {
			return err
		}
	}
	return nil
}
