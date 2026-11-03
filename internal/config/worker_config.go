package config

import (
	"fmt"
	"path"
	"reflect"
	"strings"
	"time"
	_ "time/tzdata"
)

// Host paths stay on the worker. Only operator SSH paths are expanded locally.
type BackupWorkerConfig struct {
	RestoreChecks    []WorkerRestoreCheck `yaml:"restore_checks" json:"restore_checks"`
	Sources          []string             `yaml:"sources" json:"sources"`
	Storage          WorkerStorage        `yaml:"storage" json:"storage"`
	Schedule         string               `yaml:"schedule" json:"schedule"`
	Timezone         string               `yaml:"timezone" json:"timezone"`
	Freshness        string               `yaml:"freshness" json:"freshness"`
	MaxRunTime       string               `yaml:"max_run_time" json:"max_run_time"`
	MaxSnapshotBytes int64                `yaml:"max_snapshot_bytes" json:"max_snapshot_bytes"`
	Timeouts         WorkerTimeouts       `yaml:"timeouts" json:"timeouts"`
	Retention        *RetentionConfig     `yaml:"retention" json:"retention"`
	Notifications    WorkerNotifications  `yaml:"notifications" json:"notifications"`
	Monitoring       WorkerMonitoring     `yaml:"monitoring" json:"monitoring"`
}

type WorkerStorage struct {
	Path         string `yaml:"path" json:"path"`
	RequireMount string `yaml:"require_mount" json:"require_mount"`
	MinFreeBytes int64  `yaml:"min_free_bytes" json:"min_free_bytes"`
}

type WorkerTimeouts struct {
	Connect       string `yaml:"connect" json:"connect"`
	Inventory     string `yaml:"inventory" json:"inventory"`
	Snapshot      string `yaml:"snapshot" json:"snapshot"`
	Transfer      string `yaml:"transfer" json:"transfer"`
	Verify        string `yaml:"verify" json:"verify"`
	ShutdownGrace string `yaml:"shutdown_grace" json:"shutdown_grace"`
}

type WorkerNotifications struct {
	Primary     string          `yaml:"primary" json:"primary"`
	Fallback    string          `yaml:"fallback" json:"fallback"`
	Failure     string          `yaml:"failure" json:"failure"`
	RepeatAfter string          `yaml:"repeat_after" json:"repeat_after"`
	Recovery    *bool           `yaml:"recovery" json:"recovery"`
	DailyAt     string          `yaml:"daily_at" json:"daily_at"`
	Signal      *WorkerSignal   `yaml:"signal" json:"signal,omitempty"`
	Telegram    *WorkerTelegram `yaml:"telegram" json:"telegram,omitempty"`
}

type WorkerSignal struct {
	AccountFile   string `yaml:"account_file" json:"account_file"`
	RecipientFile string `yaml:"recipient_file" json:"recipient_file"`
}

type WorkerTelegram struct {
	TokenFile  string `yaml:"token_file" json:"token_file"`
	ChatIDFile string `yaml:"chat_id_file" json:"chat_id_file"`
}

type WorkerMonitoring struct {
	BackupPingURLFile        string `yaml:"backup_ping_url_file" json:"backup_ping_url_file"`
	DailyDeliveryPingURLFile string `yaml:"daily_delivery_ping_url_file" json:"daily_delivery_ping_url_file"`
}

