package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DesiredWorkers is policy, not an installation specification. Paths and secret
// references are resolved on the target before compiling any runtime input.
// Keeping it distinct prevents legacy setup from consuming unresolved v3 policy.
type DesiredWorkers struct {
	Backup       *BackupDeclaration       `yaml:"backup_worker,omitempty" json:"backup_worker,omitempty"`
	Notification *NotificationDeclaration `yaml:"notification_worker,omitempty" json:"notification_worker,omitempty"`
}

type WorkerPaths struct {
	Config        string   `yaml:"config,omitempty" json:"config,omitempty"`
	State         string   `yaml:"state,omitempty" json:"state,omitempty"`
	Data          string   `yaml:"data,omitempty" json:"data,omitempty"`
	Runtime       string   `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Installation  string   `yaml:"installation,omitempty" json:"installation,omitempty"`
	UnitDirectory string   `yaml:"unit_directory,omitempty" json:"unit_directory,omitempty"`
	DeniedPaths   []string `yaml:"denied_paths,omitempty" json:"denied_paths,omitempty"`
}

type WorkerArtifacts struct {
	Catalog      string `yaml:"catalog,omitempty" json:"catalog,omitempty"` // operator-local
	Helper       string `yaml:"helper,omitempty" json:"helper,omitempty"`   // operator-local
	HelperSHA256 string `yaml:"helper_sha256,omitempty" json:"helper_sha256,omitempty"`
	Image        string `yaml:"image,omitempty" json:"image,omitempty"`
	RelayImage   string `yaml:"relay_image,omitempty" json:"relay_image,omitempty"`
}

type BackupDeclaration struct {
	RetireSources    map[string]string          `yaml:"retire_sources,omitempty" json:"retire_sources,omitempty"`
	Enabled          *bool                      `yaml:"enabled,omitempty" json:"enabled"`
	Sources          []string                   `yaml:"sources" json:"sources,omitempty"`
	Storage          DesiredStorage             `yaml:"storage,omitempty" json:"storage"`
	Schedule         string                     `yaml:"schedule,omitempty" json:"schedule"`
	Timezone         string                     `yaml:"timezone,omitempty" json:"timezone"`
	Freshness        string                     `yaml:"freshness,omitempty" json:"freshness"`
	MaxRunTime       string                     `yaml:"max_run_time,omitempty" json:"max_run_time"`
	MaxSnapshotBytes int64                      `yaml:"max_snapshot_bytes,omitempty" json:"max_snapshot_bytes"`
	Timeouts         WorkerTimeouts             `yaml:"timeouts,omitempty" json:"timeouts"`
	Retention        *RetentionConfig           `yaml:"retention,omitempty" json:"retention"`
	Notifications    DesiredBackupNotifications `yaml:"notifications,omitempty" json:"notifications"`
	Monitoring       WorkerMonitoring           `yaml:"monitoring,omitempty" json:"monitoring"`
	RestoreChecks    []WorkerRestoreCheck       `yaml:"restore_checks,omitempty" json:"restore_checks,omitempty"`
	Paths            WorkerPaths                `yaml:"paths,omitempty" json:"paths"`
	Artifacts        WorkerArtifacts            `yaml:"artifacts,omitempty" json:"artifacts"`
}

type DesiredStorage struct {
	Mode         string `yaml:"mode,omitempty" json:"mode"`
	Path         string `yaml:"path,omitempty" json:"path"`
	RequireMount string `yaml:"require_mount,omitempty" json:"require_mount"`
	MinFreeBytes int64  `yaml:"min_free_bytes,omitempty" json:"min_free_bytes"`
}

type DesiredBackupNotifications struct {
	Worker              string `yaml:"worker,omitempty" json:"worker,omitempty"`
	Route               string `yaml:"route,omitempty" json:"route,omitempty"`
	Enabled             *bool  `yaml:"enabled,omitempty" json:"enabled"`
	WorkerNotifications `yaml:",inline"`
}

type NotificationDeclaration struct {
	Enabled    *bool                          `yaml:"enabled,omitempty" json:"enabled"`
	Providers  ProviderDeclarations           `yaml:"providers,omitempty" json:"providers,omitempty"`
	Routes     map[string]NotificationRoute   `yaml:"routes,omitempty" json:"routes,omitempty"`
	Producers  map[string]ProducerDeclaration `yaml:"producers,omitempty" json:"producers,omitempty"`
	Limits     NotificationLimits             `yaml:"limits,omitempty" json:"limits"`
	Commands   CommandDeclaration             `yaml:"commands,omitempty" json:"commands"`
	StatusLEDs LEDDeclaration                 `yaml:"status_leds,omitempty" json:"status_leds"`
	Paths      WorkerPaths                    `yaml:"paths,omitempty" json:"paths"`
	Artifacts  WorkerArtifacts                `yaml:"artifacts,omitempty" json:"artifacts"`
}

type ProviderDeclaration struct {
	TokenFile        string `yaml:"token_file,omitempty" json:"token_file,omitempty"`
	ChatIDFile       string `yaml:"chat_id_file,omitempty" json:"chat_id_file,omitempty"`
	AccountFile      string `yaml:"account_file,omitempty" json:"account_file,omitempty"`
	RecipientFile    string `yaml:"recipient_file,omitempty" json:"recipient_file,omitempty"`
	TokenInput       string `yaml:"token_input,omitempty" json:"token_input,omitempty"` // operator-local
	ChatIDInput      string `yaml:"chat_id_input,omitempty" json:"chat_id_input,omitempty"`
	AccountInput     string `yaml:"account_input,omitempty" json:"account_input,omitempty"`
	RecipientInput   string `yaml:"recipient_input,omitempty" json:"recipient_input,omitempty"`
	Account          string `yaml:"account,omitempty" json:"account,omitempty"`
	BotName          string `yaml:"bot_name,omitempty" json:"bot_name,omitempty"`
	Socket           string `yaml:"socket,omitempty" json:"socket,omitempty"`
	Data             string `yaml:"data,omitempty" json:"data,omitempty"`
	Executable       string `yaml:"executable,omitempty" json:"executable,omitempty"`
	ExecutableSHA256 string `yaml:"executable_sha256,omitempty" json:"executable_sha256,omitempty"`
	Conversation     string `yaml:"conversation,omitempty" json:"conversation,omitempty"`
}

type ProviderDeclarations map[string]ProviderDeclaration

func (p *ProviderDeclarations) UnmarshalYAML(n *yaml.Node) error {
	result := ProviderDeclarations{}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, child := range n.Content {
			if child.Kind != yaml.ScalarNode || child.Tag != "!!str" || (child.Value != "telegram" && child.Value != "signal") {
				return fmt.Errorf("providers: expected telegram or signal")
			}
			if _, exists := result[child.Value]; exists {
				return fmt.Errorf("providers: duplicate provider")
			}
			result[child.Value] = ProviderDeclaration{}
		}
	case yaml.MappingNode:
		// Node.Decode does not inherit KnownFields. Decode each mapping strictly.
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if key != "telegram" && key != "signal" {
				return fmt.Errorf("providers: unsupported provider name")
			}
			if _, exists := result[key]; exists {
				return fmt.Errorf("providers: duplicate provider")
			}
			var value ProviderDeclaration
			if err := decodeStrictNode(n.Content[i+1], &value); err != nil {
				return fmt.Errorf("providers.%s: %w", key, err)
			}
			result[key] = value
		}
	default:
		return fmt.Errorf("providers: expected list or mapping")
	}
	*p = result
	return nil
}

func decodeStrictNode(n *yaml.Node, out any) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return fmt.Errorf("invalid YAML mapping")
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err = dec.Decode(out); err != nil {
		return fmt.Errorf("invalid configuration fields or types")
	}
	return nil
}

type NotificationRoute struct {
	Primary  string `yaml:"primary" json:"primary"`
	Fallback string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

type ProducerDeclaration struct {
	Required           *bool    `yaml:"required,omitempty" json:"required"`
	Routes             []string `yaml:"routes,omitempty" json:"routes,omitempty"`
	MaxSilenceSeconds  int      `yaml:"max_silence_seconds,omitempty" json:"max_silence_seconds"`
	MaxEvidenceSeconds int      `yaml:"max_evidence_seconds,omitempty" json:"max_evidence_seconds"`
	MaxMessages        int      `yaml:"max_messages,omitempty" json:"max_messages"`
	PolicyHealth       bool     `yaml:"policy_health,omitempty" json:"policy_health"`
	InteractiveContext bool     `yaml:"interactive_context,omitempty" json:"interactive_context"`
	ContextKinds       []string `yaml:"context_kinds,omitempty" json:"context_kinds,omitempty"`
}

type NotificationLimits struct {
	MaxMessages      int   `yaml:"max_messages,omitempty" json:"max_messages"`
	MaxPayloadBytes  int64 `yaml:"max_payload_bytes,omitempty" json:"max_payload_bytes"`
	MaxDatabaseBytes int64 `yaml:"max_database_bytes,omitempty" json:"max_database_bytes"`
	MinFreeBytes     int64 `yaml:"min_free_bytes,omitempty" json:"min_free_bytes"`
	ReceiptAuditDays int   `yaml:"receipt_audit_days,omitempty" json:"receipt_audit_days"`
}

type CommandDeclaration struct {
	Enabled       bool                               `yaml:"enabled,omitempty" json:"enabled"`
	Mode          string                             `yaml:"mode,omitempty" json:"mode"`
	Workers       map[string]string                  `yaml:"workers,omitempty" json:"workers,omitempty"` // alias -> producer
	Conversations map[string]ConversationDeclaration `yaml:"conversations,omitempty" json:"conversations,omitempty"`
	Permissions   []PermissionDeclaration            `yaml:"permissions,omitempty" json:"permissions,omitempty"`
}

type ConversationDeclaration struct {
	Provider string `yaml:"provider" json:"provider"`
	Account  string `yaml:"account" json:"account"`
	NativeID string `yaml:"native_id" json:"native_id"`
	Thread   string `yaml:"thread,omitempty" json:"thread,omitempty"`
}

type PermissionDeclaration struct {
	Actor        string          `yaml:"actor" json:"actor"`
	Conversation string          `yaml:"conversation" json:"conversation"`
	Worker       string          `yaml:"worker" json:"worker"`
	Permissions  []string        `yaml:"permissions" json:"permissions,omitempty"`
	Scope        PermissionScope `yaml:"scope,omitempty" json:"scope"`
}

type PermissionScope struct {
	WholeWorker bool     `yaml:"whole_worker,omitempty" json:"whole_worker,omitempty"`
	Subjects    []string `yaml:"subjects,omitempty" json:"subjects,omitempty"`
}

type LEDDeclaration struct {
	Enabled  bool                             `yaml:"enabled,omitempty" json:"enabled"`
	PowerLED bool                             `yaml:"power_led,omitempty" json:"power_led"`
	Socket   string                           `yaml:"socket,omitempty" json:"socket,omitempty"`
	Daemon   LEDDaemon                        `yaml:"daemon,omitempty" json:"daemon"`
	Bindings map[string]LEDBindingDeclaration `yaml:"bindings,omitempty" json:"bindings,omitempty"`
}

type LEDDaemon struct {
	Device   string `yaml:"device,omitempty" json:"device,omitempty"`
	Mode     string `yaml:"mode,omitempty" json:"mode"`
	Artifact string `yaml:"artifact,omitempty" json:"artifact,omitempty"` // operator-local
	SHA256   string `yaml:"sha256,omitempty" json:"sha256,omitempty"`
}

type LEDBindingDeclaration struct {
	Producer string                         `yaml:"producer" json:"producer"`
	Subjects []string                       `yaml:"subjects" json:"subjects,omitempty"`
	LED      string                         `yaml:"led" json:"led"`
	Styles   map[string]LEDStyleDeclaration `yaml:"styles,omitempty" json:"styles,omitempty"`
}

type LEDStyleDeclaration struct {
	Pattern  string `yaml:"pattern" json:"pattern"`
	Priority int    `yaml:"priority" json:"priority"`
}

func WorkerEnabled(enabled *bool) bool { return enabled == nil || *enabled }

// DesiredFingerprint includes normalized declared policy, never credential bytes
// or generated route revisions. Maps encode deterministically with encoding/json.
func (d DesiredWorkers) Fingerprint() (string, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func TargetPath(value string) bool {
	if strings.HasPrefix(value, "~/") {
		value = "/target-home/" + strings.TrimPrefix(value, "~/")
	}
	return WorkerAbsolutePath(value) && !strings.ContainsAny(value, "$`\\")
}

