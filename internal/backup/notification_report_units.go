package backup

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

// Path lists do not expand environment variables like ExecStart arguments do.
func reportUnitPathQuote(path string) string {
	return strconv.Quote(strings.ReplaceAll(path, "%", "%%"))
}

// Rendering never installs or enables units. The ownership handoff will select
// these names only after fencing the legacy sender and reconciling its evidence.
func renderBackupReportUnits(paths WorkerUnitPaths, policy config.BackupWorkerConfig, extraCredentials []string) (map[string]string, error) {
	for _, path := range []string{paths.Home, paths.Config, paths.State} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return nil, fmt.Errorf("report unit paths must be clean absolute paths")
		}
	}
	helper := filepath.Join(paths.Home, ".local/lib/deploy/backup-worker/deploy")
	denied := append([]string{}, extraCredentials...)
	policyWithoutMonitors := policy
	policyWithoutMonitors.Monitoring = config.WorkerMonitoring{}
	for path := range notificationMounts(policyWithoutMonitors) {
		denied = append(denied, path)
	}
	for _, path := range []string{paths.Keys, paths.RelayState} {
		if path != "" {
			denied = append(denied, path)
		}
	}
	slices.Sort(denied)
	denied = slices.Compact(denied)
	var restrictions strings.Builder
	for _, path := range denied {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return nil, fmt.Errorf("invalid report credential exclusion")
		}
		for _, required := range []string{helper, paths.Config, paths.State, policy.Monitoring.BackupPingURLFile, policy.Monitoring.DailyDeliveryPingURLFile} {
			if required != "" && (path == "/" || required == path || strings.HasPrefix(required, path+"/")) {
				return nil, fmt.Errorf("report credential exclusion overlaps a required path")
			}
		}
		restrictions.WriteString("InaccessiblePaths=" + reportUnitPathQuote("-"+path) + "\n")
	}
	return map[string]string{
		"deploy-backup-report.service": "[Unit]\nDescription=Evaluate and export backup reports\n\n[Service]\nType=oneshot\nExecStart=" + unitQuote(helper) + " backup-worker __report --local --config " + unitQuote(paths.Config) + " --state-dir " + unitQuote(paths.State) + "\nTimeoutStartSec=70\nTimeoutStopSec=5\nKillMode=control-group\nMemoryMax=256M\nTasksMax=64\nCPUQuota=100%\nNoNewPrivileges=true\nProtectSystem=strict\nProtectHome=read-only\nReadWritePaths=" + reportUnitPathQuote(paths.State) + "\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\nUMask=0077\n" + restrictions.String(),
		"deploy-backup-report.timer":   "[Unit]\nDescription=Periodic backup report evaluation\n\n[Timer]\nOnCalendar=*-*-* *:*:00,30 UTC\nAccuracySec=1s\nPersistent=true\nUnit=deploy-backup-report.service\n\n[Install]\nWantedBy=timers.target\n",
		"deploy-backup-report.path":    "[Unit]\nDescription=Wake backup report exporter for committed events\n\n[Path]\nPathChanged=" + strings.ReplaceAll(filepath.Join(paths.State, "urgent"), "%", "%%") + "\nUnit=deploy-backup-report.service\n\n[Install]\nWantedBy=default.target\n",
	}, nil
}