func NormalizeWorker(c BackupWorkerConfig) (BackupWorkerConfig, error) {
	fail := func(msg string) (BackupWorkerConfig, error) { return c, fmt.Errorf("backup_worker: %s", msg) }
	if len(c.Sources) == 0 || len(c.Sources) > 128 {
		return fail("sources must contain 1 to 128 explicit server names")
	}
	seen := map[string]bool{}
	for _, s := range c.Sources {
		if !ValidInventoryName(s) || seen[s] {
			return fail("invalid or duplicate source")
		}
		seen[s] = true
	}
	checks := map[string]bool{}
	for i := range c.RestoreChecks {
		check := &c.RestoreChecks[i]
		key := check.Source + "/" + check.Instance
		if !seen[check.Source] || !ServicePattern.MatchString(check.Instance) || checks[key] || len(check.Command) == 0 || len(check.Command) > 32 {
			return fail("invalid or duplicate restore_checks scope/command")
		}
		checks[key] = true
		if err := PinnedWorkerImage(check.Image); err != nil {
			return fail("restore_checks image must be pinned by digest")
		}
		for _, arg := range check.Command {
			if strings.ContainsAny(arg, "\x00\r\n") || len(arg) > 4096 {
				return fail("invalid restore_checks command argument")
			}
		}
		if check.Timeout == "" {
			check.Timeout = "2m"
		}
		if _, err := PositiveWorkerDuration(check.Timeout, 5*time.Minute); err != nil {
			return fail("invalid restore_checks timeout")
		}
	}
	if !WorkerAbsolutePath(c.Storage.Path) || !WorkerAbsolutePath(c.Storage.RequireMount) {
		return fail("storage.path and storage.require_mount must be clean absolute non-root paths")
	}
	if c.Storage.Path != c.Storage.RequireMount && !strings.HasPrefix(c.Storage.Path, c.Storage.RequireMount+"/") {
		return fail("storage.path must be inside storage.require_mount")
	}
	if c.Storage.MinFreeBytes == 0 {
		c.Storage.MinFreeBytes = 1 << 30
	}
	if c.Storage.MinFreeBytes < 0 {
		return fail("storage.min_free_bytes must be positive")
	}
	if c.Schedule == "" {
		c.Schedule = "hourly"
	}
	if c.Timezone == "" {
		c.Timezone = "Europe/Vienna"
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "Local" {
		return fail("timezone must be a valid explicit IANA timezone")
	}
	// Restrict v1 to schedules whose maximum interval can be established locally.
	// Arbitrary systemd calendar expressions need an explicit occurrence policy.
	interval := map[string]time.Duration{"hourly": time.Hour, "daily": 25 * time.Hour, "weekly": 169 * time.Hour}[c.Schedule]
	if interval == 0 {
		return fail("supported schedules are hourly, daily and weekly; arbitrary calendar expressions are not qualified")
	}
	if c.Freshness == "" {
		c.Freshness = "2h"
	}
	if c.MaxRunTime == "" {
		c.MaxRunTime = "40m"
	}
	freshness, err := PositiveWorkerDuration(c.Freshness, 31*24*time.Hour)
	if err != nil {
		return fail("invalid freshness")
	}
	run, err := PositiveWorkerDuration(c.MaxRunTime, 24*time.Hour)
	if err != nil {
		return fail("invalid max_run_time")
	}
	if c.MaxSnapshotBytes == 0 {
		c.MaxSnapshotBytes = 10 << 30
	}
	if c.MaxSnapshotBytes < 4096 || c.MaxSnapshotBytes > 1<<40 {
		return fail("max_snapshot_bytes must be between 4096 and 1 TiB")
	}
	defaults := WorkerTimeouts{"10s", "30s", "5m", "10m", "5m", "10s"}
	v, d := reflect.ValueOf(&c.Timeouts).Elem(), reflect.ValueOf(defaults)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).String() == "" {
			v.Field(i).SetString(d.Field(i).String())
		}
		if _, err := PositiveWorkerDuration(v.Field(i).String(), 24*time.Hour); err != nil {
			return fail("invalid timeouts." + v.Type().Field(i).Tag.Get("yaml"))
		}
	}
	if Budget(c.Timeouts.ShutdownGrace, "10s") > time.Minute {
		return fail("shutdown_grace must be at most 1m")
	}
	// Reserve host launch/cleanup, shutdown grace and one minute timer jitter.
	delay := run + 3*time.Minute + Budget(c.Timeouts.ShutdownGrace, "10s")
	if delay >= interval {
		return fail("max_run_time plus launch/cleanup, shutdown and timer delay must fit the schedule")
	}
	if freshness <= interval+delay {
		return fail("freshness must exceed the schedule interval plus execution delay")
	}
	if c.Retention != nil {
		policy := *c.Retention
		if policy == (RetentionConfig{}) {
			return fail("an explicit retention block cannot be empty")
		}
		if policy.KeepLast == 0 {
			policy.KeepLast = 48
		}
		if policy.KeepLast < 1 || policy.KeepLast > 1000000 {
			return fail("retention.keep_last must be 1 to 1000000")
		}
		if policy.MaxAge != "" {
			if _, err := PositiveWorkerDuration(policy.MaxAge, 10*365*24*time.Hour); err != nil {
				return fail("invalid retention.max_age")
			}
		}
		c.Retention = &policy
	}
	n := &c.Notifications
	if n.Failure == "" {
		n.Failure = "immediate"
	}
	if n.Failure != "immediate" {
		return fail("notifications.failure must be immediate")
	}
	if n.RepeatAfter == "" {
		n.RepeatAfter = "6h"
	}
	if _, err := PositiveWorkerDuration(n.RepeatAfter, 30*24*time.Hour); err != nil {
		return fail("invalid notifications.repeat_after")
	}
	if n.Recovery == nil {
		yes := true
		n.Recovery = &yes
	}
	if n.DailyAt == "" {
		n.DailyAt = "09:00"
	}
	if parsed, err := time.Parse("15:04", n.DailyAt); err != nil || parsed.Format("15:04") != n.DailyAt {
		return fail("notifications.daily_at must be HH:MM")
	}
	configured := map[string]bool{"signal": n.Signal != nil, "telegram": n.Telegram != nil}
	if !configured[n.Primary] {
		return fail("notifications.primary must name a configured signal or telegram provider")
	}
	if n.Fallback != "" && (!configured[n.Fallback] || n.Fallback == n.Primary) {
		return fail("notifications.fallback must name a different configured provider")
	}
	secretPaths := []string{}
	if n.Signal != nil {
		secretPaths = append(secretPaths, n.Signal.AccountFile, n.Signal.RecipientFile)
	}
	if n.Telegram != nil {
		secretPaths = append(secretPaths, n.Telegram.TokenFile, n.Telegram.ChatIDFile)
	}
	for _, s := range secretPaths {
		if !WorkerAbsolutePath(s) {
			return fail("notification secret files must be absolute worker-host paths")
		}
	}
	for _, s := range []string{c.Monitoring.BackupPingURLFile, c.Monitoring.DailyDeliveryPingURLFile} {
		if s != "" && !WorkerAbsolutePath(s) {
			return fail("monitoring secret files must be absolute worker-host paths")
		}
	}
	if c.Monitoring.BackupPingURLFile != "" && c.Monitoring.BackupPingURLFile == c.Monitoring.DailyDeliveryPingURLFile {
		return fail("backup and delivery monitoring must use distinct checks")
	}
	return c, nil
}