func validateDesiredPaths(v reflect.Value, prefix string) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return validateDesiredPaths(v.Elem(), prefix)
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		name := strings.Split(v.Type().Field(i).Tag.Get("yaml"), ",")[0]
		if name == "" {
			name = v.Type().Field(i).Name
		}
		full := prefix + "." + name
		if f.Kind() == reflect.String && (strings.HasSuffix(name, "_file") || name == "path" || name == "require_mount" || name == "socket") && f.String() != "" && !TargetPath(f.String()) {
			return fmt.Errorf("%s: expected clean absolute target path or ~/path", full)
		}
		if err := validateDesiredPaths(f, full); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkerPaths(p WorkerPaths) error {
	for _, v := range []string{p.Config, p.State, p.Data, p.Runtime, p.Installation, p.UnitDirectory} {
		if v != "" && !TargetPath(v) {
			return fmt.Errorf("worker paths must be clean absolute target paths or ~/path")
		}
	}
	for _, v := range p.DeniedPaths {
		if !TargetPath(v) {
			return fmt.Errorf("invalid denied_paths target path")
		}
	}
	return nil
}

func normalizeArtifacts(file string, a *WorkerArtifacts) error {
	var err error
	for _, p := range []*string{&a.Catalog, &a.Helper} {
		if *p != "" {
			*p, err = localInventoryPath(file, *p)
			if err != nil {
				return err
			}
		}
	}
	if (a.Helper == "") != (a.HelperSHA256 == "") || a.HelperSHA256 != "" && !digestString(a.HelperSHA256) {
		return fmt.Errorf("artifacts.helper requires exact helper_sha256")
	}
	for _, v := range []string{a.Image, a.RelayImage} {
		if v != "" {
			if err := PinnedWorkerImage(v); err != nil {
				return err
			}
		}
	}
	return nil
}
func digestString(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Reject aliases/merges in v3 rather than silently accepting an ambiguous edit.
// v2 continues to use its historical decoder.
func validateDesiredNode(n *yaml.Node, prefix string) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return fmt.Errorf("%s: YAML anchors and aliases are not supported in version 3", prefix)
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Tag != "!!str" || k.Value == "<<" || seen[k.Value] {
				return fmt.Errorf("%s: duplicate or ambiguous YAML key", prefix)
			}
			seen[k.Value] = true
			if v.Tag == "!!null" && k.Value != "retention" {
				return fmt.Errorf("%s.%s: explicit null is not a value", prefix, k.Value)
			}
			var integer int64
			zeroInteger := v.Tag == "!!int" && v.Decode(&integer) == nil && integer == 0
			if zeroInteger && (strings.HasPrefix(k.Value, "max_") || k.Value == "min_free_bytes" || k.Value == "receipt_audit_days" || k.Value == "keep_last") {
				return fmt.Errorf("%s.%s: must be positive when supplied", prefix, k.Value)
			}
			if err := validateDesiredNode(v, prefix+"."+k.Value); err != nil {
				return err
			}
		}
	} else {
		for _, v := range n.Content {
			if err := validateDesiredNode(v, prefix); err != nil {
				return err
			}
		}
	}
	return nil
}

