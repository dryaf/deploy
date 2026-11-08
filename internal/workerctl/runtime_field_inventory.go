package workerctl

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
)

type RuntimeField struct{ Path, Source string }
type runtimeFieldGroup struct{ prefix, fields, source string }

var runtimeFieldGroups = []runtimeFieldGroup{
	{"backup.", "image relay_image", "YAML backup_worker.artifacts.image / relay_image or the selected pinned catalog"},
	{"backup.", "schema worker_id archive_id config_hash", "Generated schema, reserved worker/archive identities and canonical policy digest; immutable ownership facts"},
	{"backup.paths.", "version home config_home state_home data_home quadlet_directory", "Generated target layout authority; target home/XDG facts and installer layout version"},
	{"backup.paths.", "config runtime state data installation unit_directory denied_paths", "YAML backup_worker.paths overrides resolved against pinned target home/XDG defaults; setup, recovery and runtime share the retained layout"},
	{"backup.qualification.", "*", "Observed qualification records; never generated passing values from desired YAML"},
	{"backup.sources[].", "alias host user port", "YAML backup_worker.sources and the referenced inventory server endpoints"},
	{"backup.sources[].", "key_file known_hosts source_id", "Generated restricted source key/host trust files and enrolled source identity; operator SSH credentials are separate"},
	{"backup.policy.", "sources schedule timezone freshness max_run_time max_snapshot_bytes", "YAML backup_worker fields of the same name, with versioned normalization"},
	{"backup.policy.storage.", "mode path require_mount min_free_bytes", "YAML backup_worker.storage; target-resolved path and explicit home/mount mode"},
	{"backup.policy.timeouts.", "connect inventory snapshot transfer verify shutdown_grace", "YAML backup_worker.timeouts fields of the same name"},
	{"backup.policy.retention.", "keep_last max_age", "YAML backup_worker.retention; null/omitted means no automatic pruning"},
	{"backup.policy.notifications.", "mode", "Generated none/shared/legacy reporting mode from declared enabled state and worker/provider selection"},
	{"backup.policy.notifications.", "primary fallback failure repeat_after recovery daily_at", "YAML backup_worker.notifications; shared route ownership is resolved separately"},
	{"backup.policy.notifications.signal.", "account_file recipient_file", "YAML backup_worker.notifications.signal target credential references"},
	{"backup.policy.notifications.telegram.", "token_file chat_id_file", "YAML backup_worker.notifications.telegram target credential references"},
	{"backup.policy.monitoring.", "backup_ping_url_file daily_delivery_ping_url_file", "YAML backup_worker.monitoring target endpoint references"},
	{"backup.policy.restore_checks[].", "source instance image command timeout", "YAML backup_worker.restore_checks entries, preserving source/image/argument restrictions"},
	{"notification.", "version bootstrap", "Generated runtime version and verification-only empty enrollment state; bootstrap never invents a producer"},
	{"notification.", "led_socket", "YAML notification_worker.status_leds.socket or verified managed daemon default"},
	{"notification.led_endpoint.", "version socket owner_uid peer_uid", "Generated endpoint version, resolved socket and observed file/kernel peer ownership pins"},
	{"notification.worker.", "max_messages max_payload_bytes max_database_bytes min_free_bytes receipt_audit_days", "YAML notification_worker.limits fields of the same name"},
	{"notification.worker.producers[].producer.", "id epoch", "Generated real specialist reservation and retained enrollment; YAML uses producer aliases"},
	{"notification.worker.producers[].", "required routes max_silence_seconds max_evidence_seconds max_messages policy_health interactive_context context_kinds", "YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence"},
	{"notification.worker.routes.*.", "revision", "Generated stable provider-destination-bound route revision from unresolved declaration input"},
	{"notification.worker.routes.*.", "primary fallback", "YAML notification_worker.routes entry of the same name"},
	{"notification.worker.led_bindings[].", "id", "YAML notification_worker.status_leds.bindings mapping key, normalized deterministically"},
	{"notification.worker.led_bindings[].producer.", "id epoch", "Generated real producer reservation selected by the binding's declared producer alias"},
	{"notification.worker.led_bindings[].", "subjects led allow_power", "YAML binding subject selectors/output and explicit status_leds.power_led permission"},
	{"notification.worker.led_bindings[].styles.*.", "pattern priority", "YAML binding styles or the versioned default style bands"},
	{"notification.worker.interaction.", "revision", "Generated stable command policy revision; new rights require admission"},
	{"notification.worker.interaction.workers[].", "alias", "YAML notification_worker.commands.workers alias"},
	{"notification.worker.interaction.workers[].producer.", "id epoch", "Generated reserved producer identity selected by commands.workers alias mapping"},
	{"notification.worker.interaction.workers[].", "generation", "Generated retained specialist generation; not a caller-selected YAML token"},
	{"notification.worker.interaction.workers[].commands.*.", "id version permission effect confirm scoped target_required help", "Generated from validated specialist command descriptors, filtered by YAML command mode/permissions"},
	{"notification.worker.interaction.workers[].commands.*.arguments.*.", "kind required max", "Generated validated specialist argument contract; YAML cannot invent executable descriptors"},
	{"notification.worker.interaction.destinations[].", "id provider account native_id thread", "YAML notification_worker.commands.conversations mapping key and declared destination fields"},
	{"notification.worker.interaction.destinations[].", "generation", "Generated stable conversation enrollment revision; audience changes require migration"},
	{"notification.worker.interaction.permissions[].", "actor conversation worker permissions", "YAML notification_worker.commands.permissions fields, checked against descriptors"},
	{"notification.worker.interaction.permissions[].scope.", "whole_worker subjects", "YAML command permission scope, normalized and validated"},
	{"notification.providers.*.", "kind", "YAML notification_worker.providers mapping key or list shorthand"},
	{"notification.providers.*.", "token_file chat_file", "YAML Telegram target references or private target files provisioned from operator-local inputs"},
	{"notification.providers.*.", "socket", "YAML Signal socket override or generated owned native service endpoint"},
	{"notification.providers.*.", "destination", "Generated fingerprint of the actual provider identity and destination; never plaintext recipient data"},
	{"notification.providers.*.signal_delivery.", "service_hash account_hash recipient_hash", "Generated immutable native delivery approval bound to the original service and exact private input bytes; never new account/link authority"},
	{"notification.providers.*.signal_delivery.", "account_file recipient_file", "YAML Signal target references selected by a same-account recipient/reference migration; original setup references remain retained"},
	{"notification.providers.*.relay_identity.", "worker recovery", "Generated canonical relay/inbox identity from verified service ownership"},
	{"notification.providers.*.conversation.", "id provider account native_id thread", "YAML referenced command conversation; fixed destination references are excluded"},
	{"notification.providers.*.conversation.", "generation", "Generated conversation enrollment revision, shared with the admitted interaction policy"},
	{"notification.interactive_telegram.", "account token_file bot_name", "YAML Telegram account/token reference/bot_name, bound to the declared conversations"},
	{"notification.interactive_signal.", "account socket", "YAML Signal logical account and declared or service-derived endpoint"},
	{"notification.interactive_signal.inbox_identity.", "worker recovery", "Generated canonical Signal inbox identity; adoption/recovery preserves its durable authority"},
}

