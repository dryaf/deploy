package workerctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/ledhost"
)

type Change struct {
	Field      string `json:"field"`
	Transition string `json:"transition"`
}

type Plan struct {
	Omitted      bool               `json:"omitted,omitempty"`
	DesiredScope int                `json:"desired_scope,omitempty"`
	LEDHost      *ledhost.Plan      `json:"led_host,omitempty"`
	Observed     *TargetStatus      `json:"observed,omitempty"`
	Version      int                `json:"version"`
	Server       string             `json:"server"`
	Worker       string             `json:"worker"`
	Operation    string             `json:"operation"`
	State        string             `json:"state"`
	Code         string             `json:"code"`
	DesiredHash  string             `json:"desired_hash"`
	Paths        *ResolvedPaths     `json:"paths,omitempty"`
	Artifacts    *ResolvedArtifacts `json:"artifacts,omitempty"`
	Missing      []string           `json:"missing,omitempty"`
	Actions      []string           `json:"actions,omitempty"`
	Changes      []Change           `json:"changes,omitempty"`
	Unresolved   []string           `json:"unresolved,omitempty"`
	Next         string             `json:"next,omitempty"`
}

func DeclarationPlan(inv config.ServerInventory, kind, alias, operation string) (Plan, error) {
	p := Plan{Version: 1, Server: alias, Worker: kind, Operation: operation, State: "planned", Code: "ok"}
	if inv.Version != 3 {
		return p, fmt.Errorf("short worker commands require inventory version 3; preview migration with deploy server migrate --to-version 3")
	}
	s, ok := inv.Servers[alias]
	if !ok {
		return p, fmt.Errorf("unknown server %q", alias)
	}
	if s.Desired == nil {
		return p, fmt.Errorf("server has no desired worker policy")
	}
	d := *s.Desired
	switch kind {
	case "backup-worker":
		if d.Backup == nil {
			return omittedDeclarationPlan(p, d)
		}
		if d.Backup.Notifications.Worker == "" {
			d.Notification = nil
		}
		if !config.WorkerEnabled(d.Backup.Enabled) {
			p.Actions = []string{"stop backup production in dependency order and retain archive/history"}
		} else {
			p.Actions = []string{"resolve target paths and verify prerequisites", "select and verify matching release artifacts", "enroll declared sources", "reconcile backup policy through its journal", "check required qualification before activation"}
		}
		if d.Backup.Notifications.Worker != "" && config.WorkerEnabled(d.Backup.Enabled) && config.WorkerEnabled(d.Backup.Notifications.Enabled) {
			p.Actions = append([]string{"reconcile local notification dependency and enroll the real backup producer"}, p.Actions...)
		}
	case "notification-worker":
		if d.Notification == nil {
			return omittedDeclarationPlan(p, d)
		}
		// Retain the declared backup policy for producer discovery, but missing source
		// selection does not prevent a standalone notification bootstrap.
		if !config.WorkerEnabled(d.Notification.Enabled) {
			p.Actions = []string{"stop dependent production and retain account, queues and history"}
		} else {
			p.Actions = []string{"resolve target paths and verify prerequisites", "select and verify matching release artifacts", "resolve private provider references", "initialize or reconcile notification enrollment", "check required qualification before activation"}
		}
		if d.Notification.StatusLEDs.Enabled {
			p.Actions = append(p.Actions, "discover supported LEDs and reconcile owned output")
		}
	default:
		return p, fmt.Errorf("unknown worker kind")
	}
	p.DesiredHash, _ = d.Fingerprint()
	if kind == "notification-worker" {
		p.DesiredScope = 1
		p.DesiredHash, _ = d.NotificationFingerprint()
	}
	switch operation {
	case "uninstall":
		if kind == "backup-worker" {
			p.Actions = []string{"verify retained installation and source administrator endpoints", "stop worker activity and revoke source access", "remove only unchanged owned service/configuration files", "retain archive, identities and lifecycle history"}
		} else {
			p.Actions = []string{"verify the retained unbound gateway and reject dependent consumers", "stop and remove its owned verification service", "retain gateway identity, database, credentials and audit history"}
		}
	case "status":
		p.Actions = []string{"read target filesystem/service observations and canonical installation receipts"}
	case "adopt":
		if kind == "backup-worker" {
			p.Actions = []string{"verify saved YAML against retained standalone backup and archive ownership", "retain source keys, writer identity and runtime files while recording declaration ownership"}
			break
		}
		p.Actions = []string{"verify saved YAML against the existing unbound direct-provider gateway and owned native service", "atomically record coordinator ownership without changing services or credentials"}
	case "import":
		p.Actions = []string{"verify retained domain ownership without changing services", "emit representable YAML as a review candidate; preserve identities and history"}
	case "configure":
		p.Actions = []string{"collect missing declaration inputs and private credential references", "save a concurrency-checked YAML edit or emit a read-only candidate"}
	case "qualify":
		p.Actions = []string{"verify the current completed enrollment and qualification binding", "collect operator observations and evidence file digests", "retain a resumable private draft and record supplied observations without activation"}
	case "activate":
		if kind != "backup-worker" {
			return p, fmt.Errorf("notification delivery admission uses apply after qualification")
		}
		p.Actions = []string{"verify exact installed declaration and current qualification", "verify a recent successful backup, restricted sources and reporting ownership", "enable the declared schedule and reconcile any retained activation intent"}
	case "run":
		if kind != "backup-worker" {
			return p, fmt.Errorf("the short run command is for backup-worker; notification workers run under their managed service")
		}
		if !config.WorkerEnabled(d.Backup.Enabled) {
			return p, fmt.Errorf("enable and apply the backup declaration before requesting a manual run")
		}
		p.Actions = []string{"verify exact installed declaration, artifact and owned backup service", "request a manual backup only if the service is idle; retain timer state", "inspect actual backup results separately before schedule activation"}
	case "test":
		if kind != "notification-worker" {
			return p, fmt.Errorf("the short test command is for notification-worker; backup diagnostics use notify-test")
		}
		p.Actions = []string{"verify the exact installed declaration and retained diagnostic outcome", "send one explicitly requested diagnostic only if no attempt was started", "retain the outcome without granting production qualification"}
	}
	if operation != "import" {
		p.Missing = MissingForWorker(kind, d)
	}
	if len(p.Missing) > 0 {
		p.State = "needs_input"
		p.Code = "missing_input"
		p.Next = "deploy " + kind + " configure " + alias
	}
	p.Unresolved = []string{"target identity and home/XDG paths", "installed ownership and pending operations", "provider files and qualification", "target architecture and artifact availability"}
	return p, nil
}

