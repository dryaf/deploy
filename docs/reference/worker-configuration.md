# Version 3 worker configuration reference

This reference describes the desired-policy parser, runtime compiler and planning
tools. Telegram and fresh direct-alert Signal unbound gateway installation are
connected to short apply and resume. Standalone backup apply/resume compiles policy and stages the existing
installer with matching image/helper artifacts. Local shared declarations reserve
and enroll the real backup producer before staging backup with a legacy-sender
fence. Qualified gateway admission and fresh shared report connection are implemented.
Guided notification/backup qualification records supplied observations. Short backup
activation checks current evidence and a recent successful backup before enabling
schedules. Other provisioning and migration flows are still in progress;
accepting or compiling a field does not yet mean a short apply can deploy it. Follow [active usability plan](../plans/worker-usability.md) for that milestone.
Executable parser examples are in [examples/workers](../workers/README.md).

`server.yaml` owns selected behavior. Secret values stay in private files, and
identities, producer epochs, route revisions, queues, receipts and recovery records
remain generated runtime facts. Application database backup opt-in stays in the
application declaration. The worker selects source server aliases from inventory.

## Inventory and precedence

Use `version: 3` and a nonempty `servers` mapping. Each server requires `host` and
`user`; `ssh_port` defaults to 22. `ssh_key` and `ssh_known_hosts` resolve on the
operator machine relative to the inventory, with `~/...` support. Existing `stack`
configuration remains available. A worker requires a rootless user.

A present worker block defaults to enabled. An absent block makes apply/resume,
plan/status and uninstall perform a read-only presence review. Retained files or
a loaded service yield `removal_review`; restore the previous block, including
custom paths, before explicit domain removal. Empty default paths with a confirmed
absent service yield `not_requested`. Unknown service state stays unverified.
Omission never creates credentials or changes worker activity.

`enabled: false` disables an unbound
notification gateway without deleting history; changing only this setting back
to `true` restores verification with retained identity. It leaves any native
Signal account service running; explicit uninstall removes that service too.
For a standalone backup, changing only `backup_worker.enabled` to `false` fences
execution and disables/stops owned units while retaining the archive, keys and
history. Re-enabling returns to staged mode; qualification, a recent successful
backup and explicit `activate` remain required before schedules run. Activity
apply/resume does not enroll sources or require a new image. Shared backup and
bound/shared gateway activity changes remain pending implementation.
Explicit YAML policy wins over versioned defaults;
short-command flags select operations and presentation, not hidden policy.

When both workers are enabled on a server, backup reporting defaults to that local
notification worker and its `alerts` route. An explicit reference to an absent or
disabled worker fails validation. Cross-server gateway transport is unsupported.
Backup sources must refer to distinct rootless SSH accounts, separate from the
worker account.

Version-2 inventories retain their previous normalization and mount requirements.
`deploy server migrate --to-version 3` writes a candidate retaining old effective
values, provider references and home-based installation paths. It does not modify
or deploy the original. Runtime configuration JSON is not a public escape hatch.

## Backup worker fields

| YAML field | Default or contract |
| --- | --- |
| `enabled` | `true` when the block is present |
| `sources` | Explicit server aliases; missing selection is a guided input |
| `storage.mode` | `home`, or `mount` when `require_mount` is supplied |
| `storage.path` | Resolved archive path below the target worker data directory |
| `storage.require_mount` | Required in mount mode; never falls back to home storage |
| `storage.min_free_bytes` | 1 GiB |
| `schedule` | `hourly`; also supports `daily` and `weekly` |
| `timezone` | `UTC` for v3; migration retains the v2 effective timezone |
| `freshness` | 2h hourly, 26h daily, 170h weekly; explicit values must cover the schedule plus execution delay |
| `max_run_time` | 40m, subject to the existing schedule budget |
| `max_snapshot_bytes` | 10 GiB; existing 4096-byte to 1-TiB bounds |
| `timeouts` | `connect: 10s`, `inventory: 30s`, `snapshot: 5m`, `transfer: 10m`, `verify: 5m`, `shutdown_grace: 10s` |
| `retention` | Omitted/null disables automatic pruning; explicit nonempty block supports `keep_last` and `max_age` |
| `notifications.worker`, `notifications.route` | Shared local gateway alias and route; mutually exclusive with legacy provider selection |
| `notifications.enabled` | Reporting policy switch; does not erase outstanding receipts |
| `notifications.failure`, `repeat_after`, `recovery`, `daily_at` | `immediate`, `6h`, `true`, `09:00` |
| `notifications.primary`, `fallback`, `signal`, `telegram` | Legacy profile preserved for migration; provider file references retain existing restrictions |
| `monitoring.backup_ping_url_file`, `daily_delivery_ping_url_file` | Separate target files; no inferred external monitor |
| `restore_checks` | Existing source, instance, digest-pinned image, argument-array command and timeout contract |
| `paths`, `artifacts` | Common fields described below |

Home archive mode now has a runtime storage branch independent of mountpoint
checks. It retains archive identity/filesystem checks, free-space guards and the
existing local ext4 requirement. The target home is the creation root for default
archives. A customized outside-home archive requires an existing owned parent
that is not writable by others. External mount mode keeps its exact mount guard.
Short backup apply/resume compiles and invokes this policy for standalone workers
with reporting disabled or the direct Telegram profile. Source trust and helper
artifacts are verified before mutation. Generated runtime reporting mode and the
archive's matching reporting pin are not additional YAML settings. No-reporting
installations retain incident history, create no delivery queue, and require only
the applicable qualification evidence. Local shared declarations use a separate
generated shared mode, retain pending events and refuse legacy sending. Schedule
activation is available after the shared report service is connected and backup
qualification passes. Changing reporting ownership and custom config/state/XDG
installation layouts still require further coordinator work. Monitoring references are rejected in no-reporting mode because
that mode does not run a delivery/heartbeat service.

## Notification provider fields

`providers: [telegram]` and `providers: {telegram: {}}` normalize identically.
Supported providers are `telegram` and `signal`. One provider supplies `alerts`
automatically. With both providers, configure `routes.<name>.primary` and optional
`fallback`; guided configuration offers a persisted primary/fallback choice.

| Provider | Fields |
| --- | --- |
| Telegram | `token_file`, `chat_id_file`; operator-local alternatives `token_input`, `chat_id_input`; logical `account`, `bot_name`, `conversation` |
| Signal | `account_file`, `recipient_file`; operator-local alternatives `account_input`, `recipient_input`; logical `account`, `conversation`, target `socket`, `data`, `executable`, `executable_sha256` |

Each `*_input` is mutually exclusive with its corresponding target `*_file`.
Input paths are operator-local and inventory-relative. Target file paths must be
clean absolute paths or `~/...`. Signal executable overrides require an exact
SHA256. Provider-specific fields cannot be mixed between Telegram and Signal.
Logical account identity is separate from account selectors and private account
database contents. Telegram admission uses exact recorded qualification on reapply;
a short qualification wizard collects operator observations. Fresh Signal linking,
private artifact transfer and supervised service setup are connected to the short
command; owned native coordinator adoption is supported, while legacy account adoption and relinking remain unfinished. See the
[Signal guide](../workers/signal.md).

## Notification enrollment and commands

| YAML field | Contract |
| --- | --- |
| `enabled` | `true` for a present worker block |
| `routes.<name>` | Configured primary and optional distinct fallback provider |
| `producers.<name>.required` | Defaults to `true`; identity is resolved from the actual specialist |
| `producers.<name>.routes` | Defaults to `alerts` when available |
| `producers.<name>.max_silence_seconds` | 120 by default; 10..86400 |
| `producers.<name>.max_evidence_seconds` | 7200 by default; 1..2678400 |
| `producers.<name>.max_messages` | 1000 by default; 1..10000 |
| `producers.<name>.policy_health` | Explicit domain health capability |
| `producers.<name>.interactive_context`, `context_kinds` | Context kinds require explicit capability; at most 16 kinds |
| `limits.max_messages` | 10000 |
| `limits.max_payload_bytes` | 64 MiB aggregate payload |
| `limits.max_database_bytes` | 512 MiB |
| `limits.min_free_bytes` | 128 MiB |
| `limits.receipt_audit_days` | 90; supported range 90..3650 |
| `commands.enabled` | `false` |
| `commands.mode` | `read` by default; `mutation` allows explicitly authorized incident decisions on a new installation, after qualification and separate admission |
| `commands.workers` | User-facing alias to declared producer name |
| `commands.conversations.<name>` | `provider`, logical `account`, `native_id`, optional `thread` |
| `commands.permissions` | List of `actor`, `conversation`, `worker`, `permissions`, and explicit `scope` |
| Permission `scope` | Either `whole_worker: true` or a nonempty unique `subjects` list |

