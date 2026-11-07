package config

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var desiredToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

var desiredLEDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

type inventoryV3 struct {
	Version int                 `yaml:"version"`
	Servers map[string]serverV3 `yaml:"servers"`
}
type serverV3 struct {
	Host           string      `yaml:"host"`
	User           string      `yaml:"user"`
	SSHPort        int         `yaml:"ssh_port,omitempty"`
	SSHKey         string      `yaml:"ssh_key,omitempty"`
	KnownHostsFile string      `yaml:"ssh_known_hosts,omitempty"`
	Stack          ServerStack `yaml:"stack,omitempty"`
	DesiredWorkers `yaml:",inline"`
}

func readInventoryV3(file string, data []byte, doc *yaml.Node) (ServerInventory, error) {
	inv := ServerInventory{Version: 3, Servers: map[string]ServerConfig{}}
	if err := validateDesiredNode(doc, "inventory"); err != nil {
		return inv, err
	}
	var raw inventoryV3
	if err := DecodeConfig(file, data, &raw); err != nil {
		return inv, err
	}
	if raw.Version != 3 || len(raw.Servers) == 0 {
		return inv, fmt.Errorf("inventory requires version: 3 and nonempty servers")
	}
	fields := yamlFields(doc, map[*yaml.Node]bool{})
	servers := yamlFields(fields["servers"], map[*yaml.Node]bool{})
	names := make([]string, 0, len(raw.Servers))
	for name := range raw.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := raw.Servers[name]
		fail := func(err error) (ServerInventory, error) { return inv, fmt.Errorf("servers.%s: %w", name, err) }
		if !ValidInventoryName(name) || !HostPattern.MatchString(s.Host) || !UserPattern.MatchString(s.User) {
			return fail(fmt.Errorf("explicit valid server name, host and user required"))
		}
		present := yamlFields(servers[name], map[*yaml.Node]bool{})
		if s.SSHPort == 0 && present["ssh_port"] == nil {
			s.SSHPort = 22
		}
		if s.SSHPort < 1 || s.SSHPort > 65535 {
			return fail(fmt.Errorf("ssh_port must be between 1 and 65535"))
		}
		if err := validateSingleLines(reflect.ValueOf(s), "servers."+name); err != nil {
			return inv, err
		}
		if s.User == "root" && (s.Backup != nil || s.Notification != nil) {
			return fail(fmt.Errorf("workers require a rootless user"))
		}
		var err error
		s.SSHKey, err = localInventoryPath(file, s.SSHKey)
		if err != nil {
			return fail(err)
		}
		s.KnownHostsFile, err = localInventoryPath(file, s.KnownHostsFile)
		if err != nil {
			return fail(err)
		}
		if s.Notification != nil {
			if err = normalizeNotification(file, s.Notification); err != nil {
				return fail(fmt.Errorf("notification_worker: %w", err))
			}
		}
		if s.Backup != nil {
			if err = normalizeBackupDeclaration(file, s.Backup); err != nil {
				return fail(err)
			}
			n := &s.Backup.Notifications
			if WorkerEnabled(n.Enabled) && n.Worker == "" && n.Primary == "" && s.Notification != nil {
				n.Worker = name
			}
			if n.Enabled == nil {
				enabled := n.Worker != "" || n.Primary != ""
				n.Enabled = &enabled
			}
			if n.Worker != "" && n.Route == "" {
				n.Route = "alerts"
			}
		}
		desired := s.DesiredWorkers
		inv.Servers[name] = ServerConfig{Host: s.Host, User: s.User, SSHPort: s.SSHPort, SSHKey: s.SSHKey, KnownHostsFile: s.KnownHostsFile, Stack: s.Stack, Desired: &desired}
	}
	for _, name := range names {
		s := inv.Servers[name]
		b := s.Desired.Backup
		if b == nil {
			continue
		}
		seen := map[string]bool{}
		for _, alias := range b.Sources {
			source, ok := inv.Servers[alias]
			if !ok {
				return inv, fmt.Errorf("servers.%s.backup_worker.sources: unknown source %s", name, alias)
			}
			account := serverAccount(source)
			if source.User == "root" || seen[account] || account == serverAccount(s) {
				return inv, fmt.Errorf("servers.%s.backup_worker.sources: sources require distinct rootless SSH accounts", name)
			}
			seen[account] = true
		}
		n := b.Notifications
		if n.Worker != "" {
			target, ok := inv.Servers[n.Worker]
			if !ok || target.Desired.Notification == nil || WorkerEnabled(b.Enabled) && WorkerEnabled(n.Enabled) && !WorkerEnabled(target.Desired.Notification.Enabled) {
				return inv, fmt.Errorf("servers.%s.backup_worker.notifications.worker: referenced worker is absent or disabled", name)
			}
			if n.Worker != name {
				return inv, fmt.Errorf("servers.%s.backup_worker.notifications.worker: cross-server notification transport is not supported", name)
			}
			if len(target.Desired.Notification.Routes) > 0 {
				if _, ok := target.Desired.Notification.Routes[n.Route]; !ok {
					return inv, fmt.Errorf("servers.%s.backup_worker.notifications.route: unknown route", name)
				}
			}
			if producer, explicit := target.Desired.Notification.Producers["backup-worker"]; explicit && WorkerEnabled(b.Enabled) && WorkerEnabled(n.Enabled) {
				allowed := false
				for _, route := range producer.Routes {
					allowed = allowed || route == n.Route
				}
				if !allowed {
					return inv, fmt.Errorf("servers.%s.backup_worker.notifications.route: not allowed by notification_worker.producers.backup-worker.routes", name)
				}
			}
		}
	}
	return inv, nil
}

