package workerctl

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

// WorkerField describes the public YAML contract, including incomplete
// lifecycles. It is documentation metadata, never an alternative policy source.
type WorkerField struct {
	Path       string
	Runtime    string
	Transition string
	Execution  string
}
type fieldGroup struct{ prefix, fields, runtime, execution string }

// Explicit leaf names intentionally make a newly added public setting fail the
// inventory check until its compilation and lifecycle behavior are accounted for.
var workerFieldGroups = []fieldGroup{
	{"backup_worker.", "retire_sources.*", "retained source revocation intent and archive incident disposition", "Explicit alias/reason for removal or combined new-alias replacement; retains archives, keys, fault dispositions and unchanged shared producer/gateway ownership"},
	{"backup_worker.", "enabled", "setup activity intent and execution fence", "Standalone activity plus shared capture/report fencing through either worker; shared restoration requires fresh gateway qualification, separate backup qualification and capture activation; original ownership retained"},
	{"backup_worker.", "sources", "WorkerInstallation.Sources and restricted source enrollment", "Initial setup, additions and retained-identity endpoint moves (both also after completed shared policy changes) and explicit retire_sources removal, including after completed shared policy changes; combined new-alias replacement, also after completed shared policy changes, retains enrollment/revocation across rollback with shared ownership unchanged; retired alias/identity reuse pending"},
	{"backup_worker.", "schedule timezone max_run_time max_snapshot_bytes", "WorkerInstallation.Policy", "Standalone and unchanged-gateway shared updates, including after completed production notification policy changes, retain the original reporting handoff and stage capture; change derived freshness separately before changing schedule"},
	{"backup_worker.", "freshness", "WorkerInstallation.Policy and default gateway producer evidence deadline", "Standalone and unchanged-gateway shared update; derived freshness-only changes coordinate both runtimes and archive checkpoint through either worker before activation or on unrecovered command-free production gateways; production updates stage capture and pause delivery for qualification, preserving ownership and unchanged Signal/LED state; unrecovered Telegram-only command gateways support derived freshness after disabling intake; retain receiver/results and stage capture separately from notification qualification; recovered and Signal command migrations pending"},
	{"backup_worker.storage.", "mode path require_mount", "WorkerInstallation.Policy.Storage and archive ownership", "New home/mount setup; existing path/filesystem changes require migration"},
	{"backup_worker.storage.", "min_free_bytes", "WorkerInstallation.Policy.Storage.MinFreeBytes", "Validated setup/update; archive capacity guards remain"},
	{"backup_worker.timeouts.", "connect inventory snapshot transfer verify shutdown_grace", "WorkerInstallation.Policy.Timeouts", "Validated setup and journaled update"},
	{"backup_worker.retention.", "keep_last max_age", "WorkerInstallation.Policy.Retention", "Explicit setup/update; omitted/null retention disables pruning"},
	{"backup_worker.notifications.", "enabled worker", "generated reporting mode and producer reservation", "Standalone or local shared initial setup; shared notifications.enabled uses coordinated capture/report activity fences, retaining the original owner; changing worker requires separate handoff"},
	{"backup_worker.notifications.", "route", "retained backup report route and gateway producer permissions", "Select a declared/applied route through either worker before activation or on unrecovered production gateways; disable admitted Telegram command intake first; settle frozen backup exports; default producer permissions migrate with settled delivery messages and fresh qualification; an explicitly allowed route retains runtime and qualification; original handoff, receiver/results and frozen audiences remain; recovered and Signal command variants pending"},
	{"backup_worker.notifications.", "primary fallback", "WorkerInstallation.Policy.Notifications", "Direct Telegram setup; direct Signal and provider/route migration need lifecycle work"},
	{"backup_worker.notifications.", "failure repeat_after recovery daily_at", "WorkerInstallation.Policy.Notifications", "Standalone and unchanged-gateway shared reporting policy update"},
	{"backup_worker.notifications.signal.", "account_file recipient_file", "WorkerInstallation.Policy.Notifications.Signal", "Target references; direct Signal short setup remains unsupported"},
	{"backup_worker.notifications.telegram.", "token_file", "WorkerInstallation.Policy.Notifications.Telegram.TokenFile", "Private target reference; standalone same-bot rotation/reference moves pin old and new token/chat files through update/rollback; qualification and explicit schedule reactivation follow"},
	{"backup_worker.notifications.telegram.", "chat_id_file", "WorkerInstallation.Policy.Notifications.Telegram.ChatIDFile", "Private target reference; standalone same-bot numeric destination changes require accepted outbox history and no reserved delivery, retain both file sets through rollback, and stage qualification; shared production migration pending"},
	{"backup_worker.monitoring.", "backup_ping_url_file daily_delivery_ping_url_file", "WorkerInstallation.Policy.Monitoring", "Private target HTTPS references; updates validate distinct checks, preserve shared ownership and return schedules to qualification without sending heartbeats"},
	{"backup_worker.restore_checks[].", "source instance image command timeout", "WorkerInstallation.Policy.RestoreChecks", "Validated source/instance, pinned image, argument array and timeout; setup/update"},
	{"backup_worker.paths.", "config state data runtime installation unit_directory denied_paths", "target path resolver and versioned backup layout pins", "New custom home/XDG setup, native exclusions and pinned recovery; custom unit directory must be discoverable by the target user manager; existing path migration pending"},
	{"backup_worker.artifacts.", "catalog helper helper_sha256", "artifact resolver and SetupDeclaration", "Local catalog/static helper selection; compatible standalone updates and fresh shared backup helper upgrades retain source helpers and original reporting ownership, including completed command receivers with unchanged capabilities, SQL binding and results; shared gateway/source helper and receiver capability upgrades remain separate"},
	{"backup_worker.artifacts.", "image relay_image", "pinned container/relay images", "Backup image required for staging; pinned shared backup-image changes retain the helper, sources and reporting handoff, including after completed shared policy/source changes, and stage capture for qualification; relay image used only by applicable legacy reporting"},
	{"notification_worker.", "enabled", "managed activity transaction", "Unbound activity plus unrecovered shared disable/re-enable; dependent capture and reporting stop before gateway pause/removal; retains queues, owner, native Signal and LED history; admitted intake must first be disabled; restoration requires fresh qualification"},
	{"notification_worker.providers.*.", "token_file chat_id_file", "provider.Profile Telegram credentials", "Telegram target references; unbound and shared verification rotation/destination changes, including unadmitted commands for the same bot, retain old/new private files and any unchanged owned native Signal service; an unused Telegram profile can be added/removed beside unchanged owned Signal before activation or on an unrecovered command-free production gateway with a settled queue and fresh qualification; verification shared changes retain unchanged LED policy and require a settled queue; unrecovered command-free production gateways, including unchanged owned Signal, can rotate same-destination private references with queued work retained, or change Telegram chat/bot with a settled queue; both pause for fresh qualification and retain reporting ownership and old/new credential exclusions; unrecovered Telegram-only command gateways support same-bot reference rotation and fixed alert destination changes after separate disable/apply, retaining receiver, cursor, saved results/replies and LED ownership; fresh qualification and explicit intake re-enable follow; new command bots/audiences, account/name declarations, profile retirement and recovered variants require their separate workflows"},
	{"notification_worker.providers.*.", "account_file recipient_file", "fresh Signal selector and fixed destination", "Owned direct Signal target references can change before shared activation or on unrecovered command-free production gateways; preserve the same account bytes, native service, inbox, link and executables; recipient approval pins both file sets, reporting excludes their history, effective production changes pause for fresh qualification; new accounts, native intake, service/artifact replacement and recovered variants require their separate lifecycles"},
	{"notification_worker.providers.*.", "token_input chat_id_input account_input recipient_input", "private framed credential provisioning", "Operator-local input references; initial setup, never plaintext YAML or argv"},
	{"notification_worker.providers.*.", "account bot_name conversation", "provider/interaction account and conversation profiles", "Initial Telegram account/bot_name matched to the supplied token; fixed alerts retained separately; interactive alert destinations and native Signal intake pending"},
	{"notification_worker.providers.*.", "socket data executable executable_sha256", "fresh Signal service/account/native artifact paths", "Fresh Signal setup and coordinator adoption of a proven owned native service; legacy account takeover, relink and replacement pending"},
	{"notification_worker.routes.*.", "primary fallback", "worker.Config.Routes and provider-bound revisions", "Initial/unbound, shared verification and effective production route-only changes on unrecovered command-free gateways; settle queued work first, retain provider/producer permissions, reporting and unchanged Signal/LED state, then qualify; omitted single-provider route defaults normalize without a change; unrecovered Telegram-only command gateways support route-only changes after disabling intake, retaining receiver restrictions, saved results/replies and LED state with settled delivery messages and fresh qualification; recovery, Signal command and provider replacement migrations pending"},
	{"notification_worker.producers.*.", "max_silence_seconds max_evidence_seconds", "worker.Config.Producers timing thresholds", "Initial local backup policy and shared verification updates; effective production timing changes on unrecovered command-free gateways retain queued work, original observations and unchanged Signal/LED state; fresh qualification required; equivalent explicit/derived producer defaults retain the runtime and qualification; unrecovered Telegram-only gateways with admitted backup commands can disable intake and update existing producer policy or explicit/derived declarations, retaining receiver, original observations, queued work, reporting and LED state; effective changes require qualification and explicit re-enable; recovery and Signal command migrations pending"},
	{"notification_worker.producers.*.", "required routes max_messages policy_health interactive_context context_kinds", "worker.Config.Producers enrollment", "Existing local backup policy supports verification and effective production changes on unrecovered command-free gateways; retains ownership, observations, reporting and unchanged Signal/LED state; access changes require a settled queue and fresh qualification; equivalent explicit/derived producer defaults retain the runtime and qualification; unrecovered Telegram-only gateways with admitted backup commands support these same-identity policy updates after disabling intake; queued commands/results/replies and reduced receiver rights remain; effective changes require qualification, equivalent explicit/derived declarations preserve runtime and qualification; recovery, Signal command and additional-identity migrations pending"},
	{"notification_worker.limits.", "max_messages max_payload_bytes max_database_bytes min_free_bytes receipt_audit_days", "worker.Config storage and receipt budgets", "Initial setup, unbound and shared verification updates retain LED history; effective production budget changes on unrecovered command-free local shared gateways coordinate a delivery pause, retained queue/owner/reporting and fresh qualification, including unchanged owned Signal and LED settings; unrecovered Telegram gateways with retained backup commands can disable intake, apply limits-only changes, qualify and apply while retaining receiver, permissions, queues/results and LED ownership, then explicitly re-enable commands; recovered variants remain pending"},
	{"notification_worker.commands.", "enabled", "coordinated command admission and backup declaration scope", "Disabled by default; notification-first command drafts can change before producer reservation without restarting; verification commands can be disabled/restored with exact retained policy and no intake; admitted local Telegram backup commands can be disabled/re-enabled with other settings unchanged, retaining reporting and results; re-enable requires valid delivery qualification; unrecovered Telegram command gateways can disable intake, qualify and apply to renew delivery while retaining receiver, results and permission history, then explicitly re-enable; initial enrollment on an existing staged Telegram gateway supported; activated command-free enrollment pending"},
	{"notification_worker.commands.", "mode workers.*", "interaction.Config and command admission mode", "New or already enrolled verification gateways can add local backup read or mutation commands using real reservations, stage a versioned receiver and separately admit qualified intake; alias renaming and mode changes within the original receiver capabilities supported while disabled; receiver capability upgrades pending"},
	{"notification_worker.commands.conversations.*.", "provider account native_id thread", "interaction.Config.Conversations and provider profiles", "Initial Telegram conversations and threads use the retained account; command admission follows completed receiver setup; conversation retirement supported while disabled; new or redirected Telegram audiences supported before activation; production changes pending"},
	{"notification_worker.commands.permissions[].", "actor conversation worker permissions", "interaction.Config.Permissions", "Initial read and explicit incident.manage permissions checked against approved backup descriptors and retained before admission; mutation requires mutation mode; permission reductions and explicit restoration within original receiver authority supported while disabled; new authority can be declared before activation; production expansion pending"},
	{"notification_worker.commands.permissions[].scope.", "whole_worker subjects", "interaction permission scope", "Initial read/incident scopes compiled, sorted and enforced by the specialist receiver; scope reductions and explicit restoration within original receiver authority supported while disabled; new authority can be declared before activation; production expansion pending"},
	{"notification_worker.status_leds.", "enabled", "managed LED output disposition", "Initial setup and verification toggles; qualified unrecovered shared delivery toggles preserve bindings, incidents and reporting; command-free qualification renewals retain output history; re-enable requires originally qualified output"},
	{"notification_worker.status_leds.", "power_led", "worker.LEDBinding.AllowPower and host selection", "Initial managed provisioning with explicit opt-in and discovered PWR device; changing an installed daemon requires migration"},
	{"notification_worker.status_leds.", "socket", "managed runtime LED endpoint", "Managed or explicitly existing daemon; later endpoint migration pending"},
	{"notification_worker.status_leds.daemon.", "mode device artifact sha256", "managed daemon artifact and observed device", "Managed initial setup/removal and existing-daemon verification; replacement pending"},
	{"notification_worker.status_leds.bindings.*.", "producer subjects led", "worker.Config.LEDBindings", "Initial local backup mapping and later verification-mode binding edits coordinate the staged backup scope; verification output toggles retain bindings; production transitions pending"},
	{"notification_worker.status_leds.bindings.*.styles.*.", "pattern priority", "worker.LEDBinding styles", "Initial and later verification-mode binding edits validate supported names/priority bands; production mapping changes pending"},
	{"notification_worker.paths.", "config state data runtime installation unit_directory denied_paths", "target resolver and managed service paths/exclusions", "New custom home/XDG setup; existing path migration/import pending"},
	{"notification_worker.artifacts.", "catalog helper helper_sha256", "artifact resolver and managed executable pins", "Initial setup and unbound compatible helper update; bound/shared upgrade pending"},
	{"notification_worker.artifacts.", "image relay_image", "not applicable to the native notification worker", "Rejected; container image policy belongs to backup_worker.artifacts"},
}