Command descriptors and supported command arguments come from the specialist's
contract. YAML grants scope and permissions; it cannot define arbitrary commands.
Initial setup and existing unactivated Telegram gateways support read commands
and optional `incident.manage`
permissions in `mutation` mode for one local shared backup alias. Actors,
conversations and scopes are explicit; fixed alerts retain a separate destination.
Receiver setup precedes command admission; actual qualification is required.
A notification-first gateway waiting for producers can retain revised command
declarations without a restart or identity reservation. This path ends when
backup setup reserves its producer; complete setup before using enrolled updates.
Before delivery activation, an existing command declaration can change actors,
permissions, scopes, aliases, conversations/topics and read/mutation mode while
retaining the bot, backup identity and other settings. It reserves policy only;
qualification and receiver admission use the revised declaration.
Before activation, `commands.enabled` can also disable and restore the exact
retained command policy in separate applies; aliases stay reserved and intake
remains unadmitted. Keep permissions and other settings unchanged during toggles.
Already admitted Telegram commands support YAML disable/re-enable, permission
and subject reductions, alias renaming, conversation retirement and mode changes
within the original receiver's capabilities. Change policy while intake is
disabled; explicit permission restoration is supported within the original
receiver authority, with aliases and audiences unchanged. Re-enable separately
with current qualification. New permission authority,
new/redirected audiences, receiver upgrades, native Signal intake and interactive
alert destinations still need their migration workflows. See
[command setup](../workers/commands.md).
Producer policy defaults above are explicit declaration defaults. Automatic local
backup enrollment uses its real reservation; derived evidence limits follow the
declared backup freshness.

## Status LEDs

Use `notification_worker.status_leds.enabled`, default `false`.
`raspi_leds_status` is not an alias. Output-only toggles also work on unrecovered
production gateways with local shared backups and valid delivery qualification.
Re-enabling requires originally qualified output permission. Short qualification
renewals without command history retain that permission even while output is
disabled; renewal does not grant permission that was never admitted. Keep
bindings, the daemon and other policy unchanged; see [LED setup](../workers/leds.md).

| YAML field under `status_leds` | Contract |
| --- | --- |
| `power_led` | Explicit opt-in; default `false` |
| `socket` | Target socket override; required with an existing daemon |
| `daemon.mode` | `managed` or `existing`; defaults to `managed` |
| `daemon.artifact`, `daemon.sha256` | Optional operator-local pinned daemon artifact |
| `bindings.<name>.producer` | Specialist name whose identity must be resolved |
| `bindings.<name>.subjects` | Unique subject selectors, or a sole `*` |
| `bindings.<name>.led` | `green`, or `power` with opt-in |
| `bindings.<name>.styles.<condition>` | `pattern` and bounded `priority` |

Priority bands are baseline 0..9, ok 10..39, paused 40..59, warning 60..79,
unknown 80..89 and critical 90..100. At most 32 mappings are accepted. These are
policy inputs. Short commands provision the managed ACT daemon through a reviewed
administrator prerequisite, or inspect an explicitly existing daemon. Initial local
backup bindings wait for a real producer reservation and then use a separate
mapping transition before qualification. Verification mapping changes and
output-only toggles preserve incidents and ownership. Qualified production output
toggles also coordinate the shared backup policy; production mapping changes and
daemon replacement remain pending. Initial
managed setup supports `power_led: true` through the same administrator review,
with a separately discovered, unambiguous PWR device; default setup grants only ACT
access. Changing this selection on an existing installation requires migration.

## Paths and artifacts

| Common `paths` field | New-install resolution |
| --- | --- |
| `config` | Target XDG config base plus `deploy/<worker>` |
| `state` | Target XDG state base plus `deploy/<worker>/state` |
| `data` | Target XDG data base plus `deploy/<worker>` |
| `runtime` | `<config>/runtime.json` |
| `installation` | Notification: `<data>/installation`; backup: target `~/.local/lib/deploy/backup-worker` |
| `unit_directory` | Target XDG config base plus `systemd/user` |
| `denied_paths` | Target paths excluded from native service sandboxes; required operational paths cannot be hidden |

XDG bases fall back to target `~/.config`, `~/.local/state` and `~/.local/share`.
Do not substitute the operator's home. Path validation rejects arbitrary shell
expansion, ambiguous relative target paths, overlapping operational stores and
notification socket paths exceeding the Unix limit. Existing-installation path
adoption and path migration remain separate work.

New backup installations support all path overrides, including separate runtime
files and target XDG roots. The setup journal/lease stays at the historical home
location so a changed environment cannot hide a retained installation. Custom
layouts require `busctl`: setup checks the actual user manager's unit search path
and Quadlet configuration. `unit_directory` must already be discoverable there;
setup does not alter the manager environment. Local shared reporting currently
requires both workers to use the same unit directory. Changing existing paths or
exclusions requires a separate migration; apply will not move or recreate state.
See [backup path examples](../workers/paths.md).

`artifacts.catalog` selects an operator-local catalog. `helper` plus
`helper_sha256` overrides its helper. `image` and `relay_image` accept digest-pinned
image references. The plan validates target architecture and artifact pins without
downloading or silently changing an installed release. Release publishing and
complete provider/LED catalog qualification are still outstanding.

Unbound notification gateways support helper-only updates through ordinary apply
after reviewing the plan. Other notification policy must stay unchanged. The
update retains identity, state and recovery-helper compatibility; an executable
change restarts verification once. Equivalent reference edits and conversion of
retained unbound whole-policy snapshots do not restart services. Bound/shared
release updates and missing-snapshot import still require their separate workflows.

## Field inventory and execution coverage

The complete leaf-field table below is generated with `make worker-reference`.
Regular tests compare it with every public version-3 YAML field and require an
explicit runtime destination and change classification. Adding a struct field
without updating the inventory fails the check. Coverage text records unsupported
lifecycles; it is not a claim that parser support completes deployment.