func normalizeBackupDeclaration(file string, b *BackupDeclaration) error {
	if b.Enabled == nil {
		v := true
		b.Enabled = &v
	}
	if b.Freshness == "" {
		switch b.Schedule {
		case "daily":
			b.Freshness = "26h"
		case "weekly":
			b.Freshness = "170h"
		}
	}
	if b.Timezone == "" {
		b.Timezone = "UTC"
	}
	if b.Storage.Mode == "" {
		b.Storage.Mode = "home"
		if b.Storage.RequireMount != "" {
			b.Storage.Mode = "mount"
		}
	}
	if b.Storage.Mode != "home" && b.Storage.Mode != "mount" {
		return fmt.Errorf("backup_worker.storage.mode: expected home or mount")
	}
	if b.Storage.Mode == "home" && b.Storage.RequireMount != "" {
		return fmt.Errorf("backup_worker.storage: home mode cannot require a mount")
	}
	if b.Storage.Mode == "mount" {
		if b.Storage.Path == "" || !WorkerAbsolutePath(b.Storage.RequireMount) || !WorkerAbsolutePath(b.Storage.Path) {
			return fmt.Errorf("backup_worker.storage: mount mode requires absolute path and require_mount")
		}
		if b.Storage.Path != b.Storage.RequireMount && !strings.HasPrefix(b.Storage.Path, b.Storage.RequireMount+"/") {
			return fmt.Errorf("backup_worker.storage.path: must be inside require_mount")
		}
	}
	if b.Storage.Path != "" && !TargetPath(b.Storage.Path) {
		return fmt.Errorf("backup_worker.storage.path: invalid target path")
	}
	n := b.Notifications
	if n.Worker != "" && (n.Primary != "" || n.Fallback != "" || n.Signal != nil || n.Telegram != nil) {
		return fmt.Errorf("backup_worker.notifications: shared worker and legacy providers are mutually exclusive")
	}
	if n.Route != "" && !desiredToken.MatchString(n.Route) || n.Worker != "" && !ValidInventoryName(n.Worker) {
		return fmt.Errorf("backup_worker.notifications: invalid worker or route name")
	}
	if len(b.RetireSources) > 128 {
		return fmt.Errorf("backup_worker.retire_sources: at most 128 retained dispositions")
	}
	for alias, reason := range b.RetireSources {
		if !ValidInventoryName(alias) || strings.TrimSpace(reason) == "" || len(reason) > 1024 || strings.ContainsAny(reason, "\x00\r\n") {
			return fmt.Errorf("backup_worker.retire_sources: expected source alias and a bounded single-line reason")
		}
		for _, active := range b.Sources {
			if alias == active {
				return fmt.Errorf("backup_worker.retire_sources: a retired source cannot remain in sources")
			}
		}
	}
	// Reuse schedule, retention, capture, source and timing validation. Runtime
	// path and provider admission remain stricter than this desired declaration.
	c, err := normalizeWorker(BackupWorkerConfig{Sources: b.Sources, Storage: WorkerStorage{Path: b.Storage.Path, RequireMount: b.Storage.RequireMount, MinFreeBytes: b.Storage.MinFreeBytes}, Schedule: b.Schedule, Timezone: b.Timezone, Freshness: b.Freshness, MaxRunTime: b.MaxRunTime, MaxSnapshotBytes: b.MaxSnapshotBytes, Timeouts: b.Timeouts, Retention: b.Retention, Notifications: b.Notifications.WorkerNotifications, Monitoring: b.Monitoring, RestoreChecks: b.RestoreChecks}, true)
	if err != nil {
		return err
	}
	b.Schedule = c.Schedule
	b.Timezone = c.Timezone
	b.Freshness = c.Freshness
	b.MaxRunTime = c.MaxRunTime
	b.MaxSnapshotBytes = c.MaxSnapshotBytes
	b.Timeouts = c.Timeouts
	b.Retention = c.Retention
	b.Notifications.WorkerNotifications = c.Notifications
	b.Storage.MinFreeBytes = c.Storage.MinFreeBytes
	b.RestoreChecks = c.RestoreChecks
	sort.Strings(b.Sources)
	if err = validateWorkerPaths(b.Paths); err != nil {
		return err
	}
	return normalizeArtifacts(file, &b.Artifacts)
}