// WorkerFieldInventory verifies exact coverage before returning documentation.
// '*' names a declared mapping entry; '[]' names each structured sequence item.
func WorkerFieldInventory() ([]WorkerField, error) {
	public, err := yamlLeafFields(reflect.TypeOf(config.DesiredWorkers{}))
	if err != nil {
		return nil, err
	}
	declared := map[string]WorkerField{}
	for _, group := range workerFieldGroups {
		for _, name := range strings.Fields(group.fields) {
			path := group.prefix + name
			if _, exists := declared[path]; exists {
				return nil, fmt.Errorf("duplicate field inventory entry %s", path)
			}
			declared[path] = WorkerField{path, group.runtime, changeClass(path), group.execution}
		}
	}
	var result []WorkerField
	for _, path := range public {
		field, ok := declared[path]
		if !ok {
			return nil, fmt.Errorf("public worker field lacks runtime/lifecycle mapping: %s", path)
		}
		result = append(result, field)
		delete(declared, path)
	}
	if len(declared) != 0 {
		var names []string
		for name := range declared {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("field inventory includes unknown YAML fields: %s", strings.Join(names, ", "))
	}
	return result, nil
}

func yamlLeafFields(root reflect.Type) ([]string, error) {
	return taggedLeafFields(root, "yaml")
}

func taggedLeafFields(root reflect.Type, tagName string) ([]string, error) {
	var result []string
	var walk func(reflect.Type, string) error
	walk = func(t reflect.Type, prefix string) error {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if !field.IsExported() {
					continue
				}
				tag := field.Tag.Get(tagName)
				name := strings.Split(tag, ",")[0]
				if name == "-" {
					continue
				}
				if strings.Contains(tag, ",inline") || tagName == "json" && field.Anonymous && name == "" {
					if err := walk(field.Type, prefix); err != nil {
						return err
					}
					continue
				}
				if name == "" {
					return fmt.Errorf("field %s.%s lacks explicit %s name", prefix, field.Name, tagName)
				}
				path := name
				if prefix != "" {
					path = prefix + "." + name
				}
				if err := walk(field.Type, path); err != nil {
					return err
				}
			}
		case reflect.Map:
			return walk(t.Elem(), prefix+".*")
		case reflect.Slice, reflect.Array:
			elem := t.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct {
				return walk(elem, prefix+"[]")
			}
			result = append(result, prefix)
		default:
			result = append(result, prefix)
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	sort.Strings(result)
	return result, nil
}

func WorkerFieldReference() (string, error) {
	fields, err := WorkerFieldInventory()
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString("<!-- worker-field-inventory:start -->\n\n")
	out.WriteString("Checked against the version-3 YAML types. `*` names a mapping entry and `[]` a structured list item. Provider-specific fields apply only to that provider. This table distinguishes compilation from currently supported lifecycle operations.\n\n")
	out.WriteString("| YAML field | Runtime or lifecycle target | Change class | Execution coverage |\n| --- | --- | --- | --- |\n")
	for _, f := range fields {
		fmt.Fprintf(&out, "| `%s` | %s | `%s` | %s |\n", f.Path, f.Runtime, f.Transition, f.Execution)
	}
	runtimeFields, err := WorkerRuntimeFieldInventory()
	if err != nil {
		return "", err
	}
	out.WriteString("\nThe following runtime leaves are checked against the backup installation and notification runtime types. YAML-owned entries identify their input; generated entries explain why they are not independent YAML settings. Provider account databases, queues, ownership receipts and recovery history remain runtime stores outside these configuration documents.\n\n")
	out.WriteString("| Runtime field | Authority and derivation |\n| --- | --- |\n")
	for _, f := range runtimeFields {
		fmt.Fprintf(&out, "| `%s` | %s |\n", f.Path, f.Source)
	}
	out.WriteString("\n<!-- worker-field-inventory:end -->\n")
	return out.String(), nil
}