| Policy or runtime fields | Source and implementation |
| --- | --- |
| Backup sources, schedule, timezone, freshness, run/snapshot limits, timeouts, retention, restore checks | Compiled into the typed setup transaction; shared dependency reserved/enrolled before sources; ordinary standalone and gateway-unchanged shared updates retain rollback; initial/updated timers staged; derived freshness-only updates coordinate both workers before activation or on unrecovered command-free production gateways, staging capture and pausing delivery for qualification; recovered/command-bearing changes require migration |
| Backup archive path, mount requirement and free-space floor | Compiled into host/runtime/unit code; home and required-mount guards remain, archive overlaps with operational stores are rejected |
| Backup notification selection, reminder/recovery/daily policy and monitor references | No-reporting mode or direct Telegram target references compiled; shared reservation/enrollment, direct-sender fence and qualified fresh report connection implemented; reminder/recovery/daily edits preserve unchanged gateway scope; route/provider/monitor changes need migration |
| Notification routes, queue/payload/database/free-space limits and receipt retention | Compiled into `worker.Config`; raw route labels resolve once into destination-bound revisions. Unbound limit-only changes use a journaled forward migration, check database/WAL and free-space reserves, and preserve identity and completed LED output history; shared verification budgets and route-only changes coordinate the backup declaration before delivery admission while retaining unchanged LED policy; route edits require no pending messages and unchanged providers; effective production changes on unrecovered command-free gateways pause for fresh qualification and retain delivery ownership/reporting; unrecovered Telegram gateways with admitted backup commands support limits-only changes after disabling intake, retaining receiver, permissions, queues/results and LED ownership through fresh qualification and explicit re-enable; recovered budgets and production command-bearing route changes remain pending |
| Producer requiredness, routes, silence/evidence limits, quotas and context capabilities | Explicit local `backup-worker` policy works through notification-first bootstrap and real reservation/enrollment; the configured backup route must be allowed; no YAML-created producer IDs or epochs; verification gateways with unchanged LED policy support existing producer policy changes with unchanged identity; quota reductions must fit retained work, access-policy changes require a settled queue, and effective changes require qualification; effective production changes on unrecovered command-free gateways retain delivery ownership/reporting and pause for qualification; recovery, command-bearing and additional-identity variants remain pending |
| Commands, worker aliases, conversations/threads, actors, permissions and subject scopes | Compiled from YAML and reserved identities/validated specialist descriptors; read mode excludes mutation descriptors and rejects mutation permissions |
| Telegram/Signal provider profiles | Compiled from declared file/socket/conversation references and resolved provider/account facts; Telegram and fresh direct-alert Signal setup are connected to the short command; shared verification gateways support Telegram credential/destination changes, including unadmitted commands for the same bot, with retained private files and a settled queue; unrecovered command-free production gateways also support credential/destination changes with reporting retained and fresh qualification; changing a destination requires a settled queue; recovered and command-bearing variants remain pending |
| Signal linking, data directory, executable and executable digest | Lifecycle inputs, not gateway runtime JSON; fresh linking, supervised setup and explicit resume of an intact stopped service are implemented; owned native coordinator adoption is implemented; legacy adoption/relinking/replacement remain pending |
| LED bindings, selectors, styles, priorities, power opt-in and socket | Initial custom local backup mappings follow real enrollment through a separate resumable transition, including completed output-disable/re-enable history; existing-daemon power output requires explicit opt-in and device inspection. Aggregate setup and unbound toggles are supported; managed ACT and explicitly opted-in PWR setup/removal stages reviewed administrator operations (`daemon.device` selects an observed device). Verification mapping changes can alternate with completed shared policy updates; qualified production output toggles preserve incidents and reporting; production mapping changes, installed power selection changes, recovered installations and daemon replacement remain pending |
| Config/state/data/runtime/installation/unit paths and exclusions | Target resolution; notification bootstrap installs to the selected paths and passes exclusions to managed service rendering; initial backup overrides use versioned layout pins and manager search-path preflight; adoption and relocation remain pending |
| Helper/catalog, container images, provider and daemon artifacts | Local catalog resolution/digest checks; lifecycle adapters select generated runtime inputs; release publishing is separate |
| Worker/recovery/producer identities, generations, ownership receipts, queue/cursors and operation IDs | Generated/pinned domain facts; never writable YAML policy |
| Command descriptors, schema versions, route fingerprints, installation revisions and applied hashes | Validated specialist contracts or generated compilation/lifecycle records; not alternate user configuration |

Conversation-based alert routing uses the declared conversation instead of a
separate fixed chat/recipient credential. Command conversations derive the logical
provider account when omitted; Telegram intake additionally needs `bot_name`.
The canonical `status_leds.enabled` remains the only LED enablement setting.

Compiler tests cover named producer limits, alias-to-identity permissions, sorted
scopes/selectors, conversation/thread binding, explicit power-LED policy and
repeatable raw-to-resolved route compilation. Those tests are software evidence;
they do not qualify external provider accounts or Raspberry Pi hardware.

## Complete public field inventory

<!-- worker-field-inventory:start -->

Checked against the version-3 YAML types. `*` names a mapping entry and `[]` a structured list item. Provider-specific fields apply only to that provider. This table distinguishes compilation from currently supported lifecycle operations.