func normalizeNotification(file string, n *NotificationDeclaration) error {
	if n.Artifacts.Image != "" || n.Artifacts.RelayImage != "" {
		return fmt.Errorf("artifacts: image and relay_image belong to backup_worker; notification-worker uses native helper/provider artifacts")
	}
	if n.Enabled == nil {
		v := true
		n.Enabled = &v
	}
	for name, p := range n.Providers {
		if name == "telegram" && (p.AccountFile != "" || p.RecipientFile != "" || p.AccountInput != "" || p.RecipientInput != "" || p.Socket != "" || p.Data != "" || p.Executable != "") {
			return fmt.Errorf("providers.telegram: Signal fields are not allowed")
		}
		if name == "signal" && (p.TokenFile != "" || p.ChatIDFile != "" || p.TokenInput != "" || p.ChatIDInput != "" || p.BotName != "") {
			return fmt.Errorf("providers.signal: Telegram fields are not allowed")
		}
		if err := validateDesiredPaths(reflect.ValueOf(p), "providers."+name); err != nil {
			return err
		}
		for _, v := range []string{p.Data, p.Executable} {
			if v != "" && !TargetPath(v) {
				return fmt.Errorf("providers.%s: invalid target path", name)
			}
		}
		if (p.Executable == "") != (p.ExecutableSHA256 == "") || p.ExecutableSHA256 != "" && !digestString(p.ExecutableSHA256) {
			return fmt.Errorf("providers.%s.executable requires exact executable_sha256", name)
		}
		for _, pair := range [][2]*string{{&p.TokenFile, &p.TokenInput}, {&p.ChatIDFile, &p.ChatIDInput}, {&p.AccountFile, &p.AccountInput}, {&p.RecipientFile, &p.RecipientInput}} {
			if *pair[0] != "" && *pair[1] != "" {
				return fmt.Errorf("providers.%s: target reference and operator-local input are mutually exclusive", name)
			}
			if *pair[1] != "" {
				v, err := localInventoryPath(file, *pair[1])
				if err != nil {
					return err
				}
				*pair[1] = v
			}
		}
		n.Providers[name] = p
	}
	if len(n.Providers) == 1 && len(n.Routes) == 0 {
		for name := range n.Providers {
			n.Routes = map[string]NotificationRoute{"alerts": {Primary: name}}
		}
	}
	if len(n.Routes) > 128 || len(n.Producers) > 128 {
		return fmt.Errorf("at most 128 routes and producers are supported")
	}
	for name, r := range n.Routes {
		_, primary := n.Providers[r.Primary]
		_, fallback := n.Providers[r.Fallback]
		if !desiredToken.MatchString(name) || !primary || r.Fallback != "" && (!fallback || r.Fallback == r.Primary) {
			return fmt.Errorf("routes.%s: primary/fallback must name distinct configured providers", name)
		}
	}
	l := &n.Limits
	if l.MaxMessages == 0 {
		l.MaxMessages = 10000
	}
	if l.MaxPayloadBytes == 0 {
		l.MaxPayloadBytes = 64 << 20
	}
	if l.MaxDatabaseBytes == 0 {
		l.MaxDatabaseBytes = 512 << 20
	}
	if l.MinFreeBytes == 0 {
		l.MinFreeBytes = 128 << 20
	}
	if l.ReceiptAuditDays == 0 {
		l.ReceiptAuditDays = 90
	}
	if l.MaxMessages < 1 || l.MaxMessages > 10000 || l.MaxPayloadBytes < 1024 || l.MaxPayloadBytes > 64<<20 || l.MaxDatabaseBytes < 1<<20 || l.MinFreeBytes < 0 || l.ReceiptAuditDays < 90 || l.ReceiptAuditDays > 3650 {
		return fmt.Errorf("limits: invalid storage or audit limits")
	}
	for name, p := range n.Producers {
		if !ValidInventoryName(name) {
			return fmt.Errorf("producers: invalid name")
		}
		if p.Required == nil {
			v := true
			p.Required = &v
		}
		if p.MaxSilenceSeconds == 0 {
			p.MaxSilenceSeconds = 120
		}
		if p.MaxEvidenceSeconds == 0 {
			p.MaxEvidenceSeconds = 7200
		}
		if p.MaxMessages == 0 {
			p.MaxMessages = 1000
		}
		if len(p.Routes) == 0 && n.Routes["alerts"].Primary != "" {
			p.Routes = []string{"alerts"}
		}
		if p.MaxSilenceSeconds < 10 || p.MaxSilenceSeconds > 86400 || p.MaxEvidenceSeconds < 1 || p.MaxEvidenceSeconds > 31*86400 || p.MaxMessages < 1 || p.MaxMessages > 10000 {
			return fmt.Errorf("producers.%s: invalid liveness or quota", name)
		}
		if p.InteractiveContext != (len(p.ContextKinds) > 0) || len(p.ContextKinds) > 16 {
			return fmt.Errorf("producers.%s: context kinds require interactive_context", name)
		}
		seen := map[string]bool{}
		for _, r := range p.Routes {
			if _, ok := n.Routes[r]; !ok || seen[r] {
				return fmt.Errorf("producers.%s.routes: unknown or duplicate route", name)
			}
			seen[r] = true
		}
		seen = map[string]bool{}
		for _, k := range p.ContextKinds {
			if !desiredToken.MatchString(k) || seen[k] {
				return fmt.Errorf("producers.%s.context_kinds: invalid or duplicate kind", name)
			}
			seen[k] = true
		}
		sort.Strings(p.Routes)
		sort.Strings(p.ContextKinds)
		n.Producers[name] = p
	}
	if n.Commands.Mode == "" {
		n.Commands.Mode = "read"
	}
	if n.Commands.Mode != "read" && n.Commands.Mode != "mutation" {
		return fmt.Errorf("commands.mode: expected read or mutation")
	}
	for name, c := range n.Commands.Conversations {
		if !desiredToken.MatchString(name) || !desiredToken.MatchString(c.Account) || c.NativeID == "" {
			return fmt.Errorf("commands.conversations.%s: account and native_id required", name)
		}
		if _, ok := n.Providers[c.Provider]; !ok {
			return fmt.Errorf("commands.conversations.%s: unknown provider", name)
		}
	}
	if n.Commands.Enabled {
		accounts := map[string]string{}
		for _, conversation := range n.Commands.Conversations {
			if old := accounts[conversation.Provider]; old != "" && old != conversation.Account {
				return fmt.Errorf("commands.conversations: one account per provider is supported")
			}
			accounts[conversation.Provider] = conversation.Account
		}
		for name, account := range accounts {
			p := n.Providers[name]
			if p.Account != "" && p.Account != account {
				return fmt.Errorf("providers.%s.account differs from command conversations", name)
			}
			p.Account = account
			n.Providers[name] = p
		}
	}
	for name, p := range n.Providers {
		if p.Conversation == "" {
			continue
		}
		conversation, ok := n.Commands.Conversations[p.Conversation]
		if !n.Commands.Enabled || !ok || conversation.Provider != name || conversation.Account != p.Account {
			return fmt.Errorf("providers.%s.conversation requires a matching enabled command conversation", name)
		}
		if name == "telegram" && (p.ChatIDFile != "" || p.ChatIDInput != "") || name == "signal" && (p.RecipientFile != "" || p.RecipientInput != "") {
			return fmt.Errorf("providers.%s.conversation excludes fixed recipient references", name)
		}
	}
	for alias, producer := range n.Commands.Workers {
		if !desiredToken.MatchString(alias) || !ValidInventoryName(producer) {
			return fmt.Errorf("commands.workers: invalid alias or producer")
		}
	}
	for _, p := range n.Commands.Permissions {
		if p.Actor == "" || len(p.Permissions) == 0 {
			return fmt.Errorf("commands.permissions: actor and permissions required")
		}
		if _, ok := n.Commands.Conversations[p.Conversation]; !ok {
			return fmt.Errorf("commands.permissions: unknown conversation")
		}
		if _, ok := n.Commands.Workers[p.Worker]; !ok {
			return fmt.Errorf("commands.permissions: unknown worker alias")
		}
	}
	if n.StatusLEDs.Daemon.Mode == "" {
		n.StatusLEDs.Daemon.Mode = "managed"
	}
	if n.StatusLEDs.Daemon.Mode != "managed" && n.StatusLEDs.Daemon.Mode != "existing" {
		return fmt.Errorf("status_leds.daemon.mode: expected managed or existing")
	}
	if n.StatusLEDs.Socket != "" && !TargetPath(n.StatusLEDs.Socket) {
		return fmt.Errorf("status_leds.socket: invalid target path")
	}
	if n.StatusLEDs.Enabled && n.StatusLEDs.Daemon.Mode == "existing" && n.StatusLEDs.Socket == "" {
		return fmt.Errorf("status_leds.socket: required for existing daemon")
	}
	daemon := &n.StatusLEDs.Daemon
	if daemon.Device != "" && (!regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`).MatchString(daemon.Device) || daemon.Mode == "existing") {
		return fmt.Errorf("status_leds.daemon.device: select a discovered device in managed mode")
	}
	if daemon.Mode == "existing" && (daemon.Artifact != "" || daemon.SHA256 != "") {
		return fmt.Errorf("status_leds.daemon: existing mode does not install an artifact")
	}
	if (daemon.Artifact == "") != (daemon.SHA256 == "") || daemon.SHA256 != "" && !digestString(daemon.SHA256) {
		return fmt.Errorf("status_leds.daemon.artifact requires exact sha256")
	}
	if daemon.Artifact != "" {
		var err error
		daemon.Artifact, err = localInventoryPath(file, daemon.Artifact)
		if err != nil {
			return err
		}
	}
	for name, b := range n.StatusLEDs.Bindings {
		if !ValidInventoryName(name) || !ValidInventoryName(b.Producer) || len(b.Subjects) == 0 || (b.LED != "green" && b.LED != "power") || b.LED == "power" && !n.StatusLEDs.PowerLED {
			return fmt.Errorf("status_leds.bindings.%s: invalid producer, subjects or LED (power requires power_led)", name)
		}
		for _, s := range b.Subjects {
			if !(s == "*" && len(b.Subjects) == 1) && !desiredToken.MatchString(s) {
				return fmt.Errorf("status_leds.bindings.%s: invalid subject", name)
			}
		}
	}
	for name, b := range n.StatusLEDs.Bindings {
		bands := map[string][2]int{"baseline": {0, 9}, "ok": {10, 39}, "paused": {40, 59}, "warning": {60, 79}, "unknown": {80, 89}, "critical": {90, 100}}
		if len(b.Subjects) > 128 {
			return fmt.Errorf("status_leds.bindings.%s: too many subjects", name)
		}
		seen := map[string]bool{}
		for _, subject := range b.Subjects {
			if seen[subject] {
				return fmt.Errorf("status_leds.bindings.%s: duplicate subject", name)
			}
			seen[subject] = true
		}
		for condition, style := range b.Styles {
			band, ok := bands[condition]
			if !ok || !desiredLEDPattern.MatchString(style.Pattern) || style.Priority < band[0] || style.Priority > band[1] {
				return fmt.Errorf("status_leds.bindings.%s.styles: invalid condition, pattern or priority band", name)
			}
		}
	}
	if len(n.StatusLEDs.Bindings) > 32 {
		return fmt.Errorf("status_leds.bindings: at most 32 mappings")
	}
	for i, p := range n.Commands.Permissions {
		if p.Scope.WholeWorker && len(p.Scope.Subjects) != 0 || !p.Scope.WholeWorker && len(p.Scope.Subjects) == 0 || len(p.Scope.Subjects) > 128 {
			return fmt.Errorf("commands.permissions: explicit whole_worker or subject scope required")
		}
		sort.Strings(p.Scope.Subjects)
		for j, subject := range p.Scope.Subjects {
			if !desiredToken.MatchString(subject) || j > 0 && p.Scope.Subjects[j-1] == subject {
				return fmt.Errorf("commands.permissions.scope: invalid or duplicate subject")
			}
		}
		sort.Strings(p.Permissions)
		for j, permission := range p.Permissions {
			if !desiredToken.MatchString(permission) || j > 0 && p.Permissions[j-1] == permission {
				return fmt.Errorf("commands.permissions: invalid or duplicate permission")
			}
		}
		n.Commands.Permissions[i] = p
	}
	if err := validateWorkerPaths(n.Paths); err != nil {
		return err
	}
	return normalizeArtifacts(file, &n.Artifacts)
}