func PositiveWorkerDuration(s string, max time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > max {
		return 0, fmt.Errorf("invalid positive duration")
	}
	return d, nil
}

func WorkerAbsolutePath(s string) bool {
	return path.IsAbs(s) && path.Clean(s) == s && s != "/" && !strings.ContainsAny(s, "\x00\r\n%")
}

// Backup opt-in has a deliberately narrower profile than ordinary deployment.
func ValidateBackupBinding(e Environment) error {
	if !e.Database.Backup.Enabled {
		return nil
	}
	if e.User == "root" {
		return fmt.Errorf("database.backup requires a rootless source")
	}
	_, database, err := DatabasePaths(e)
	if err != nil {
		return err
	}
	for _, p := range strings.Split(e.Database.Source, "/") {
		if p == "backup" {
			return fmt.Errorf("database.backup source cannot be in a backup directory")
		}
	}
	if len(e.Quadlet.PodmanArgs) > 0 {
		return fmt.Errorf("database.backup does not support podman_args overrides; declare the database bind in volumes")
	}
	matches := 0
	destinations := map[string]bool{}
	for _, v := range e.Quadlet.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return fmt.Errorf("database.backup requires explicit source:destination volumes")
		}
		src, dst := parts[0], parts[1]
		if strings.HasPrefix(src, "./") {
			src = path.Join(e.Dir, src)
		}
		if !path.IsAbs(dst) || strings.ContainsAny(v, "%\\\"'\t ") {
			return fmt.Errorf("database.backup has an unsupported volume binding")
		}
		dst = path.Clean(dst)
		for other := range destinations {
			if PathsOverlap(dst, other) {
				return fmt.Errorf("database.backup has overlapping mount destinations")
			}
		}
		destinations[dst] = true
		if len(parts) == 3 {
			for _, option := range strings.Split(parts[2], ",") {
				switch option {
				case "ro", "rw", "z", "Z":
				default:
					return fmt.Errorf("database.backup has an unsupported volume option")
				}
			}
		}
		if path.IsAbs(src) && strings.HasPrefix(database, path.Clean(src)+"/") {
			if path.Dir(database) != path.Clean(src) {
				return fmt.Errorf("database.backup requires the database directly inside its dedicated bind directory")
			}
			if path.Clean(src) == "/" || path.Clean(src) == e.Dir {
				return fmt.Errorf("database.backup requires a dedicated database directory bind")
			}
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("database.backup requires exactly one host bind containing the database directory")
	}
	return nil
}

func PathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}
