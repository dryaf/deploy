package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

type WorkerHealthProbe struct {
	Schema          int                          `json:"schema"`
	ObservedAt      time.Time                    `json:"observed_at"`
	BootID          string                       `json:"boot_id"`
	Uptime          float64                      `json:"uptime"`
	ConfigHash      string                       `json:"config_hash"`
	BackupHealthy   bool                         `json:"backup_healthy"`
	DeliveryHealthy bool                         `json:"delivery_healthy"`
	Healthy         bool                         `json:"healthy"`
	Diagnostics     []string                     `json:"diagnostics"`
	Units           map[string]map[string]string `json:"units"`
}

func (p WorkerHealthProbe) Fresh(now time.Time) bool {
	boot, uptime, err := process.HostClock()
	if err != nil || p.Schema != 1 || p.BootID != boot || uptime < p.Uptime || uptime-p.Uptime > 120 {
		return false
	}
	elapsed := now.Sub(p.ObservedAt).Seconds()
	return elapsed >= 0 && elapsed < 120 && absFloat(elapsed-(uptime-p.Uptime)) < 30
}

func absFloat(n float64) float64 {
	if n < 0 {
		return -n
	}
	return n
}

func collectWorkerHealth(c WorkerInstallation, archivePath string, state *backupState) WorkerHealthProbe {
	p := WorkerHealthProbe{Schema: 1, ObservedAt: time.Now().UTC(), ConfigHash: c.ConfigHash, Healthy: true, BackupHealthy: true, DeliveryHealthy: true, Diagnostics: []string{}, Units: map[string]map[string]string{}}
	delivery := false
	fail := func(message string) {
		p.Healthy = false
		if delivery {
			p.DeliveryHealthy = false
		} else {
			p.BackupHealthy = false
		}
		p.Diagnostics = append(p.Diagnostics, message)
	}
	var err error
	p.BootID, p.Uptime, err = process.HostClock()
	if err != nil {
		fail("host monotonic clock unavailable")
	}
	mountInfo, mountErr := os.ReadFile("/proc/self/mountinfo")
	mounted := false
	if mountErr == nil {
		for _, line := range strings.Split(string(mountInfo), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 6 && fields[4] == c.Policy.Storage.RequireMount {
				mounted = true
				break
			}
		}
	}
	if !mounted {
		fail("required backup mount is absent")
	}
	a, err := openBackupArchive(archivePath)
	if err != nil {
		fail("archive identity/storage unavailable")
	} else {
		filesystem, fsErr := a.dir.FilesystemID()
		if fsErr != nil || filesystem != a.identity.FilesystemID {
			fail("archive filesystem differs from enrolled writer storage")
		}
		if a.identity.ID != c.ArchiveID {
			fail("archive identity changed")
		}
		if err = a.dir.Capacity(c.Policy.Storage.MinFreeBytes); err != nil {
			fail("archive capacity below configured reserve")
		}
		if err = a.syncVerificationStatuses(state); err != nil {
			fail("verification history unavailable")
		}
		if err = a.refreshLatestAvailability(state); err != nil {
			fail("latest snapshot availability cannot be established")
		}
		a.Close()
	}
	// Bound the whole manager probe as well as each command. Adding monitored
	// units must not allow a wedged manager to exhaust the host helper's budget.
	managerContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	deliveryUnits := []string{"deploy-backup-notifier.timer", "deploy-backup-notifier.service"}
	var owner backupNotificationOwner
	shared := false
	ownerErr := backupTransferTx(context.Background(), state, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key='notification_owner')").Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return nil
		}
		var err error
		owner, err = readBackupNotificationOwner(tx)
		if err != nil {
			return err
		}
		if owner.Mode == "legacy" {
			deliveryUnits = []string{"deploy-backup-legacy.timer", "deploy-backup-legacy.service"}
			return nil
		}
		if owner.Mode != "shared" {
			return fmt.Errorf("notification ownership handoff is pending")
		}
		shared = true
		return nil
	})
	if ownerErr == nil && owner.Mode == "legacy" {
		ownerErr = requireLegacyDeliveryOwner(state)
	}
	if ownerErr != nil {
		delivery = true
		fail("notification ownership is unavailable or pending")
	}
	if shared {
		deliveryUnits = []string{"deploy-backup-report.timer", "deploy-backup-report.service"}
	}
	units := append([]string{"deploy-backup.timer", "deploy-backup-health.timer", "deploy-backup-maintenance.timer", "deploy-backup.service", "deploy-backup-maintenance.service"}, deliveryUnits...)
	for _, unit := range units {
		delivery = strings.Contains(unit, "notifier") || strings.Contains(unit, "report")
		if managerContext.Err() != nil {
			fail("user manager cannot inspect " + unit)
			continue
		}
		cmd := exec.Command("systemctl", "--user", "show", "--no-pager", "--property=LoadState,ActiveState,SubState,Result,UnitFileState,NextElapseUSecRealtime,ExecMainStatus", unit)
		var output, errors process.Buffer
		output.Limit = 16 << 10
		errors.Limit = 4096
		cmd.Stdout = &output
		cmd.Stderr = &errors
		if err = process.RunContext(managerContext, cmd, 5*time.Second, time.Second); err != nil {
			fail("user manager cannot inspect " + unit)
			continue
		}
		props := map[string]string{}
		for _, line := range strings.Split(output.String(), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				props[key] = value
			}
		}
		p.Units[unit] = props
		if props["LoadState"] != "loaded" {
			fail(unit + " is not loaded")
		}
		if strings.HasSuffix(unit, ".timer") && (props["ActiveState"] != "active" || props["UnitFileState"] != "enabled" || props["NextElapseUSecRealtime"] == "" || props["NextElapseUSecRealtime"] == "n/a") {
			fail(unit + " is not armed")
		}
		if props["ActiveState"] == "failed" || (props["Result"] != "" && props["Result"] != "success") {
			fail(unit + " failed")
		}
	}
	if shared {
		delivery = true
		var report backupReportHealth
		if err = state.get("shared_report_health", &report); err != nil || report.Binding != owner.Binding || !report.Available || p.ObservedAt.Before(report.At) || p.ObservedAt.Sub(report.At) >= 120*time.Second {
			fail("shared notification report/API evidence is unavailable or stale")
		}
	}
	return p
}