// MissingInputs identifies declaration-level inputs without contacting targets.
// File contents and target prerequisites are inspected by the coordinator.
func (d DesiredWorkers) MissingInputs() []string {
	var missing []string
	if d.Backup != nil && WorkerEnabled(d.Backup.Enabled) && len(d.Backup.Sources) == 0 {
		missing = append(missing, "backup_worker.sources")
	}
	n := d.Notification
	if n != nil && WorkerEnabled(n.Enabled) {
		if len(n.Providers) == 0 {
			missing = append(missing, "notification_worker.providers")
		}
		if len(n.Providers) > 1 && len(n.Routes) == 0 {
			missing = append(missing, "notification_worker.routes.alerts.primary")
		}
		for name, p := range n.Providers {
			pairs := [][3]string{{"token_file", p.TokenFile, p.TokenInput}, {"chat_id_file", p.ChatIDFile, p.ChatIDInput}}
			if name == "signal" {
				pairs = [][3]string{{"account_file", p.AccountFile, p.AccountInput}, {"recipient_file", p.RecipientFile, p.RecipientInput}}
			}
			if p.Conversation != "" {
				pairs = pairs[:1]
			}
			if name == "telegram" && n.Commands.Enabled && p.Account != "" && p.BotName == "" {
				missing = append(missing, "notification_worker.providers.telegram.bot_name")
			}
			for _, v := range pairs {
				if v[1] == "" && v[2] == "" {
					missing = append(missing, "notification_worker.providers."+name+"."+v[0])
				}
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// Version3Candidate writes a candidate only. It never changes the input or target.
func Version3Candidate(file string, out io.Writer) error {
	inv, err := ReadInventory(file)
	if err != nil {
		return err
	}
	if inv.Version != 2 {
		return fmt.Errorf("migration requires a version 2 inventory")
	}
	var candidate inventoryV3
	candidate.Version = 3
	candidate.Servers = map[string]serverV3{}
	for name, s := range inv.Servers {
		v := serverV3{Host: s.Host, User: s.User, SSHPort: s.SSHPort, SSHKey: s.SSHKey, KnownHostsFile: s.KnownHostsFile, Stack: s.Stack}
		if b := s.BackupWorker; b != nil {
			enabled := true
			v.Backup = &BackupDeclaration{Enabled: &enabled, Sources: b.Sources, Storage: DesiredStorage{Mode: "mount", Path: b.Storage.Path, RequireMount: b.Storage.RequireMount, MinFreeBytes: b.Storage.MinFreeBytes}, Schedule: b.Schedule, Timezone: b.Timezone, Freshness: b.Freshness, MaxRunTime: b.MaxRunTime, MaxSnapshotBytes: b.MaxSnapshotBytes, Timeouts: b.Timeouts, Retention: b.Retention, Monitoring: b.Monitoring, RestoreChecks: b.RestoreChecks, Notifications: DesiredBackupNotifications{Enabled: &enabled, WorkerNotifications: b.Notifications}}
			// Existing installer paths predate XDG resolution. Pin their target-home
			// layout rather than letting a new XDG environment move installed state.
			v.Backup.Paths = WorkerPaths{Config: "~/.config/deploy/backup-worker", State: "~/.local/state/deploy/backup-worker/state", Data: "~/.local/share/deploy/backup-worker"}
		}
		candidate.Servers[name] = v
	}
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	defer enc.Close()
	var document yaml.Node
	if err := document.Encode(candidate); err != nil {
		return err
	}
	omitNullFields(&document)
	return enc.Encode(&document)
}

// Keep explicit retention:null (no pruning) but omit optional legacy pointers.
func omitNullFields(n *yaml.Node) {
	if n.Kind == yaml.MappingNode {
		content := n.Content[:0]
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if value.Tag == "!!null" && key.Value != "retention" {
				continue
			}
			omitNullFields(value)
			content = append(content, key, value)
		}
		n.Content = content
	} else {
		for _, child := range n.Content {
			omitNullFields(child)
		}
	}
}