| YAML field | Runtime or lifecycle target | Change class | Execution coverage |
| --- | --- | --- | --- |
| `backup_worker.artifacts.catalog` | artifact resolver and SetupDeclaration | `artifact_update` | Local catalog/static helper selection; compatible standalone updates and fresh shared backup helper upgrades retain source helpers and original reporting ownership, including completed command receivers with unchanged capabilities, SQL binding and results; shared gateway/source helper and receiver capability upgrades remain separate |
| `backup_worker.artifacts.helper` | artifact resolver and SetupDeclaration | `artifact_update` | Local catalog/static helper selection; compatible standalone updates and fresh shared backup helper upgrades retain source helpers and original reporting ownership, including completed command receivers with unchanged capabilities, SQL binding and results; shared gateway/source helper and receiver capability upgrades remain separate |
| `backup_worker.artifacts.helper_sha256` | artifact resolver and SetupDeclaration | `artifact_update` | Local catalog/static helper selection; compatible standalone updates and fresh shared backup helper upgrades retain source helpers and original reporting ownership, including completed command receivers with unchanged capabilities, SQL binding and results; shared gateway/source helper and receiver capability upgrades remain separate |
| `backup_worker.artifacts.image` | pinned container/relay images | `artifact_update` | Backup image required for staging; pinned shared backup-image changes retain the helper, sources and reporting handoff, including after completed shared policy/source changes, and stage capture for qualification; relay image used only by applicable legacy reporting |
| `backup_worker.artifacts.relay_image` | pinned container/relay images | `artifact_update` | Backup image required for staging; pinned shared backup-image changes retain the helper, sources and reporting handoff, including after completed shared policy/source changes, and stage capture for qualification; relay image used only by applicable legacy reporting |
| `backup_worker.enabled` | setup activity intent and execution fence | `activity_transition` | Standalone disable and staged re-enable; shared activity pending |
| `backup_worker.freshness` | WorkerInstallation.Policy and default gateway producer evidence deadline | `backup_update` | Standalone and unchanged-gateway shared update; derived freshness-only changes coordinate both runtimes and archive checkpoint through either worker before activation or on unrecovered command-free production gateways; production updates stage capture and pause delivery for qualification, preserving ownership and unchanged Signal/LED state; recovered and command-bearing migrations pending |
| `backup_worker.max_run_time` | WorkerInstallation.Policy | `backup_update` | Standalone and unchanged-gateway shared updates, including after completed production notification policy changes, retain the original reporting handoff and stage capture; change derived freshness separately before changing schedule |
| `backup_worker.max_snapshot_bytes` | WorkerInstallation.Policy | `backup_update` | Standalone and unchanged-gateway shared updates, including after completed production notification policy changes, retain the original reporting handoff and stage capture; change derived freshness separately before changing schedule |
| `backup_worker.monitoring.backup_ping_url_file` | WorkerInstallation.Policy.Monitoring | `monitor_review` | Private target HTTPS references; updates validate distinct checks, preserve shared ownership and return schedules to qualification without sending heartbeats |
| `backup_worker.monitoring.daily_delivery_ping_url_file` | WorkerInstallation.Policy.Monitoring | `monitor_review` | Private target HTTPS references; updates validate distinct checks, preserve shared ownership and return schedules to qualification without sending heartbeats |
| `backup_worker.notifications.daily_at` | WorkerInstallation.Policy.Notifications | `backup_update` | Standalone and unchanged-gateway shared reporting policy update |
| `backup_worker.notifications.enabled` | generated reporting mode and producer reservation | `route_migration` | Standalone or local shared initial setup; changing reporting ownership requires migration |
| `backup_worker.notifications.failure` | WorkerInstallation.Policy.Notifications | `backup_update` | Standalone and unchanged-gateway shared reporting policy update |
| `backup_worker.notifications.fallback` | WorkerInstallation.Policy.Notifications | `route_migration` | Direct Telegram setup; direct Signal and provider/route migration need lifecycle work |
| `backup_worker.notifications.primary` | WorkerInstallation.Policy.Notifications | `route_migration` | Direct Telegram setup; direct Signal and provider/route migration need lifecycle work |
| `backup_worker.notifications.recovery` | WorkerInstallation.Policy.Notifications | `backup_update` | Standalone and unchanged-gateway shared reporting policy update |
| `backup_worker.notifications.repeat_after` | WorkerInstallation.Policy.Notifications | `backup_update` | Standalone and unchanged-gateway shared reporting policy update |
| `backup_worker.notifications.route` | retained backup report route and gateway producer permissions | `route_migration` | Before activation select an existing route through either worker; default producer permissions migrate with a settled queue; an explicitly allowed route records the choice without restarting; production changes pending |
| `backup_worker.notifications.signal.account_file` | WorkerInstallation.Policy.Notifications.Signal | `route_migration` | Target references; direct Signal short setup remains unsupported |
| `backup_worker.notifications.signal.recipient_file` | WorkerInstallation.Policy.Notifications.Signal | `route_migration` | Target references; direct Signal short setup remains unsupported |
| `backup_worker.notifications.telegram.chat_id_file` | WorkerInstallation.Policy.Notifications.Telegram.ChatIDFile | `route_migration` | Private target reference; standalone same-bot numeric destination changes require accepted outbox history and no reserved delivery, retain both file sets through rollback, and stage qualification; shared production migration pending |
| `backup_worker.notifications.telegram.token_file` | WorkerInstallation.Policy.Notifications.Telegram.TokenFile | `route_migration` | Private target reference; standalone same-bot rotation/reference moves pin old and new token/chat files through update/rollback; qualification and explicit schedule reactivation follow |
| `backup_worker.notifications.worker` | generated reporting mode and producer reservation | `route_migration` | Standalone or local shared initial setup; changing reporting ownership requires migration |
| `backup_worker.paths.config` | target path resolver and versioned backup layout pins | `path_migration` | New custom home/XDG setup, native exclusions and pinned recovery; custom unit directory must be discoverable by the target user manager; existing path migration pending |
| `backup_worker.paths.data` | target path resolver and versioned backup layout pins | `path_migration` | New custom home/XDG setup, native exclusions and pinned recovery; custom unit directory must be discoverable by the target user manager; existing path migration pending |
| `backup_worker.paths.denied_paths` | target path resolver and versioned backup layout pins | `path_migration` | New custom home/XDG setup, native exclusions and pinned recovery; custom unit directory must be discoverable by the target user manager; existing path migration pending |
| `backup_worker.paths.installation` | target path resolver and versioned backup layout pins | `path_migration` | New custom home/XDG setup, native exclusions and pinned recovery; custom unit directory must be discoverable by the target user manager; existing path migration pending |
| `backup_worker.paths.runtime` | target path resolver and versioned backup layout pins | `path_migration` | New custom home/XDG setup, native exclusions and pinned recovery; custom unit directory must be discoverable by the target user manager; existing path migration pending |
| `backup_worker.paths.state` | target path resolver and versioned backup layout pins | `path_migration` | New custom home/XDG setup, native exclusions and pinned recovery; custom unit directory must be discoverable by the target user manager; existing path migration pending |
| `backup_worker.paths.unit_directory` | target path resolver and versioned backup layout pins | `path_migration` | New custom home/XDG setup, native exclusions and pinned recovery; custom unit directory must be discoverable by the target user manager; existing path migration pending |
| `backup_worker.restore_checks[].command` | WorkerInstallation.Policy.RestoreChecks | `backup_update` | Validated source/instance, pinned image, argument array and timeout; setup/update |
| `backup_worker.restore_checks[].image` | WorkerInstallation.Policy.RestoreChecks | `backup_update` | Validated source/instance, pinned image, argument array and timeout; setup/update |
| `backup_worker.restore_checks[].instance` | WorkerInstallation.Policy.RestoreChecks | `backup_update` | Validated source/instance, pinned image, argument array and timeout; setup/update |
| `backup_worker.restore_checks[].source` | WorkerInstallation.Policy.RestoreChecks | `backup_update` | Validated source/instance, pinned image, argument array and timeout; setup/update |
| `backup_worker.restore_checks[].timeout` | WorkerInstallation.Policy.RestoreChecks | `backup_update` | Validated source/instance, pinned image, argument array and timeout; setup/update |
| `backup_worker.retention.keep_last` | WorkerInstallation.Policy.Retention | `backup_update` | Explicit setup/update; omitted/null retention disables pruning |
| `backup_worker.retention.max_age` | WorkerInstallation.Policy.Retention | `backup_update` | Explicit setup/update; omitted/null retention disables pruning |
| `backup_worker.retire_sources.*` | retained source revocation intent and archive incident disposition | `source_retirement` | Explicit alias/reason for removal or combined new-alias replacement; retains archives, keys, fault dispositions and unchanged shared producer/gateway ownership |
| `backup_worker.schedule` | WorkerInstallation.Policy | `backup_update` | Standalone and unchanged-gateway shared updates, including after completed production notification policy changes, retain the original reporting handoff and stage capture; change derived freshness separately before changing schedule |
| `backup_worker.sources` | WorkerInstallation.Sources and restricted source enrollment | `producer_enrollment` | Initial setup, additions and retained-identity endpoint moves (both also after completed shared policy changes) and explicit retire_sources removal, including after completed shared policy changes; combined new-alias replacement, also after completed shared policy changes, retains enrollment/revocation across rollback with shared ownership unchanged; retired alias/identity reuse pending |
| `backup_worker.storage.min_free_bytes` | WorkerInstallation.Policy.Storage.MinFreeBytes | `backup_update` | Validated setup/update; archive capacity guards remain |
| `backup_worker.storage.mode` | WorkerInstallation.Policy.Storage and archive ownership | `path_migration` | New home/mount setup; existing path/filesystem changes require migration |
| `backup_worker.storage.path` | WorkerInstallation.Policy.Storage and archive ownership | `path_migration` | New home/mount setup; existing path/filesystem changes require migration |
| `backup_worker.storage.require_mount` | WorkerInstallation.Policy.Storage and archive ownership | `path_migration` | New home/mount setup; existing path/filesystem changes require migration |
| `backup_worker.timeouts.connect` | WorkerInstallation.Policy.Timeouts | `backup_update` | Validated setup and journaled update |
| `backup_worker.timeouts.inventory` | WorkerInstallation.Policy.Timeouts | `backup_update` | Validated setup and journaled update |
| `backup_worker.timeouts.shutdown_grace` | WorkerInstallation.Policy.Timeouts | `backup_update` | Validated setup and journaled update |
| `backup_worker.timeouts.snapshot` | WorkerInstallation.Policy.Timeouts | `backup_update` | Validated setup and journaled update |
| `backup_worker.timeouts.transfer` | WorkerInstallation.Policy.Timeouts | `backup_update` | Validated setup and journaled update |
| `backup_worker.timeouts.verify` | WorkerInstallation.Policy.Timeouts | `backup_update` | Validated setup and journaled update |
| `backup_worker.timezone` | WorkerInstallation.Policy | `backup_update` | Standalone and unchanged-gateway shared updates, including after completed production notification policy changes, retain the original reporting handoff and stage capture; change derived freshness separately before changing schedule |
| `notification_worker.artifacts.catalog` | artifact resolver and managed executable pins | `artifact_update` | Initial setup and unbound compatible helper update; bound/shared upgrade pending |
| `notification_worker.artifacts.helper` | artifact resolver and managed executable pins | `artifact_update` | Initial setup and unbound compatible helper update; bound/shared upgrade pending |
| `notification_worker.artifacts.helper_sha256` | artifact resolver and managed executable pins | `artifact_update` | Initial setup and unbound compatible helper update; bound/shared upgrade pending |
| `notification_worker.artifacts.image` | not applicable to the native notification worker | `artifact_update` | Rejected; container image policy belongs to backup_worker.artifacts |
| `notification_worker.artifacts.relay_image` | not applicable to the native notification worker | `artifact_update` | Rejected; container image policy belongs to backup_worker.artifacts |
| `notification_worker.commands.conversations.*.account` | interaction.Config.Conversations and provider profiles | `command_admission` | Initial Telegram conversations and threads use the retained account; command admission follows completed receiver setup; conversation retirement supported while disabled; new or redirected Telegram audiences supported before activation; production changes pending |
| `notification_worker.commands.conversations.*.native_id` | interaction.Config.Conversations and provider profiles | `command_admission` | Initial Telegram conversations and threads use the retained account; command admission follows completed receiver setup; conversation retirement supported while disabled; new or redirected Telegram audiences supported before activation; production changes pending |
| `notification_worker.commands.conversations.*.provider` | interaction.Config.Conversations and provider profiles | `command_admission` | Initial Telegram conversations and threads use the retained account; command admission follows completed receiver setup; conversation retirement supported while disabled; new or redirected Telegram audiences supported before activation; production changes pending |
| `notification_worker.commands.conversations.*.thread` | interaction.Config.Conversations and provider profiles | `command_admission` | Initial Telegram conversations and threads use the retained account; command admission follows completed receiver setup; conversation retirement supported while disabled; new or redirected Telegram audiences supported before activation; production changes pending |
| `notification_worker.commands.enabled` | coordinated command admission and backup declaration scope | `command_admission` | Disabled by default; notification-first command drafts can change before producer reservation without restarting; verification commands can be disabled/restored with exact retained policy and no intake; admitted local Telegram backup commands can be disabled/re-enabled with other settings unchanged, retaining reporting and results; re-enable requires valid delivery qualification; unrecovered Telegram command gateways can disable intake, qualify and apply to renew delivery while retaining receiver, results and permission history, then explicitly re-enable; initial enrollment on an existing staged Telegram gateway supported; activated command-free enrollment pending |
| `notification_worker.commands.mode` | interaction.Config and command admission mode | `command_admission` | New or already enrolled verification gateways can add local backup read or mutation commands using real reservations, stage a versioned receiver and separately admit qualified intake; alias renaming and mode changes within the original receiver capabilities supported while disabled; receiver capability upgrades pending |
| `notification_worker.commands.permissions[].actor` | interaction.Config.Permissions | `command_admission` | Initial read and explicit incident.manage permissions checked against approved backup descriptors and retained before admission; mutation requires mutation mode; permission reductions and explicit restoration within original receiver authority supported while disabled; new authority can be declared before activation; production expansion pending |
| `notification_worker.commands.permissions[].conversation` | interaction.Config.Permissions | `command_admission` | Initial read and explicit incident.manage permissions checked against approved backup descriptors and retained before admission; mutation requires mutation mode; permission reductions and explicit restoration within original receiver authority supported while disabled; new authority can be declared before activation; production expansion pending |
| `notification_worker.commands.permissions[].permissions` | interaction.Config.Permissions | `command_admission` | Initial read and explicit incident.manage permissions checked against approved backup descriptors and retained before admission; mutation requires mutation mode; permission reductions and explicit restoration within original receiver authority supported while disabled; new authority can be declared before activation; production expansion pending |
| `notification_worker.commands.permissions[].scope.subjects` | interaction permission scope | `command_admission` | Initial read/incident scopes compiled, sorted and enforced by the specialist receiver; scope reductions and explicit restoration within original receiver authority supported while disabled; new authority can be declared before activation; production expansion pending |
| `notification_worker.commands.permissions[].scope.whole_worker` | interaction permission scope | `command_admission` | Initial read/incident scopes compiled, sorted and enforced by the specialist receiver; scope reductions and explicit restoration within original receiver authority supported while disabled; new authority can be declared before activation; production expansion pending |
| `notification_worker.commands.permissions[].worker` | interaction.Config.Permissions | `command_admission` | Initial read and explicit incident.manage permissions checked against approved backup descriptors and retained before admission; mutation requires mutation mode; permission reductions and explicit restoration within original receiver authority supported while disabled; new authority can be declared before activation; production expansion pending |
| `notification_worker.commands.workers.*` | interaction.Config and command admission mode | `command_admission` | New or already enrolled verification gateways can add local backup read or mutation commands using real reservations, stage a versioned receiver and separately admit qualified intake; alias renaming and mode changes within the original receiver capabilities supported while disabled; receiver capability upgrades pending |
| `notification_worker.enabled` | managed activity transaction | `activity_transition` | Unbound disable/re-enable; bound/shared activity pending |
| `notification_worker.limits.max_database_bytes` | worker.Config storage and receipt budgets | `budget_review` | Initial setup, unbound and shared verification updates retain LED history; effective production budget changes on unrecovered command-free local shared gateways coordinate a delivery pause, retained queue/owner/reporting and fresh qualification, including unchanged owned Signal and LED settings; unrecovered Telegram gateways with retained backup commands can disable intake, apply limits-only changes, qualify and apply while retaining receiver, permissions, queues/results and LED ownership, then explicitly re-enable commands; recovered variants remain pending |
| `notification_worker.limits.max_messages` | worker.Config storage and receipt budgets | `budget_review` | Initial setup, unbound and shared verification updates retain LED history; effective production budget changes on unrecovered command-free local shared gateways coordinate a delivery pause, retained queue/owner/reporting and fresh qualification, including unchanged owned Signal and LED settings; unrecovered Telegram gateways with retained backup commands can disable intake, apply limits-only changes, qualify and apply while retaining receiver, permissions, queues/results and LED ownership, then explicitly re-enable commands; recovered variants remain pending |
| `notification_worker.limits.max_payload_bytes` | worker.Config storage and receipt budgets | `budget_review` | Initial setup, unbound and shared verification updates retain LED history; effective production budget changes on unrecovered command-free local shared gateways coordinate a delivery pause, retained queue/owner/reporting and fresh qualification, including unchanged owned Signal and LED settings; unrecovered Telegram gateways with retained backup commands can disable intake, apply limits-only changes, qualify and apply while retaining receiver, permissions, queues/results and LED ownership, then explicitly re-enable commands; recovered variants remain pending |
| `notification_worker.limits.min_free_bytes` | worker.Config storage and receipt budgets | `budget_review` | Initial setup, unbound and shared verification updates retain LED history; effective production budget changes on unrecovered command-free local shared gateways coordinate a delivery pause, retained queue/owner/reporting and fresh qualification, including unchanged owned Signal and LED settings; unrecovered Telegram gateways with retained backup commands can disable intake, apply limits-only changes, qualify and apply while retaining receiver, permissions, queues/results and LED ownership, then explicitly re-enable commands; recovered variants remain pending |
| `notification_worker.limits.receipt_audit_days` | worker.Config storage and receipt budgets | `budget_review` | Initial setup, unbound and shared verification updates retain LED history; effective production budget changes on unrecovered command-free local shared gateways coordinate a delivery pause, retained queue/owner/reporting and fresh qualification, including unchanged owned Signal and LED settings; unrecovered Telegram gateways with retained backup commands can disable intake, apply limits-only changes, qualify and apply while retaining receiver, permissions, queues/results and LED ownership, then explicitly re-enable commands; recovered variants remain pending |
| `notification_worker.paths.config` | target resolver and managed service paths/exclusions | `path_migration` | New custom home/XDG setup; existing path migration/import pending |
| `notification_worker.paths.data` | target resolver and managed service paths/exclusions | `path_migration` | New custom home/XDG setup; existing path migration/import pending |
| `notification_worker.paths.denied_paths` | target resolver and managed service paths/exclusions | `path_migration` | New custom home/XDG setup; existing path migration/import pending |
| `notification_worker.paths.installation` | target resolver and managed service paths/exclusions | `path_migration` | New custom home/XDG setup; existing path migration/import pending |
| `notification_worker.paths.runtime` | target resolver and managed service paths/exclusions | `path_migration` | New custom home/XDG setup; existing path migration/import pending |
| `notification_worker.paths.state` | target resolver and managed service paths/exclusions | `path_migration` | New custom home/XDG setup; existing path migration/import pending |
| `notification_worker.paths.unit_directory` | target resolver and managed service paths/exclusions | `path_migration` | New custom home/XDG setup; existing path migration/import pending |
| `notification_worker.producers.*.context_kinds` | worker.Config.Producers enrollment | `producer_enrollment` | Existing local backup policy supports verification and effective production changes on unrecovered command-free gateways; retains ownership, observations, reporting and unchanged Signal/LED state; access changes require a settled queue and fresh qualification; equivalent explicit/derived producer defaults retain the runtime and qualification; recovery, command-bearing and additional-identity migrations pending |
| `notification_worker.producers.*.interactive_context` | worker.Config.Producers enrollment | `producer_enrollment` | Existing local backup policy supports verification and effective production changes on unrecovered command-free gateways; retains ownership, observations, reporting and unchanged Signal/LED state; access changes require a settled queue and fresh qualification; equivalent explicit/derived producer defaults retain the runtime and qualification; recovery, command-bearing and additional-identity migrations pending |
| `notification_worker.producers.*.max_evidence_seconds` | worker.Config.Producers timing thresholds | `producer_enrollment` | Initial local backup policy and shared verification updates; effective production timing changes on unrecovered command-free gateways retain queued work, original observations and unchanged Signal/LED state; fresh qualification required; equivalent explicit/derived producer defaults retain the runtime and qualification; recovery and command-bearing migrations pending |
| `notification_worker.producers.*.max_messages` | worker.Config.Producers enrollment | `producer_enrollment` | Existing local backup policy supports verification and effective production changes on unrecovered command-free gateways; retains ownership, observations, reporting and unchanged Signal/LED state; access changes require a settled queue and fresh qualification; equivalent explicit/derived producer defaults retain the runtime and qualification; recovery, command-bearing and additional-identity migrations pending |
| `notification_worker.producers.*.max_silence_seconds` | worker.Config.Producers timing thresholds | `producer_enrollment` | Initial local backup policy and shared verification updates; effective production timing changes on unrecovered command-free gateways retain queued work, original observations and unchanged Signal/LED state; fresh qualification required; equivalent explicit/derived producer defaults retain the runtime and qualification; recovery and command-bearing migrations pending |
| `notification_worker.producers.*.policy_health` | worker.Config.Producers enrollment | `producer_enrollment` | Existing local backup policy supports verification and effective production changes on unrecovered command-free gateways; retains ownership, observations, reporting and unchanged Signal/LED state; access changes require a settled queue and fresh qualification; equivalent explicit/derived producer defaults retain the runtime and qualification; recovery, command-bearing and additional-identity migrations pending |
| `notification_worker.producers.*.required` | worker.Config.Producers enrollment | `producer_enrollment` | Existing local backup policy supports verification and effective production changes on unrecovered command-free gateways; retains ownership, observations, reporting and unchanged Signal/LED state; access changes require a settled queue and fresh qualification; equivalent explicit/derived producer defaults retain the runtime and qualification; recovery, command-bearing and additional-identity migrations pending |
| `notification_worker.producers.*.routes` | worker.Config.Producers enrollment | `producer_enrollment` | Existing local backup policy supports verification and effective production changes on unrecovered command-free gateways; retains ownership, observations, reporting and unchanged Signal/LED state; access changes require a settled queue and fresh qualification; equivalent explicit/derived producer defaults retain the runtime and qualification; recovery, command-bearing and additional-identity migrations pending |
| `notification_worker.providers.*.account` | provider/interaction account and conversation profiles | `provider_review` | Initial Telegram account/bot_name matched to the supplied token; fixed alerts retained separately; interactive alert destinations and native Signal intake pending |
| `notification_worker.providers.*.account_file` | fresh Signal selector and fixed destination | `provider_review` | Signal target references; fresh setup supported, rotation/audience migration pending |
| `notification_worker.providers.*.account_input` | private framed credential provisioning | `provider_review` | Operator-local input references; initial setup, never plaintext YAML or argv |
| `notification_worker.providers.*.bot_name` | provider/interaction account and conversation profiles | `provider_review` | Initial Telegram account/bot_name matched to the supplied token; fixed alerts retained separately; interactive alert destinations and native Signal intake pending |
| `notification_worker.providers.*.chat_id_file` | provider.Profile Telegram credentials | `provider_review` | Telegram target references; unbound and shared verification rotation/destination changes, including unadmitted commands for the same bot, retain old/new private files and any unchanged owned native Signal service; an unused Telegram profile can be added/removed beside unchanged owned Signal before activation or on an unrecovered command-free production gateway with a settled queue and fresh qualification; verification shared changes retain unchanged LED policy and require a settled queue; unrecovered command-free production gateways, including unchanged owned Signal, can rotate same-destination private references with queued work retained, or change Telegram chat/bot with a settled queue; both pause for fresh qualification and retain reporting ownership and old/new credential exclusions; pinned account/name changes, Signal accounts, command-bearing and recovered variants remain pending |
| `notification_worker.providers.*.chat_id_input` | private framed credential provisioning | `provider_review` | Operator-local input references; initial setup, never plaintext YAML or argv |
| `notification_worker.providers.*.conversation` | provider/interaction account and conversation profiles | `provider_review` | Initial Telegram account/bot_name matched to the supplied token; fixed alerts retained separately; interactive alert destinations and native Signal intake pending |
| `notification_worker.providers.*.data` | fresh Signal service/account/native artifact paths | `provider_review` | Fresh Signal setup and coordinator adoption of a proven owned native service; legacy account takeover, relink and replacement pending |
| `notification_worker.providers.*.executable` | fresh Signal service/account/native artifact paths | `provider_review` | Fresh Signal setup and coordinator adoption of a proven owned native service; legacy account takeover, relink and replacement pending |
| `notification_worker.providers.*.executable_sha256` | fresh Signal service/account/native artifact paths | `provider_review` | Fresh Signal setup and coordinator adoption of a proven owned native service; legacy account takeover, relink and replacement pending |
| `notification_worker.providers.*.recipient_file` | fresh Signal selector and fixed destination | `provider_review` | Signal target references; fresh setup supported, rotation/audience migration pending |
| `notification_worker.providers.*.recipient_input` | private framed credential provisioning | `provider_review` | Operator-local input references; initial setup, never plaintext YAML or argv |
| `notification_worker.providers.*.socket` | fresh Signal service/account/native artifact paths | `provider_review` | Fresh Signal setup and coordinator adoption of a proven owned native service; legacy account takeover, relink and replacement pending |
| `notification_worker.providers.*.token_file` | provider.Profile Telegram credentials | `provider_review` | Telegram target references; unbound and shared verification rotation/destination changes, including unadmitted commands for the same bot, retain old/new private files and any unchanged owned native Signal service; an unused Telegram profile can be added/removed beside unchanged owned Signal before activation or on an unrecovered command-free production gateway with a settled queue and fresh qualification; verification shared changes retain unchanged LED policy and require a settled queue; unrecovered command-free production gateways, including unchanged owned Signal, can rotate same-destination private references with queued work retained, or change Telegram chat/bot with a settled queue; both pause for fresh qualification and retain reporting ownership and old/new credential exclusions; pinned account/name changes, Signal accounts, command-bearing and recovered variants remain pending |
| `notification_worker.providers.*.token_input` | private framed credential provisioning | `provider_review` | Operator-local input references; initial setup, never plaintext YAML or argv |
| `notification_worker.routes.*.fallback` | worker.Config.Routes and provider-bound revisions | `route_migration` | Initial/unbound, shared verification and effective production route-only changes on unrecovered command-free gateways; settle queued work first, retain provider/producer permissions, reporting and unchanged Signal/LED state, then qualify; omitted single-provider route defaults normalize without a change; recovery, command-bearing and provider replacement migrations pending |
| `notification_worker.routes.*.primary` | worker.Config.Routes and provider-bound revisions | `route_migration` | Initial/unbound, shared verification and effective production route-only changes on unrecovered command-free gateways; settle queued work first, retain provider/producer permissions, reporting and unchanged Signal/LED state, then qualify; omitted single-provider route defaults normalize without a change; recovery, command-bearing and provider replacement migrations pending |
| `notification_worker.status_leds.bindings.*.led` | worker.Config.LEDBindings | `led_transition` | Initial local backup mapping and later verification-mode binding edits coordinate the staged backup scope; verification output toggles retain bindings; production transitions pending |
| `notification_worker.status_leds.bindings.*.producer` | worker.Config.LEDBindings | `led_transition` | Initial local backup mapping and later verification-mode binding edits coordinate the staged backup scope; verification output toggles retain bindings; production transitions pending |
| `notification_worker.status_leds.bindings.*.styles.*.pattern` | worker.LEDBinding styles | `led_transition` | Initial and later verification-mode binding edits validate supported names/priority bands; production mapping changes pending |
| `notification_worker.status_leds.bindings.*.styles.*.priority` | worker.LEDBinding styles | `led_transition` | Initial and later verification-mode binding edits validate supported names/priority bands; production mapping changes pending |
| `notification_worker.status_leds.bindings.*.subjects` | worker.Config.LEDBindings | `led_transition` | Initial local backup mapping and later verification-mode binding edits coordinate the staged backup scope; verification output toggles retain bindings; production transitions pending |
| `notification_worker.status_leds.daemon.artifact` | managed daemon artifact and observed device | `led_transition` | Managed initial setup/removal and existing-daemon verification; replacement pending |
| `notification_worker.status_leds.daemon.device` | managed daemon artifact and observed device | `led_transition` | Managed initial setup/removal and existing-daemon verification; replacement pending |
| `notification_worker.status_leds.daemon.mode` | managed daemon artifact and observed device | `led_transition` | Managed initial setup/removal and existing-daemon verification; replacement pending |
| `notification_worker.status_leds.daemon.sha256` | managed daemon artifact and observed device | `led_transition` | Managed initial setup/removal and existing-daemon verification; replacement pending |
| `notification_worker.status_leds.enabled` | managed LED output disposition | `led_transition` | Initial setup and verification toggles; qualified unrecovered shared delivery toggles preserve bindings, incidents and reporting; command-free qualification renewals retain output history; re-enable requires originally qualified output |
| `notification_worker.status_leds.power_led` | worker.LEDBinding.AllowPower and host selection | `led_transition` | Initial managed provisioning with explicit opt-in and discovered PWR device; changing an installed daemon requires migration |
| `notification_worker.status_leds.socket` | managed runtime LED endpoint | `led_transition` | Managed or explicitly existing daemon; later endpoint migration pending |