func ResolvePlan(p Plan, s config.ServerConfig, f TargetFacts, defaultCatalog string) (Plan, error) {
	var declared config.WorkerPaths
	var artifact config.WorkerArtifacts
	if p.Worker == "backup-worker" {
		declared = s.Desired.Backup.Paths
		artifact = s.Desired.Backup.Artifacts
	} else {
		declared = s.Desired.Notification.Paths
		artifact = s.Desired.Notification.Artifacts
	}
	paths, err := ResolvePaths(p.Worker, declared, f)
	if err != nil {
		return p, err
	}
	if p.Worker == "backup-worker" && s.Desired.Backup.Storage.Path != "" {
		paths.Archive, err = ResolveTargetPath(s.Desired.Backup.Storage.Path, f.Home)
		if err != nil {
			return p, err
		}
	}
	p.Paths = &paths
	p.Unresolved = []string{"installed ownership and pending operations", "provider files and qualification"}
	if !f.UserManager {
		p.Missing = append(p.Missing, "target.user_manager")
	}
	disabling := p.Worker == "notification-worker" && !config.WorkerEnabled(s.Desired.Notification.Enabled) || p.Worker == "backup-worker" && !config.WorkerEnabled(s.Desired.Backup.Enabled)
	disabling = disabling || sharedActivityStopping(*s.Desired, p.Server)
	if !f.Linger && !disabling {
		p.Missing = append(p.Missing, "target.linger")
	}
	required := []string{"systemctl"}
	if p.Worker == "backup-worker" && !disabling {
		required = append(required, "podman", "ssh", "ssh-keygen", "bwrap")
		layout, e := backup.ResolveInstallationPaths(f.Home, f.ConfigHome, f.StateHome, f.DataHome, declared)
		if e != nil {
			return p, e
		}
		legacy, e := backup.ResolveInstallationPaths(f.Home, "", "", "", config.WorkerPaths{})
		if e != nil {
			return p, e
		}
		if !reflect.DeepEqual(layout, legacy) {
			required = append(required, "busctl")
		}
	}
	for _, name := range required {
		if !f.Tools[name] {
			p.Missing = append(p.Missing, "target.tools."+name)
		}
	}
	resolved, err := ResolveArtifacts(artifact, f.Arch, defaultCatalog)
	if err != nil {
		p.Unresolved = append(p.Unresolved, err.Error())
	} else {
		p.Artifacts = &resolved
		if p.Worker == "backup-worker" && !disabling && resolved.Image == "" {
			p.Missing = append(p.Missing, "backup_worker.artifacts.image")
		}
	}
	notificationRelevant := p.Worker == "notification-worker" || s.Desired.Backup != nil && config.WorkerEnabled(s.Desired.Backup.Enabled) && config.WorkerEnabled(s.Desired.Backup.Notifications.Enabled) && s.Desired.Backup.Notifications.Worker != ""
	if n := s.Desired.Notification; notificationRelevant && n != nil && config.WorkerEnabled(n.Enabled) && n.StatusLEDs.Enabled {
		if !strings.Contains(strings.ToLower(f.Board), "raspberry pi") || len(f.LEDs) == 0 {
			p.Missing = append(p.Missing, "target.supported_status_led")
		}
		if n.StatusLEDs.Daemon.Mode == "managed" && p.Artifacts != nil {
			a, e := ResolveLEDArtifact(n.StatusLEDs.Daemon, p.Artifacts.LEDs)
			if e != nil {
				p.Missing = append(p.Missing, "notification_worker.status_leds.daemon.artifact")
				p.Unresolved = append(p.Unresolved, e.Error())
			} else {
				p.Artifacts.LEDs = a
				spec, e := ledhost.ResolveSpec(f.UID, f.LEDs, n.StatusLEDs.Daemon.Device, n.StatusLEDs.PowerLED, a.SHA256, func(name string) (string, error) { return f.LEDPaths[name], nil })
				if e != nil {
					p.Missing = append(p.Missing, "notification_worker.status_leds.daemon.device")
					p.Unresolved = append(p.Unresolved, e.Error())
				} else {
					host, e := ledhost.Build(spec)
					if e != nil {
						p.Unresolved = append(p.Unresolved, "target LED device path must be rediscovered before staging its host prerequisite")
					} else {
						p.LEDHost = &host
						p.Actions = append(p.Actions, host.Actions...)
						p.Unresolved = append(p.Unresolved, "managed LED daemon requires a reviewed administrator prerequisite before first installation")
					}
				}
			}
		}
	}
	sort.Strings(p.Missing)
	if len(p.Missing) > 0 {
		p.State = "needs_input"
		p.Code = "missing_input"
	}
	return p, nil
}