// Every raw runtime setting is either mapped to desired YAML or explicitly
// explained as a generated fact. Unknown additions fail the regular doc check.
func WorkerRuntimeFieldInventory() ([]RuntimeField, error) {
	declared := map[string]RuntimeField{}
	for _, group := range runtimeFieldGroups {
		for _, name := range strings.Fields(group.fields) {
			path := group.prefix + name
			if _, ok := declared[path]; ok {
				return nil, fmt.Errorf("duplicate runtime inventory field %s", path)
			}
			declared[path] = RuntimeField{path, group.source}
		}
	}
	var result []RuntimeField
	for _, root := range []struct {
		prefix string
		t      reflect.Type
	}{{"backup.", reflect.TypeOf(backup.WorkerInstallation{})}, {"notification.", reflect.TypeOf(install.Runtime{})}} {
		fields, err := taggedLeafFields(root.t, "json")
		if err != nil {
			return nil, err
		}
		for _, field := range fields {
			path := root.prefix + field
			entry, ok := declared[path]
			if !ok {
				return nil, fmt.Errorf("runtime field lacks YAML/generated-state mapping: %s", path)
			}
			result = append(result, entry)
			delete(declared, path)
		}
	}
	if len(declared) > 0 {
		var fields []string
		for path := range declared {
			fields = append(fields, path)
		}
		sort.Strings(fields)
		return nil, fmt.Errorf("runtime inventory includes unknown fields: %s", strings.Join(fields, ", "))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}