The following runtime leaves are checked against the backup installation and notification runtime types. YAML-owned entries identify their input; generated entries explain why they are not independent YAML settings. Provider account databases, queues, ownership receipts and recovery history remain runtime stores outside these configuration documents.

| Runtime field | Authority and derivation |
| --- | --- |
| `backup.archive_id` | Generated schema, reserved worker/archive identities and canonical policy digest; immutable ownership facts |
| `backup.config_hash` | Generated schema, reserved worker/archive identities and canonical policy digest; immutable ownership facts |
| `backup.image` | YAML backup_worker.artifacts.image / relay_image or the selected pinned catalog |
| `backup.paths.config` | YAML backup_worker.paths overrides resolved against pinned target home/XDG defaults; setup, recovery and runtime share the retained layout |
| `backup.paths.config_home` | Generated target layout authority; target home/XDG facts and installer layout version |
| `backup.paths.data` | YAML backup_worker.paths overrides resolved against pinned target home/XDG defaults; setup, recovery and runtime share the retained layout |
| `backup.paths.data_home` | Generated target layout authority; target home/XDG facts and installer layout version |
| `backup.paths.denied_paths` | YAML backup_worker.paths overrides resolved against pinned target home/XDG defaults; setup, recovery and runtime share the retained layout |
| `backup.paths.home` | Generated target layout authority; target home/XDG facts and installer layout version |
| `backup.paths.installation` | YAML backup_worker.paths overrides resolved against pinned target home/XDG defaults; setup, recovery and runtime share the retained layout |
| `backup.paths.quadlet_directory` | Generated target layout authority; target home/XDG facts and installer layout version |
| `backup.paths.runtime` | YAML backup_worker.paths overrides resolved against pinned target home/XDG defaults; setup, recovery and runtime share the retained layout |
| `backup.paths.state` | YAML backup_worker.paths overrides resolved against pinned target home/XDG defaults; setup, recovery and runtime share the retained layout |
| `backup.paths.state_home` | Generated target layout authority; target home/XDG facts and installer layout version |
| `backup.paths.unit_directory` | YAML backup_worker.paths overrides resolved against pinned target home/XDG defaults; setup, recovery and runtime share the retained layout |
| `backup.paths.version` | Generated target layout authority; target home/XDG facts and installer layout version |
| `backup.policy.freshness` | YAML backup_worker fields of the same name, with versioned normalization |
| `backup.policy.max_run_time` | YAML backup_worker fields of the same name, with versioned normalization |
| `backup.policy.max_snapshot_bytes` | YAML backup_worker fields of the same name, with versioned normalization |
| `backup.policy.monitoring.backup_ping_url_file` | YAML backup_worker.monitoring target endpoint references |
| `backup.policy.monitoring.daily_delivery_ping_url_file` | YAML backup_worker.monitoring target endpoint references |
| `backup.policy.notifications.daily_at` | YAML backup_worker.notifications; shared route ownership is resolved separately |
| `backup.policy.notifications.failure` | YAML backup_worker.notifications; shared route ownership is resolved separately |
| `backup.policy.notifications.fallback` | YAML backup_worker.notifications; shared route ownership is resolved separately |
| `backup.policy.notifications.mode` | Generated none/shared/legacy reporting mode from declared enabled state and worker/provider selection |
| `backup.policy.notifications.primary` | YAML backup_worker.notifications; shared route ownership is resolved separately |
| `backup.policy.notifications.recovery` | YAML backup_worker.notifications; shared route ownership is resolved separately |
| `backup.policy.notifications.repeat_after` | YAML backup_worker.notifications; shared route ownership is resolved separately |
| `backup.policy.notifications.signal.account_file` | YAML backup_worker.notifications.signal target credential references |
| `backup.policy.notifications.signal.recipient_file` | YAML backup_worker.notifications.signal target credential references |
| `backup.policy.notifications.telegram.chat_id_file` | YAML backup_worker.notifications.telegram target credential references |
| `backup.policy.notifications.telegram.token_file` | YAML backup_worker.notifications.telegram target credential references |
| `backup.policy.restore_checks[].command` | YAML backup_worker.restore_checks entries, preserving source/image/argument restrictions |
| `backup.policy.restore_checks[].image` | YAML backup_worker.restore_checks entries, preserving source/image/argument restrictions |
| `backup.policy.restore_checks[].instance` | YAML backup_worker.restore_checks entries, preserving source/image/argument restrictions |
| `backup.policy.restore_checks[].source` | YAML backup_worker.restore_checks entries, preserving source/image/argument restrictions |
| `backup.policy.restore_checks[].timeout` | YAML backup_worker.restore_checks entries, preserving source/image/argument restrictions |
| `backup.policy.retention.keep_last` | YAML backup_worker.retention; null/omitted means no automatic pruning |
| `backup.policy.retention.max_age` | YAML backup_worker.retention; null/omitted means no automatic pruning |
| `backup.policy.schedule` | YAML backup_worker fields of the same name, with versioned normalization |
| `backup.policy.sources` | YAML backup_worker fields of the same name, with versioned normalization |
| `backup.policy.storage.min_free_bytes` | YAML backup_worker.storage; target-resolved path and explicit home/mount mode |
| `backup.policy.storage.mode` | YAML backup_worker.storage; target-resolved path and explicit home/mount mode |
| `backup.policy.storage.path` | YAML backup_worker.storage; target-resolved path and explicit home/mount mode |
| `backup.policy.storage.require_mount` | YAML backup_worker.storage; target-resolved path and explicit home/mount mode |
| `backup.policy.timeouts.connect` | YAML backup_worker.timeouts fields of the same name |
| `backup.policy.timeouts.inventory` | YAML backup_worker.timeouts fields of the same name |
| `backup.policy.timeouts.shutdown_grace` | YAML backup_worker.timeouts fields of the same name |
| `backup.policy.timeouts.snapshot` | YAML backup_worker.timeouts fields of the same name |
| `backup.policy.timeouts.transfer` | YAML backup_worker.timeouts fields of the same name |
| `backup.policy.timeouts.verify` | YAML backup_worker.timeouts fields of the same name |
| `backup.policy.timezone` | YAML backup_worker fields of the same name, with versioned normalization |
| `backup.qualification.*` | Observed qualification records; never generated passing values from desired YAML |
| `backup.relay_image` | YAML backup_worker.artifacts.image / relay_image or the selected pinned catalog |
| `backup.schema` | Generated schema, reserved worker/archive identities and canonical policy digest; immutable ownership facts |
| `backup.sources[].alias` | YAML backup_worker.sources and the referenced inventory server endpoints |
| `backup.sources[].host` | YAML backup_worker.sources and the referenced inventory server endpoints |
| `backup.sources[].key_file` | Generated restricted source key/host trust files and enrolled source identity; operator SSH credentials are separate |
| `backup.sources[].known_hosts` | Generated restricted source key/host trust files and enrolled source identity; operator SSH credentials are separate |
| `backup.sources[].port` | YAML backup_worker.sources and the referenced inventory server endpoints |
| `backup.sources[].source_id` | Generated restricted source key/host trust files and enrolled source identity; operator SSH credentials are separate |
| `backup.sources[].user` | YAML backup_worker.sources and the referenced inventory server endpoints |
| `backup.worker_id` | Generated schema, reserved worker/archive identities and canonical policy digest; immutable ownership facts |
| `notification.bootstrap` | Generated runtime version and verification-only empty enrollment state; bootstrap never invents a producer |
| `notification.interactive_signal.account` | YAML Signal logical account and declared or service-derived endpoint |
| `notification.interactive_signal.inbox_identity.recovery` | Generated canonical Signal inbox identity; adoption/recovery preserves its durable authority |
| `notification.interactive_signal.inbox_identity.worker` | Generated canonical Signal inbox identity; adoption/recovery preserves its durable authority |
| `notification.interactive_signal.socket` | YAML Signal logical account and declared or service-derived endpoint |
| `notification.interactive_telegram.account` | YAML Telegram account/token reference/bot_name, bound to the declared conversations |
| `notification.interactive_telegram.bot_name` | YAML Telegram account/token reference/bot_name, bound to the declared conversations |
| `notification.interactive_telegram.token_file` | YAML Telegram account/token reference/bot_name, bound to the declared conversations |
| `notification.led_endpoint.owner_uid` | Generated endpoint version, resolved socket and observed file/kernel peer ownership pins |
| `notification.led_endpoint.peer_uid` | Generated endpoint version, resolved socket and observed file/kernel peer ownership pins |
| `notification.led_endpoint.socket` | Generated endpoint version, resolved socket and observed file/kernel peer ownership pins |
| `notification.led_endpoint.version` | Generated endpoint version, resolved socket and observed file/kernel peer ownership pins |
| `notification.led_socket` | YAML notification_worker.status_leds.socket or verified managed daemon default |
| `notification.providers.*.chat_file` | YAML Telegram target references or private target files provisioned from operator-local inputs |
| `notification.providers.*.conversation.account` | YAML referenced command conversation; fixed destination references are excluded |
| `notification.providers.*.conversation.generation` | Generated conversation enrollment revision, shared with the admitted interaction policy |
| `notification.providers.*.conversation.id` | YAML referenced command conversation; fixed destination references are excluded |
| `notification.providers.*.conversation.native_id` | YAML referenced command conversation; fixed destination references are excluded |
| `notification.providers.*.conversation.provider` | YAML referenced command conversation; fixed destination references are excluded |
| `notification.providers.*.conversation.thread` | YAML referenced command conversation; fixed destination references are excluded |
| `notification.providers.*.destination` | Generated fingerprint of the actual provider identity and destination; never plaintext recipient data |
| `notification.providers.*.kind` | YAML notification_worker.providers mapping key or list shorthand |
| `notification.providers.*.relay_identity.recovery` | Generated canonical relay/inbox identity from verified service ownership |
| `notification.providers.*.relay_identity.worker` | Generated canonical relay/inbox identity from verified service ownership |
| `notification.providers.*.socket` | YAML Signal socket override or generated owned native service endpoint |
| `notification.providers.*.token_file` | YAML Telegram target references or private target files provisioned from operator-local inputs |
| `notification.version` | Generated runtime version and verification-only empty enrollment state; bootstrap never invents a producer |
| `notification.worker.interaction.destinations[].account` | YAML notification_worker.commands.conversations mapping key and declared destination fields |
| `notification.worker.interaction.destinations[].generation` | Generated stable conversation enrollment revision; audience changes require migration |
| `notification.worker.interaction.destinations[].id` | YAML notification_worker.commands.conversations mapping key and declared destination fields |
| `notification.worker.interaction.destinations[].native_id` | YAML notification_worker.commands.conversations mapping key and declared destination fields |
| `notification.worker.interaction.destinations[].provider` | YAML notification_worker.commands.conversations mapping key and declared destination fields |
| `notification.worker.interaction.destinations[].thread` | YAML notification_worker.commands.conversations mapping key and declared destination fields |
| `notification.worker.interaction.permissions[].actor` | YAML notification_worker.commands.permissions fields, checked against descriptors |
| `notification.worker.interaction.permissions[].conversation` | YAML notification_worker.commands.permissions fields, checked against descriptors |
| `notification.worker.interaction.permissions[].permissions` | YAML notification_worker.commands.permissions fields, checked against descriptors |
| `notification.worker.interaction.permissions[].scope.subjects` | YAML command permission scope, normalized and validated |
| `notification.worker.interaction.permissions[].scope.whole_worker` | YAML command permission scope, normalized and validated |
| `notification.worker.interaction.permissions[].worker` | YAML notification_worker.commands.permissions fields, checked against descriptors |
| `notification.worker.interaction.revision` | Generated stable command policy revision; new rights require admission |
| `notification.worker.interaction.workers[].alias` | YAML notification_worker.commands.workers alias |
| `notification.worker.interaction.workers[].commands.*.arguments.*.kind` | Generated validated specialist argument contract; YAML cannot invent executable descriptors |
| `notification.worker.interaction.workers[].commands.*.arguments.*.max` | Generated validated specialist argument contract; YAML cannot invent executable descriptors |
| `notification.worker.interaction.workers[].commands.*.arguments.*.required` | Generated validated specialist argument contract; YAML cannot invent executable descriptors |
| `notification.worker.interaction.workers[].commands.*.confirm` | Generated from validated specialist command descriptors, filtered by YAML command mode/permissions |
| `notification.worker.interaction.workers[].commands.*.effect` | Generated from validated specialist command descriptors, filtered by YAML command mode/permissions |
| `notification.worker.interaction.workers[].commands.*.help` | Generated from validated specialist command descriptors, filtered by YAML command mode/permissions |
| `notification.worker.interaction.workers[].commands.*.id` | Generated from validated specialist command descriptors, filtered by YAML command mode/permissions |
| `notification.worker.interaction.workers[].commands.*.permission` | Generated from validated specialist command descriptors, filtered by YAML command mode/permissions |
| `notification.worker.interaction.workers[].commands.*.scoped` | Generated from validated specialist command descriptors, filtered by YAML command mode/permissions |
| `notification.worker.interaction.workers[].commands.*.target_required` | Generated from validated specialist command descriptors, filtered by YAML command mode/permissions |
| `notification.worker.interaction.workers[].commands.*.version` | Generated from validated specialist command descriptors, filtered by YAML command mode/permissions |
| `notification.worker.interaction.workers[].generation` | Generated retained specialist generation; not a caller-selected YAML token |
| `notification.worker.interaction.workers[].producer.epoch` | Generated reserved producer identity selected by commands.workers alias mapping |
| `notification.worker.interaction.workers[].producer.id` | Generated reserved producer identity selected by commands.workers alias mapping |
| `notification.worker.led_bindings[].allow_power` | YAML binding subject selectors/output and explicit status_leds.power_led permission |
| `notification.worker.led_bindings[].id` | YAML notification_worker.status_leds.bindings mapping key, normalized deterministically |
| `notification.worker.led_bindings[].led` | YAML binding subject selectors/output and explicit status_leds.power_led permission |
| `notification.worker.led_bindings[].producer.epoch` | Generated real producer reservation selected by the binding's declared producer alias |
| `notification.worker.led_bindings[].producer.id` | Generated real producer reservation selected by the binding's declared producer alias |
| `notification.worker.led_bindings[].styles.*.pattern` | YAML binding styles or the versioned default style bands |
| `notification.worker.led_bindings[].styles.*.priority` | YAML binding styles or the versioned default style bands |
| `notification.worker.led_bindings[].subjects` | YAML binding subject selectors/output and explicit status_leds.power_led permission |
| `notification.worker.max_database_bytes` | YAML notification_worker.limits fields of the same name |
| `notification.worker.max_messages` | YAML notification_worker.limits fields of the same name |
| `notification.worker.max_payload_bytes` | YAML notification_worker.limits fields of the same name |
| `notification.worker.min_free_bytes` | YAML notification_worker.limits fields of the same name |
| `notification.worker.producers[].context_kinds` | YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence |
| `notification.worker.producers[].interactive_context` | YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence |
| `notification.worker.producers[].max_evidence_seconds` | YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence |
| `notification.worker.producers[].max_messages` | YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence |
| `notification.worker.producers[].max_silence_seconds` | YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence |
| `notification.worker.producers[].policy_health` | YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence |
| `notification.worker.producers[].producer.epoch` | Generated real specialist reservation and retained enrollment; YAML uses producer aliases |
| `notification.worker.producers[].producer.id` | Generated real specialist reservation and retained enrollment; YAML uses producer aliases |
| `notification.worker.producers[].required` | YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence |
| `notification.worker.producers[].routes` | YAML notification_worker.producers entry, or documented local-backup defaults derived from reporting cadence |
| `notification.worker.receipt_audit_days` | YAML notification_worker.limits fields of the same name |
| `notification.worker.routes.*.fallback` | YAML notification_worker.routes entry of the same name |
| `notification.worker.routes.*.primary` | YAML notification_worker.routes entry of the same name |
| `notification.worker.routes.*.revision` | Generated stable provider-destination-bound route revision from unresolved declaration input |

<!-- worker-field-inventory:end -->