// PolicyChanges contains field paths/classifications only. Values may contain
// private destinations; neither plaintext values nor secret digests enter diffs.
func PolicyChanges(before, after config.DesiredWorkers) []Change {
	a, b := flattenPolicy(before), flattenPolicy(after)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var changes []Change
	for key := range keys {
		if !reflect.DeepEqual(a[key], b[key]) {
			class := changeClass(key)
			if key == "backup_worker.freshness" {
				oldScope, nextScope := before.NotificationScope(), after.NotificationScope()
				if oldScope.Backup != nil && nextScope.Backup != nil && oldScope.Backup.Freshness != nextScope.Backup.Freshness {
					class = "producer_policy_migration"
				}
			}
			changes = append(changes, Change{key, class})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Field < changes[j].Field })
	return changes
}
func flattenPolicy(d config.DesiredWorkers) map[string]any {
	raw, _ := json.Marshal(d)
	var body any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&body)
	out := map[string]any{}
	var walk func(string, any)
	walk = func(prefix string, v any) {
		if m, ok := v.(map[string]any); ok {
			for k, x := range m {
				next := k
				if prefix != "" {
					next = prefix + "." + k
				}
				walk(next, x)
			}
		} else {
			out[prefix] = v
		}
	}
	walk("", body)
	return out
}
func changeClass(field string) string {
	switch {
	case strings.HasPrefix(field, "backup_worker.retire_sources"):
		return "source_retirement"
	case field == "backup_worker.storage.path" || field == "backup_worker.storage.mode" || field == "backup_worker.storage.require_mount":
		return "path_migration"
	case strings.HasPrefix(field, "backup_worker.monitoring."):
		return "monitor_review"
	case field == "backup_worker.notifications.failure" || field == "backup_worker.notifications.repeat_after" || field == "backup_worker.notifications.recovery" || field == "backup_worker.notifications.daily_at":
		return "backup_update"
	case strings.Contains(field, ".paths."):
		return "path_migration"
	case strings.Contains(field, ".artifacts."):
		return "artifact_update"
	case strings.Contains(field, ".status_leds."):
		return "led_transition"
	case strings.Contains(field, ".commands."):
		return "command_admission"
	case strings.Contains(field, ".sources") || strings.Contains(field, ".producers."):
		return "producer_enrollment"
	case strings.Contains(field, ".providers."):
		return "provider_review"
	case strings.Contains(field, ".routes.") || strings.Contains(field, ".notifications."):
		return "route_migration"
	case strings.Contains(field, ".limits."):
		return "budget_review"
	case strings.HasSuffix(field, ".enabled"):
		return "activity_transition"
	case strings.HasPrefix(field, "backup_worker."):
		return "backup_update"
	default:
		return "installation_review"
	}
}

func MissingForWorker(kind string, d config.DesiredWorkers) []string {
	if kind == "notification-worker" {
		d.Backup = nil
	} else if d.Backup == nil || !config.WorkerEnabled(d.Backup.Enabled) || !config.WorkerEnabled(d.Backup.Notifications.Enabled) || d.Backup.Notifications.Worker == "" {
		d.Notification = nil
	}
	return d.MissingInputs()
}
