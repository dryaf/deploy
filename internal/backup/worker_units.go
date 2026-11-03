package backup

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/quadlet"
)

type WorkerUnitPaths struct {
	Home        string
	Config      string
	State       string
	Archive     string
	Keys        string
	RelayState  string
	RelaySocket string
}

func unitQuote(s string) string {
	return strconv.Quote(strings.ReplaceAll(strings.ReplaceAll(s, "%", "%%"), "$", "$$"))
}

func renderWorkerUnits(c WorkerInstallation, paths WorkerUnitPaths, image, relayImage string) (map[string]string, error) {
	if err := config.PinnedWorkerImage(image); err != nil {
		return nil, err
	}
	if c.Policy.Notifications.Signal != nil {
		if err := config.PinnedWorkerImage(relayImage); err != nil {
			return nil, fmt.Errorf("Signal relay: %w", err)
		}
	}
	units := map[string]string{}
	container := func(name, role, command string, mounts []string, memory string, timeout time.Duration, requiresArchive bool) error {
		marker, err := quadlet.EncodeMetadata(quadlet.QuadletMetadata{Schema: 1, ManagedBy: "deploy", Role: role, Service: name})
		if err != nil {
			return err
		}
		var b strings.Builder
		b.WriteString(marker + "[Unit]\nDescription=deploy " + name + "\nWants=network-online.target\nAfter=network-online.target\n")
		if c.Policy.Notifications.Signal != nil && (name == "deploy-backup-notifier" || strings.HasPrefix(name, "deploy-backup-test-")) {
			b.WriteString("Wants=deploy-backup-signal.service\nAfter=deploy-backup-signal.service\n")
		}
		if requiresArchive {
			b.WriteString("RequiresMountsFor=" + unitQuote(c.Policy.Storage.RequireMount) + "\nConditionPathIsMountPoint=" + unitQuote(c.Policy.Storage.RequireMount) + "\n")
		}
		b.WriteString("\n[Container]\nImage=" + image + "\nPull=never\nReadOnly=true\nNoNewPrivileges=true\nDropCapability=ALL\nPodmanArgs=--security-opt=seccomp=unconfined\nTmpfs=/tmp:rw,size=128m,nosuid,nodev\nLabel=io.deploy.managed-by=deploy\nLabel=io.deploy.role=" + role + "\nLabel=io.deploy.service=" + name + "\n")
		for _, mount := range mounts {
			b.WriteString("Volume=" + strconv.Quote(strings.ReplaceAll(mount, "%", "%%")) + "\n")
		}
		b.WriteString("Exec=" + command + "\n\n[Service]\nType=oneshot\nRemainAfterExit=no\nTimeoutStartSec=" + strconv.Itoa(int(timeout.Seconds())) + "\nTimeoutStopSec=20\nKillMode=control-group\nMemoryMax=" + memory + "\nTasksMax=512\nCPUQuota=100%\nUMask=0077\n")
		units[name+".container"] = b.String()
		return nil
	}
	configMount := filepath.Join(filepath.Dir(paths.Config), "container.json") + ":/config/runtime.json:ro"
	stateMount := paths.State + ":/state:rw"
	common := " --local --config /config/runtime.json --state-dir /state"
	if err := container("deploy-backup", "backup-worker", "backup-worker __run"+common+" --archive /archive", []string{configMount, stateMount, paths.Archive + ":/archive:rw", paths.Keys + ":/keys:ro"}, "768M", config.Budget(c.Policy.MaxRunTime, "40m")+time.Minute, true); err != nil {
		return nil, err
	}
	notifierMounts := []string{configMount, stateMount}
	p := c.Policy
	if p.Notifications.Signal != nil {
		notifierMounts = append(notifierMounts, paths.RelaySocket+":/relay:rw")
	}
	for host, dest := range notificationMounts(p) {
		notifierMounts = append(notifierMounts, host+":"+dest+":ro")
	}
	if err := container("deploy-backup-notifier", "backup-worker", "backup-worker __notify"+common, notifierMounts, "256M", 55*time.Second, false); err != nil {
		return nil, err
	}
	for _, provider := range []string{p.Notifications.Primary, p.Notifications.Fallback} {
		if provider == "" {
			continue
		}
		if err := container("deploy-backup-test-"+provider, "backup-worker", "backup-worker __notify_test"+common+" --channel "+provider, notifierMounts, "256M", 30*time.Second, false); err != nil {
			return nil, err
		}
	}
	if p.Notifications.Signal != nil {
		marker, _ := quadlet.EncodeMetadata(quadlet.QuadletMetadata{Schema: 1, ManagedBy: "deploy", Role: "notification-relay", Service: "deploy-backup-signal"})
		units["deploy-backup-signal.container"] = marker + "[Unit]\nDescription=deploy Signal relay\nWants=network-online.target\nAfter=network-online.target\n\n[Container]\nImage=" + relayImage + "\nPull=never\nReadOnly=true\nNoNewPrivileges=true\nDropCapability=ALL\nVolume=" + strconv.Quote(paths.RelayState+":/signal-state:rw") + "\nVolume=" + strconv.Quote(paths.RelaySocket+":/relay:rw") + "\nVolume=" + strconv.Quote(p.Notifications.Signal.AccountFile+":/secrets/signal-account:ro") + "\nVolume=" + strconv.Quote(p.Notifications.Signal.RecipientFile+":/secrets/signal-recipient:ro") + "\nExec=backup-worker __signal_relay\nTmpfs=/tmp:rw,size=256m,nosuid,nodev\nLabel=io.deploy.managed-by=deploy\nLabel=io.deploy.role=notification-relay\nLabel=io.deploy.service=deploy-backup-signal\n\n[Service]\nRestart=on-failure\nRestartSec=10s\nMemoryMax=768M\nTasksMax=256\nCPUQuota=50%\nTimeoutStartSec=120\nTimeoutStopSec=20\n"
	}
	calendar := map[string]string{"hourly": "*-*-* *:00:00 UTC", "daily": "*-*-* 00:00:00 " + p.Timezone, "weekly": "Mon *-*-* 00:00:00 " + p.Timezone}[p.Schedule]
	units["deploy-backup.timer"] = "[Unit]\nDescription=Scheduled deploy snapshots\n\n[Timer]\nOnCalendar=" + calendar + "\nRandomizedDelaySec=60\nPersistent=true\nUnit=deploy-backup.service\n\n[Install]\nWantedBy=timers.target\n"
	for _, name := range []string{"deploy-backup-notifier", "deploy-backup-health"} {
		units[name+".timer"] = "[Unit]\nDescription=Independent deploy backup supervision\n\n[Timer]\nOnCalendar=*-*-* *:*:00 UTC\nAccuracySec=5s\nPersistent=true\nUnit=" + name + ".service\n\n[Install]\nWantedBy=timers.target\n"
	}
	helper := filepath.Join(paths.Home, ".local/lib/deploy/backup-worker/deploy")
	// Allow the bounded manager probe, scheduler contention, and catch-up start
	// to finish while keeping observations inside the 120-second freshness limit.
	units["deploy-backup-health.service"] = "[Unit]\nDescription=Independent deploy backup host probe\n\n[Service]\nType=oneshot\nExecStart=" + unitQuote(helper) + " backup-worker __health --local --config " + unitQuote(paths.Config) + " --state-dir " + unitQuote(paths.State) + " --archive " + unitQuote(paths.Archive) + "\nTimeoutStartSec=75\nTimeoutStopSec=5\nMemoryMax=128M\nTasksMax=64\nNoNewPrivileges=true\nUMask=0077\n"
	units["deploy-backup-maintenance.timer"] = "[Unit]\nDescription=Bounded backup integrity and restore maintenance\n\n[Timer]\nOnCalendar=*-*-* *:00/5:00 UTC\nPersistent=true\nUnit=deploy-backup-maintenance.service\n\n[Install]\nWantedBy=timers.target\n"
	units["deploy-backup-maintenance.service"] = "[Unit]\nDescription=Isolated backup verification and retention\n\n[Service]\nType=oneshot\nExecStart=" + unitQuote(helper) + " backup-worker __maintenance --local --config " + unitQuote(paths.Config) + " --state-dir " + unitQuote(paths.State) + " --archive " + unitQuote(paths.Archive) + "\nTimeoutStartSec=25min\nTimeoutStopSec=10s\nKillMode=control-group\nMemoryMax=768M\nCPUQuota=50%\nTasksMax=256\nUMask=0077\n"
	units["deploy-backup-notifier.path"] = "[Unit]\nDescription=Wake backup notifier for committed urgent events\n\n[Path]\nPathChanged=" + strings.ReplaceAll(filepath.Join(paths.State, "urgent"), "%", "%%") + "\nUnit=deploy-backup-notifier.service\n\n[Install]\nWantedBy=default.target\n"
	return units, nil
}

func notificationMounts(p config.BackupWorkerConfig) map[string]string {
	result := map[string]string{}
	if p.Notifications.Signal != nil {
		result[p.Notifications.Signal.AccountFile] = "/secrets/signal-account"
		result[p.Notifications.Signal.RecipientFile] = "/secrets/signal-recipient"
	}
	if p.Notifications.Telegram != nil {
		result[p.Notifications.Telegram.TokenFile] = "/secrets/telegram-token"
		result[p.Notifications.Telegram.ChatIDFile] = "/secrets/telegram-chat-id"
	}
	if p.Monitoring.BackupPingURLFile != "" {
		result[p.Monitoring.BackupPingURLFile] = "/secrets/backup-ping"
	}
	if p.Monitoring.DailyDeliveryPingURLFile != "" {
		result[p.Monitoring.DailyDeliveryPingURLFile] = "/secrets/delivery-ping"
	}
	return result
}

func containerNotificationPolicy(p config.BackupWorkerConfig) config.BackupWorkerConfig {
	// Copy provider values so generating a container config cannot rewrite the
	// installation's host paths or another component's mount plan.
	if p.Notifications.Signal != nil {
		v := *p.Notifications.Signal
		v.AccountFile = "/secrets/signal-account"
		v.RecipientFile = "/secrets/signal-recipient"
		p.Notifications.Signal = &v
	}
	if p.Notifications.Telegram != nil {
		v := *p.Notifications.Telegram
		v.TokenFile = "/secrets/telegram-token"
		v.ChatIDFile = "/secrets/telegram-chat-id"
		p.Notifications.Telegram = &v
	}
	if p.Monitoring.BackupPingURLFile != "" {
		p.Monitoring.BackupPingURLFile = "/secrets/backup-ping"
	}
	if p.Monitoring.DailyDeliveryPingURLFile != "" {
		p.Monitoring.DailyDeliveryPingURLFile = "/secrets/delivery-ping"
	}
	return p
}
