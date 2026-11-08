# Declarative worker setup: active implementation plan

Status: in progress. This milestone is separate from the completed original
84-criterion software implementation. The [roadmap](../roadmap.md)
tracks remaining work; the [worker guide](../workers/README.md) describes
what users can run today. This document defines the U1–U11 work packages and
UX01–UX30 acceptance scenarios. Planned behavior is not a claim of availability.

## Intended user experience

The ordinary commands are:

```sh
deploy notification-worker manager
deploy backup-worker manager

deploy notification-worker status manager
deploy backup-worker status manager
```

`manager` is an inventory alias, not a special server role or privileged account.
One target account may host both workers. The commands install a missing worker,
resume an interrupted operation, or reconcile a changed declaration. An unchanged
installation produces a concise no-change result without regenerating identities,
restarting services, asking for credentials again, or repeating a provider send.

Backup setup automatically handles its declared local notification dependency.
The user does not need to discover a special installation order. Missing settings
are collected in a guided session; policy choices are saved to YAML and credentials
to private files. Runtime JSON, producer epochs, operation IDs, hashes and internal
migration specifications are generated and retained by the coordinator.

## Existing implementation and integration points

| Area | Current implementation | Work required |
| --- | --- | --- |
| Inventory | [ServerConfig](../../internal/config/config.go) and [inventory parsing](../../internal/config/server_inventory.go) support version 2 and `backup_worker` | Add notification policy, shared references, a versioned defaults contract and migration to the unified schema |
| CLI dispatch | [Main dispatch](../../internal/cli/main.go) routes directly to separate command handlers | Add shared named-server command parsing while preserving advanced commands |
| Backup setup | [Setup coordinator](../../internal/backup/worker_setup.go) uses SSH, explicit pinned images and local helper artifacts | Resolve artifacts and defaults automatically, expose typed lifecycle operations and connect shared notifications |
| Backup paths | [Host setup](../../internal/backup/worker_setup_host.go) generates configuration and state beneath the target home | Preserve existing layouts; allow reviewed path overrides and deliberate home-filesystem archive storage |
| Notification setup | [Runtime configuration](../../internal/notification/install/cli.go) and [managed installation](../../internal/notification/install/managed_types.go) require local inputs and initialized state | Add remote orchestration, first-install initialization and public configuration compilation |
| Notification enrollment | [Worker configuration](../../internal/notification/worker/config.go) requires at least one producer | Support notification-first setup and later enrollment without fabricated producer identities |
| Signal | [Managed service](../../internal/notification/install/signal_service_types.go) and recovery/adoption workflows retain account ownership | Provide fresh account linking and inbox/service enrollment as an ordinary guided installation |
| LEDs | [LED client](../../internal/notification/led/client.go) implements the pinned daemon protocol; mapping and recovery workflows exist | Add hardware discovery, daemon provisioning, configuration compilation and a targeted enable/disable lifecycle |
| Terminal replacement | [Retirement and replacement](../administration/notifications/retirement-replacement.md) supports settled retirement and fresh enrollment | Present it as a guided exceptional operation, preserving its downtime and identity-change requirements |
| SSH and UI | [Worker transport](../../internal/transport/worker.go), [UI options](../../internal/ui/options.go), and [UI output](../../internal/ui/output.go) | Add structured remote requests, injectable prompts, redaction and consistent progress/status output |

A wrapper that generates shell command strings is insufficient. Several public
behaviors require lifecycle work: an initially empty gateway, first-time Signal
installation, safe LED disablement, shared-producer enrollment and configuration
changes that the current managed installer deliberately refuses.

## Configuration authority and versioning

### What belongs in YAML

YAML owns desired policy: server selection, enabled workers, source selection,
schedules, storage, retention, provider selection, routing, credential references,
LED behavior, command permissions, resource limits, paths and artifact overrides.

Keep the following separate:

| Information | Authority |
| --- | --- |
| User-selected behavior and overrides | `server.yaml` |
| Secret values and provider account databases | Private files on the target; explicit operator-local input files may provision them |
| Worker/account/producer identities, generations, receipts, queues and recovery records | Existing durable runtime stores and ownership journals |
| Resolved target UID/home, selected artifact digests and applied configuration fingerprints | Generated installation and coordinator records |
| Application database backup opt-in and managed service metadata | Existing application declarations and source inventory; worker YAML selects those sources |

Generated files are derived execution inputs and audit records. Editing a generated
runtime JSON must not become an alternative configuration workflow. An installation
record may retain the last applied policy for comparison, but it must not silently
supply a different desired policy when the YAML is missing or changed.

### Schema version decision

Introduce inventory **version 3** for the unified worker schema and its defaults.
Continue accepting version 2 with its existing semantics. This avoids changing
existing retention, timezone, notification or mount behavior merely by upgrading
`deploy`. The unified examples below therefore use an explicit version boundary.

Provide a short migration operation:

```sh
deploy server migrate --to-version 3
```

It produces a reviewable candidate and semantic diff. It does not install services
or overwrite the input without a deliberate save. Migration must materialize old
effective values where version-3 defaults differ, including disabled retention and
the existing timezone. Preserve secret references and existing installation paths.
Detect legacy server aliases that collide with newly introduced command words and
show the explicit `apply SERVER` form instead of renaming them.

The first version-3 defaults are a stable contract. Later CLI releases must not
quietly change effective policy for the same schema. Introduce an explicit defaults
revision or schema migration when a default must change. Artifact releases are
tracked separately from user policy and require a visible update plan.

### Minimal declarations

A normal Telegram installation starts with:

```yaml
version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    notification_worker:
      providers: [telegram]
    backup_worker:
      sources: [vps-main]

  vps-main:
    host: vps.example.com
    user: deploy
```

Signal and Raspberry Pi status LEDs are equally declarative:

```yaml
version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    notification_worker:
      providers: [signal]
      status_leds:
        enabled: true
    backup_worker:
      sources: [vps-main]
  vps-main:
    host: vps.example.com
    user: deploy
```

The provider list is shorthand. Support an equivalent mapping for optional provider
settings, with one normalized representation. Do not support the earlier tentative
`raspi_leds_status` spelling as a second permanent key; document
`status_leds.enabled` as the canonical interface.

A customization example is:

```yaml
version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    notification_worker:
      providers:
        telegram:
          token_file: ~/.config/deploy/secrets/telegram-token
          chat_id_file: ~/.config/deploy/secrets/telegram-chat
        signal:
          account_file: ~/.config/deploy/secrets/signal-account
          recipient_file: ~/.config/deploy/secrets/signal-recipient
      routes:
        alerts:
          primary: telegram
          fallback: signal
      status_leds:
        enabled: true
        daemon:
          mode: managed
        bindings:
          backups:
            producer: backup-worker
            subjects: [backup]
            led: green
      commands:
        enabled: false
      limits:
        max_database_bytes: 536870912
        receipt_audit_days: 90
    backup_worker:
      sources: [vps-main]
      storage:
        path: /mnt/backup/deploy
        require_mount: /mnt/backup
      schedule: hourly
      timezone: Europe/Vienna
      retention:
        keep_last: 48
        max_age: 720h
      notifications:
        worker: manager
        route: alerts
        repeat_after: 6h
        daily_at: "09:00"
  vps-main:
    host: vps.example.com
    user: deploy
```

All field names in these examples must be covered by the schema and executable
configuration fixtures before shipping the corresponding phase. Do not add a raw
runtime-JSON passthrough as a substitute for supported YAML fields.

When both workers are declared on one server, omitted backup notification settings
resolve to that server's notification worker and its `alerts` route. An explicit
worker/route reference overrides this default. A reference to an absent or disabled
worker is an error before mutation. If no notification worker is declared, backup
alone remains a supported choice; status must clearly show whether reporting is
disabled or explicitly configured through an existing supported integration. Never
silently create an undeclared provider or switch delivery mechanisms.

For the short commands, explicit YAML values override versioned schema defaults.
Flags select the inventory, operation and output mode; they must not introduce
unpersisted policy overrides. Guided configuration writes the same YAML fields that
an operator could edit manually. Advanced legacy commands retain their documented
input contract, and later short-command applies detect any resulting drift.

### Presence and removal rules

- An absent worker block means that no worker is requested for a new installation.
  If a managed installation already exists, omission produces a removal review;
  it must not silently uninstall or erase state.
- A present worker block enables that worker by default. `enabled: false` requests
  an orderly stop while retaining configuration, identities and history.
- An empty notification block means that provider selection is missing. Ask in an
  interactive session; report a required field without mutation in automation.
- `status_leds` omitted or `enabled: false` requests no new LED integration. On an
  existing enabled installation it requests the explicit disable transition.
- Provider removal, source removal, route removal and permission expansion are
  semantic changes with their own lifecycle rules; a missing field is not a waiver
  of existing obligations.
- Reject duplicate keys, unknown fields, contradictory shorthand/mapping forms,
  explicit invalid zero values, and ambiguous YAML merges. Errors identify the
  server and full field path.

### Public field coverage

Create a checked field inventory during schema implementation. Every user-owned
field below needs YAML parsing, defaults, validation, compilation and a change
classification. Every omitted low-level field needs an explanation that it is a
runtime fact or internal mechanism.

| Family | YAML coverage |
| --- | --- |
| Backup capture | Sources, source/instance selection supported by the domain, schedule, timezone, freshness, maximum run time, maximum snapshot size, all existing timeouts |
| Backup storage | Archive path, storage mode, required mount, free-space floor, existing retention/pins semantics and restore-check policy |
| Backup reporting | Route selection, immediate failures, reminder interval, recovery notifications, daily report time and monitoring credential references |
| Backup restore checks | Source, instance, pinned image, argument-array command and timeout; preserve current restrictions |
| Notification providers | Enabled providers, secret references, supported account/link settings, fixed destinations or conversations, threads, primary/fallback routes and supported provider runtime overrides |
| Notification storage | Queue/message/payload/database limits, free-space floor and receipt audit retention |
| Producer policy | Named producer references, requiredness, heartbeat/evidence limits, per-producer quota, allowed routes, context capabilities and aliases |
| Interactive commands | Enabled state, read/mutation mode, conversations, actors, worker aliases, permission lists and scope restrictions |
| LED output | Enablement, daemon ownership mode, socket override, bindings, subject selectors, supported patterns/priorities and explicit power-LED opt-in |
| Installation | Supported config/state/data/runtime path overrides, sandbox exclusions, helper/artifact selection and provider/LED artifact overrides |

Commands advertised by a specialist remain derived from its validated contract;
YAML selects permissions and supported policy rather than inventing executable
command descriptors. UUIDs, generation numbers, route digests and recovery cursors
are generated facts. Revisions must be stable for equivalent resolved policy.

## Defaults and paths

Resolve defaults on the target. Never expand worker paths using the operator's
home directory. Respect valid target XDG directories for new installations and
retain already installed absolute paths until a deliberate migration is selected.

| Setting | New installation default |
| --- | --- |
| Config | `$XDG_CONFIG_HOME/deploy/<worker>` or `~/.config/deploy/<worker>` |
| State | `$XDG_STATE_HOME/deploy/<worker>/state` or `~/.local/state/deploy/<worker>/state` |
| Persistent data | `$XDG_DATA_HOME/deploy/<worker>` or `~/.local/share/deploy/<worker>` |
| Notification installation root | `<notification data>/installation`, separate from state and provider account directories |
| Credentials | `<config base>/deploy/secrets`, with private provider files |
| User units | `<config base>/systemd/user`; Quadlets use the appropriate existing container-unit directory |
| API socket | The managed worker state's existing `api.sock` convention; validate Unix socket length before writes |
| LED socket | A stable daemon-managed target path, resolved and pinned during capability discovery; optional YAML override |
| Backup archive | `<backup data>/archive`, in explicit home-filesystem mode |
| Schedule and timezone | Hourly and UTC for version 3; preserve old effective values during migration |
| Backup budgets | 2h freshness for hourly schedules (26h daily, 170h weekly), 40m maximum run, 1 GiB free-space floor and existing bounded timeout/snapshot defaults |
| Retention | Preserve the existing conservative default: no automatic pruning until retention is configured; offer the documented 48-copy/30-day preset during configuration |
| Notification limits | Start from current example limits: 10,000 messages, 64 MiB aggregate payload, 512 MiB database, 128 MiB free-space floor and 90-day receipt audit window; validate against actual field semantics |
| Delivery route | One provider supplies `alerts` automatically; multiple providers require a persisted primary/fallback choice unless explicitly declared |
| LEDs and interactive commands | Disabled unless declared |
| Health and monitoring | Derive producer liveness limits from report cadence; external monitor endpoints remain explicit references and applicable activation checks remain required |

Use separate types for operator-local paths and target paths. SSH key paths and
explicit local secret input files resolve relative to the inventory directory.
Target fields support clean absolute paths and `~/...`; reject arbitrary shell
expansion and ambiguous relative paths. Validate ownership, symlinks, overlaps,
reserved paths and socket lengths after remote resolution.

### Home storage is a real mode

Current backup validation requires an explicit mount path. Implement home storage
as a supported policy and storage-identity check; do not pretend that the home
directory is a mountpoint. Pin the archive directory and its filesystem identity
using the existing archive ownership rules. Mount replacement or an unexpected
archive identity must require reconciliation.

An explicit `require_mount` continues to require that exact mounted filesystem.
If the external disk disappears, stop instead of falling back to the home default.
Free-space protection, archive locks, restricted source keys, retention guards and
restore checks apply to both modes. Status shows the actual archive path/filesystem
and whether retention and external monitoring are configured.

## Short command contract

| Command | Behavior |
| --- | --- |
| `deploy notification-worker manager` | Apply notification desired state or resume the same operation |
| `deploy backup-worker manager` | Apply backup desired state and reconcile its declared notification dependency |
| `deploy <worker> apply manager` | Explicit alias of the shorthand; disambiguates a server named `status`, `plan`, etc. |
| `deploy <worker> plan manager` | Read-only target inspection and proposed semantic diff; no provider sends or mutations |
| `deploy <worker> status manager` | Current installed/runtime/qualification state and one actionable next step |
| `deploy <worker> configure manager` | Edit declared settings and credential references and stage secret inputs through a guided flow; deployment remains a separate operation |
| `deploy <worker> resume manager` | Resume the journaled operation after checking its desired-policy and installation pins |
| `deploy <worker> uninstall manager` | Explicit removal workflow preserving required state, archives and audit history |
| `deploy notification-worker test manager` | Explicit test delivery to the selected configured destination |
| `deploy notification-worker test manager --provider signal` | Explicitly select another YAML-configured fixed destination for a diagnostic; routing remains YAML-owned |
| `deploy --dry-run <worker> manager` | Offline plan with unresolved remote facts clearly marked; no SSH, writes, linking, image pulls or sends |
| `deploy --non-interactive <worker> manager` | Apply only when inputs and required decisions are available; never wait for terminal input |

Keep `--servers PATH` for non-default inventory locations. The normal discovery
rule remains `./server.yaml`; `server.yml` can be selected explicitly. Do not add
implicit parent-directory searches or silently combine inventory files.

Parse named-server and legacy operations centrally in `internal/cli`, using the
union of reserved operation names. Preserve existing `--local`, `--spec`, review,
recovery and internal helper invocations. Report typos as errors, not as an
invitation to initialize a new worker. Define consistent flag placement and add
actual CLI tests, including global options and the two `status` command families.

`--json` emits a versioned, redacted result to stdout and progress to stderr.
Use stable machine-readable reason codes for missing input, required action,
unsupported capability, conflict and operational failure. Keep exit-code behavior
compatible with existing commands; the result code carries the detailed reason.
Do not introduce an unrestricted `--yes` that accepts duplicate-delivery risk,
account replacement, unknown SSH host keys, or destructive retirement.

## Configuration and credential prompts

### Persist decisions without creating hidden configuration

Prompt only for unresolved values. Skip prompts when YAML or an existing private
credential reference provides the answer. A non-secret answer that changes policy
must be written to YAML before its dependent remote operation starts. Secret values
are written only to their declared managed credential files.

Edit YAML through its node representation to preserve comments, order and unrelated
settings. Check the original file digest, acquire the local edit lock, show the
proposed changes once, and save atomically. If the file changes concurrently, stop
with a conflict. For ambiguous anchors/merges or a read-only inventory, emit a
candidate patch/file and identify the short command to rerun after it is saved.
Do not apply an unrecorded interactive policy hidden in a remote JSON file.

Use an injectable prompt interface with terminal detection, secret input without
echo, clear cancellation and bounded input validation. EOF, missing TTY or
`--non-interactive` returns all currently known missing fields before mutations.
Prompts must not become stdin consumers for internal framed helper protocols.

### Credential handling

- Use explicit provider file fields for existing target credentials. Default file
  locations are deterministic and visible in the plan.
- Provide a clearly named operator-local input mechanism for unattended provisioning;
  local and remote references must be different typed fields, mutually exclusive.
  A local input path in YAML is a reference, never a secret value.
- Transfer secret bytes through the authenticated SSH protocol/private descriptors,
  not shell interpolation, command arguments, URLs printed to logs, or environment
  variables. Stage and atomically publish files with mode `0600` and owned private
  parents. Preserve existing credentials unless rotation is requested.
- Redact prompts, progress, JSON, errors, diffs, journals and debug output. Show
  present/missing/changed instead of secret contents or public secret digests.
- Stage rotated credentials first. Fence affected sending/intake before promoting
  credentials that change runtime identity or invalidate admission. Requalify the
  affected profile and retain the old outcome history.
- `configure` may stage a private candidate, but must not replace credentials used
  by a running worker. Only the subsequent apply operation promotes that candidate
  through the appropriate lifecycle transition.
- Account identifiers, native destinations and account database contents must not
  be conflated. Existing phone/selector versus logical identity distinctions remain
  enforced by provider-specific validation.

## Architecture and operation ordering

Introduce a coordinator package such as `internal/workerctl`, with separate modules
for planning, target resolution, prompts, credentials, artifact resolution and
worker adapters. `internal/config` holds public YAML types and pure normalization.
The root CLI calls the coordinator; the coordinator calls typed backup and
notification lifecycle facades. Avoid an import cycle in which `backup.RunCLI`
imports a coordinator that itself imports `backup`.

Refactor only the necessary lifecycle entry points behind narrow exported facades.
Reuse existing journals and validators. The coordinator owns the outer workflow;
it must not replace canonical producer ownership, replay protection or recovery
records with a second simplified state machine.

The flow is:

```text
Read YAML and validate local references
  -> resolve target identity and inspect installed state
  -> collect and persist missing inputs
  -> compile a semantic plan and check required decisions
  -> stage verified artifacts and credential inputs
  -> run journaled lifecycle operations in dependency order
  -> perform applicable checks and authorized test delivery
  -> activate only the admitted capabilities
  -> record the applied policy and show status
```

The durable outer journal binds the normalized desired policy, target UID/home,
existing installation identities, artifact digests and sub-operation references.
Store it under a private target control directory, separate from worker databases.
After reconnection, re-read canonical child completion records before moving on.
A lost SSH response is not evidence that an operation failed or can be repeated
with new identities.

Concurrent CLI invocations must converge on the same lock/intent or report a clear
busy/conflict result. Establish one documented lock ordering across controller,
installation, account, producer and archive locks. Prefer short held locks around
state transitions; do not hold incompatible lifecycle locks across human prompts.
Once approved, revalidate the plan against current files and identities before use.

If YAML changes during a pending operation, never retarget that operation. Resume
its pinned intent when compatible, or present an explicit finish/cancel/replan
workflow. Completed receipts and external outcomes cannot be rolled back by
restoring old configuration bytes.

### First-install dependency ordering

The current gateway needs enrolled producers, while shared backup setup needs a
managed gateway. Resolve this dependency explicitly:

1. Reserve/initialize the backup specialist's durable identity without scheduling
   jobs or enabling legacy delivery.
2. Initialize a gateway in an explicitly unbound verification state when it has no
   consumers yet. Do not invent a dummy producer to satisfy current validation.
3. Enroll the actual reserved backup identity and route through a journaled gateway
   enrollment transition, retaining the gateway identity.
4. Complete backup source enrollment and shared report wiring.
5. Qualify provider/runtime prerequisites and activate the relevant gateway,
   producer generation and backup/report triggers in their supported order.

Define a bootstrap API rather than weakening every `Config.Validate` call. An
unbound gateway may inspect/test configured providers through a bounded diagnostic
operation, but cannot admit arbitrary production submissions or appear healthy
because an expected producer was omitted. Standalone notification installation must
remain valid until a real consumer is added. Adding or removing a consumer later
uses a bounded enrollment transition, not wholesale database reinitialization.

In the initial release, automatic shared transport is for workers on the same
server and UID, matching the existing local socket model. A cross-server gateway
reference must produce a precise unsupported-topology error. Do not imply remote
producer transport merely because YAML can name another server. Such transport is
a separate protocol/security project if later required.

## Artifact and target provisioning

A short command must not require users to discover architecture-specific helper
paths or image digests. Package a versioned artifact catalog with each supported
CLI release: Linux amd64/arm64 static helpers, backup images, supported Signal
runtime artifacts and the LED daemon artifacts needed by managed mode.

Select matching artifacts from the catalog, verify exact digests/capabilities and
cache downloads. Do not default to floating image tags or run a local x86 binary
on the Pi. Support declared version/digest/local-artifact overrides in YAML for
automation and offline installations. Retain selected artifact pins with the
installation; reapplying identical policy must not silently upgrade a service.

The existing build scripts are inputs to this work, not proof that a complete
published catalog exists. Deliver the release packaging and offline bundle before
claiming that a fresh machine supports the no-extra-flags quick start.

Extend the existing bounded SSH bootstrap with a versioned internal control
protocol and an allowlist of operations. The remote helper resolves identity and
paths, verifies files, runs typed lifecycle operations and returns structured
progress. Authenticate through the existing SSH known-host policy. New host keys
require explicit verification rather than automatic acceptance.

Probe architecture, user manager, rootless runtime, storage, required tools and
service permissions before altering the target. Report actionable prerequisite
steps. Installing host packages, enabling linger or granting LED hardware access
may require an administrator; show the concrete change and continue the same
workflow after it is performed. Do not silently escalate privilege or edit
unrelated system services.

## Provider setup and activation

### Telegram

Guide the user through supplying the bot credential and choosing the intended
chat/conversation. Persist the reference and destination configuration, then
validate the actual configured identity and destination. Choosing Telegram must
not grant interactive command permissions by default. Offer an explicit test
message with a clear destination and record its real result.

For credential rotation, preserve the logical account and frozen message audience
when that is the intended operation. A changed native account or audience must be
classified as an enrollment/routing change, not treated as a harmless file edit.
Never replay old queued content to a newly selected destination automatically.

### Signal

Implement a fresh-install path that can install the supported runtime, create
private account state, guide device linking, initialize the canonical inbox and
admit the single-owner service. The user should not need a previous backup relay,
a fabricated recovery epoch, or handwritten service/recovery JSON to install a
new Signal notification worker.

Separate fresh linking, adopting an existing account, relinking, service update and
recovery in the plan. Preserve the account lease across supervised children and
retain the linking/recovery outcome when interrupted. An existing legacy backup
account must use the implemented adoption and handoff protocol, including its
explicitly incomplete history where applicable.

If linking cannot finish without a device interaction, show `needs_action` with the
linking step and the same short resume command. Never log private linking payloads
as ordinary progress or silently register a different account on retry.

### Qualification and activation

Convert machine-verifiable checks into coordinator actions using existing
qualification/admission APIs. Store observed results against the exact runtime,
credential and artifact pins. Required human/provider/hardware observations remain
explicit steps; the wizard must not manufacture passing qualification records.

If required monitoring endpoints or other inputs are missing, ask for them and
persist references. If a required observation cannot be performed, finish with a
truthful `configured` or `needs_action` state and a short next command. A successful
file copy is not an active backup schedule or verified notification route.

Test sends require the user's explicit test action or a scoped unattended test
request. Persist the attempt so resuming a lost response cannot silently send the
same test repeatedly; ambiguous results require a decision just like other sends.

## Raspberry Pi LED setup

`status_leds.enabled: true` must be sufficient to start guided setup. Detect the
actual board and supported LEDs, select the qualified status output, and manage
the daemon or attach to an explicitly declared existing daemon. Do not guess a
sysfs path from the hostname. Power-LED use remains a separate explicit opt-in.

Managed mode provisions a pinned compatible daemon and the minimum required device
permissions. Existing-daemon mode verifies protocol, socket ownership and relevant
capabilities without replacing that daemon or changing its other clients. Generated
binding IDs must be deterministic and connected to enrolled producer identities.

The default mapping presents aggregate worker/backup status. Custom bindings,
subject selectors, patterns, priorities and socket locations belong in YAML.
Start with unknown/baseline output; enable healthy leases only when the existing
evidence and liveness rules allow them. Registry acceptance and physical visual
verification remain different observations in status.

### Disabling and re-enabling

Implement a targeted LED disable transition; retiring the whole notification
installation is not an acceptable normal response to a boolean change. Fence LED
refresh first, retain the exact old UID/socket/bindings, reconcile authorized
cleanup, and confirm removal of owned output before marking it disabled. Preserve
other applications' states and services.

Active faults cannot be silently declared resolved. If existing cleanup authority
is sufficient, use it. Otherwise either collect the required domain decision or
implement a separately audited output-disable disposition that preserves the fault
ledger and incident state without claiming recovery. This lifecycle work is a
required part of the LED milestone, not a YAML-parser-only task.

If the daemon is unreachable, retain cleanup-pending state and a resumable action;
do not report successful disablement. Re-enable with retained identities and
current fault state, then re-establish fresh evidence. Remove a managed daemon only
when no retained obligations or other clients depend on it. Restore owned hardware
configuration only when it still matches the installed state.

## Applying changes and preserving compatibility

The [shared configuration plan](shared-configuration.md) specifies the current
state/change matrix, read-only planner outcomes and SC6–SC8 dependencies for
completing local shared gateway/backup updates after ordinary YAML step 54.
SC1–SC5 planning, producer policy, routing, derived freshness, bounded
provider/reference changes and shared activity are implemented.

Compile a semantic diff before deciding how to apply it. Do not compare only raw
YAML bytes, and do not infer every runtime transition from a changed global hash.

| Change | Required behavior |
| --- | --- |
| Comments, key order, equivalent shorthand or same policy | No runtime changes |
| Backup schedule, supported budgets or retention | Existing journaled backup update with explicit retention effects in the plan |
| Same-account credential rotation | Stage secret, fence affected use, rotate atomically and requalify without replaying outcomes |
| Provider destination, route or fallback order | Preserve frozen audiences and pending obligations; perform a supported route/profile migration or require an explicit disposition |
| New/removed producer or source | Journaled enrollment or retirement; never silently drop heartbeats, fault state or unacknowledged receipts |
| Command alias, permissions or conversations | Use supported command migration; additions/expanded rights need explicit new admission and pending-input fences |
| LED bindings or enablement | Dedicated mapping/disable lifecycle with retained ownership evidence |
| Queue/database/audit limits | Validate existing usage and retained history; reject impossible reductions rather than evicting required evidence |
| Worker `enabled: false` | Stop dependent production activity in order and preserve resumable state; do not retire identities automatically |
| UID, canonical state path or native-account lifetime change | Guided supported migration or settled retirement/replacement; explain downtime, fresh enrollment and preserved archive |
| Missing/corrupt state or unexpected installed files | Stop with recovery/drift guidance; never repair by initializing an empty database |
| Artifact upgrade | Pin the new release, verify capabilities/schema, preserve helper compatibility and use existing update/rollback constraints |

Implement missing bounded transitions needed for ordinary supported settings;
list them explicitly in the work packages. Do not advertise arbitrary live
reconfiguration merely because every setting can be declared in YAML. Ordinary
budget or mapping changes should not force a new messenger account. Genuine
identity replacements retain their existing stronger prerequisites.

Existing advanced commands remain available. Add an inventory import/review path
for manually installed workers: inspect owned manifests, reconstruct representable
policy without exposing secrets, preserve UUIDs/epochs/paths, and generate a YAML
candidate. Verify ownership before taking over. A directory that merely resembles
the default installation is not proof that it belongs to this tool.

Keep provider account and archive history when a worker is uninstalled. Shared
notification uninstall must identify dependent backup producers and guide their
orderly transition. Never silently fall back to the legacy backup sender when the
shared gateway is missing. After shared cutover, preserve the legacy sender fence.

## Status and support output

Human output should distinguish `not_installed`, `configured`, `needs_input`,
`needs_action`, `verification`, `active`, `degraded`, `disabled`,
`cleanup_pending`, `recovery_required` and `up_to_date`. These are presentation
states derived from authoritative lifecycle and health records; they do not create
new delivery authority.

Show the server, worker, effective paths, active schedule/routes, missing inputs,
applicable checks, last successful operation and a concise next action. Keep hashes
and internal IDs in private diagnostics or redacted verbose output. Provider
acceptance, recipient visibility and physical LED output must remain distinct.

A successful example would read:

```text
manager: notification worker active
  Telegram: test accepted for the configured destination
  Status LEDs: disabled

manager: backup worker active
  Archive: /home/deploy/.local/share/deploy/backup-worker/archive
  Sources: vps-main
  Schedule: hourly, UTC
  Notifications: local notification worker / alerts
```

If a check is incomplete, replace the corresponding success line with the actual
state and next step. Never print this success transcript unconditionally at the
end of a setup command.

## Implementation work packages

Each package should be reviewable independently, with regular focused tests and
updated examples. The sequence exposes dependency problems before building a
large wizard around unsupported transitions.

| Package | Deliverables | Dependencies | Completion check |
| --- | --- | --- | --- |
| U1 Configuration contract | Version-3 DTOs, complete field inventory, defaults, presence rules, shorthand and semantic normalization, v2 migration fixtures | None | Both minimal YAML examples normalize deterministically and identify missing inputs; v2 behavior is unchanged |
| U2 Target and artifact resolution | Remote facts, XDG/home paths, storage modes, artifact catalog/bundles, capability probes and dependency diagnostics | U1 | amd64/arm64 fixtures resolve correct artifacts and target paths without hand-supplied digests |
| U3 Planner and remote control | Typed facades, framed SSH control, semantic diff, outer journal, lock ordering, reconnect/resume and redacted status | U1, U2 | A simulated interrupted apply resumes without duplicated state or changed identities |
| U4 Guided configuration and credentials | TTY prompts, cancellation, YAML AST edits, concurrent-edit guards, secure credential staging, unattended input and dry-run behavior | U1, U3 | Guided and unattended runs produce equivalent policy; no secret leaks or hidden policy |
| U5 Telegram and gateway bootstrap | Empty verification bootstrap, real producer reservation/enrollment, Telegram setup/test/admission and short status commands | U2–U4 | Notification-first setup and later consumer enrollment keep one gateway identity |
| U6 Shared backup orchestration | Home archive mode, source enrollment, generated runtime/keys, dependency ordering, report routing, activation and legacy adoption | U5 | Either short command order converges on the same working declared installation |
| U7 Fresh Signal setup | Linking, canonical inbox initialization, single-owner supervised service, existing-account adoption and resumable outcomes | U2–U5 | Fresh and adopted-account scenarios work through short commands without fake recovery records |
| U8 LED lifecycle | Hardware capability discovery, daemon ownership modes, YAML mappings, enable/disable/re-enable and cleanup evidence | U3, U5 | Turning the YAML setting off removes only authorized output and preserves unresolved fault state |
| U9 Configuration migration and import | Complete change classifier, missing ordinary quota/route/enrollment transitions, permission admission, import of existing installs and guided exceptional replacement | U5–U8 | Every public field has an apply/no-op/migrate/required-action behavior with executable checks |
| U10 CLI and documentation completion | Short command dispatch, consistent help/results, examples, migration guide and operator walkthroughs | U1–U9 | Users need no runtime JSON, manually copied hashes or advanced lifecycle flags for supported normal workflows |
| U11 Final qualification | Full regular suite/vet, then race suite; isolated target workflows and separately recorded live observations | U1–U10 | Automated evidence recorded against the final source; manual outcomes remain explicitly qualified or pending |

During U1, record the exact mapping from every existing `BackupWorkerConfig`,
notification runtime and enrollment field to YAML or generated state. During U9,
review that inventory again so less visible settings such as receipt retention,
producer evidence limits, restore-check commands and sandbox exclusions are not
left behind.

## Test strategy and acceptance

Use regular targeted tests during implementation and `make test` for the full
regular suite plus vet. Run `make test-all` only after the usability implementation
and regular checks pass, following the user's testing preference. Existing ongoing
or historical race results do not qualify newly changed code automatically.

Use injectable prompt streams, an in-process coordinator transport, simulated
service managers and synthetic provider/LED adapters for routine tests. Add real
subprocess/SSH/socket tests where they verify process lifetime, credential transport,
filesystem permissions or interruption behavior that mocks cannot establish.

| ID | Required acceptance scenario |
| --- | --- |
| UX01 | Minimal Telegram YAML plus the short notification command completes the supported guided setup without handwritten JSON or hashes |
| UX02 | Minimal backup YAML plus the short backup command creates the local declared notification dependency and real producer enrollment |
| UX03 | Notification-first and backup-first command orders converge; an unbound gateway never invents a producer or reports missing consumers healthy |
| UX04 | Repeating unchanged YAML causes no new identities, revisions, source keys, restarts or provider test sends |
| UX05 | One-token provider shorthand and its mapping form have the same effective policy and route revision; resolved routes are not hashed repeatedly |
| UX06 | All user-owned settings in the field inventory can be declared, validated and classified from YAML |
| UX07 | Explicit overrides win; target home/XDG values resolve remotely; operator-local secret/SSH paths remain local |
| UX08 | Home archive mode works on a root filesystem; an explicitly required external mount never falls back when missing or replaced |
| UX09 | A Pi/arm64 target selects valid static artifacts; missing/incompatible catalogs and offline bundles produce useful errors before mutation |
| UX10 | Missing inputs prompt once; supplied inputs do not prompt; cancellation/EOF and unattended mode terminate cleanly without partial activation |
| UX11 | YAML edits retain unrelated content and detect concurrent changes; read-only/ambiguous inventories produce a candidate rather than hidden policy |
| UX12 | Secret bytes never appear in YAML, argv, logs, normal/JSON errors or journals; private-file permissions and rotation ordering are verified |
| UX13 | Telegram destination validation and explicit test delivery retain outcome evidence; lost responses do not trigger automatic duplicate sends |
| UX14 | Fresh Signal linking, interrupted linking and existing-account adoption retain one account owner and do not require fabricated history |
| UX15 | Both providers preserve configured primary/fallback order, account identity and frozen message audiences through updates |
| UX16 | LED enablement on supported hardware discovers/provisions the right daemon; unsupported hardware returns a specific action without silent success |
| UX17 | LED disable/re-enable, lost clear results and daemon unavailability preserve ownership, foreign states and unresolved incidents |
| UX18 | Shared backup cutover and later configuration updates preserve the old sender fence; a missing gateway cannot activate a legacy sender |
| UX19 | Schedule/retention/budget changes apply through their supported lifecycle; invalid reductions and pending obligations are not silently discarded |
| UX20 | Source/producer removal, new command rights and audience changes require the corresponding admission/disposition and preserve replay fences |
| UX21 | Missing/corrupt state, unknown installations and manual file drift never cause silent initialization or ownership takeover |
| UX22 | Existing v2 inventories and advanced CLI commands retain behavior; v2 import/migration preserves old defaults, identities and paths |
| UX23 | `--dry-run` performs no SSH, writes, downloads, linking or sends; live `plan` and `status` remain read-only |
| UX24 | SSH disconnect, process death and lost completion responses at durable boundaries resume the same operation and preserve externally observed outcomes |
| UX25 | Concurrent CLI sessions and mid-operation YAML changes cannot retarget a pinned operation or produce two owners |
| UX26 | Disabled/removed workers retain state; notification uninstall respects dependent producers; exceptional replacement explains its identity and downtime consequences |
| UX27 | Actual short CLI invocations parse global/worker flags and reserved-name aliases correctly, including versioned JSON output |
| UX28 | Automated checks cannot mark unperformed provider, monitor, hardware or host observations as passed; status supplies the remaining short action |
| UX29 | A clean disposable ARM64 host walkthrough completes the declared install/update/resume/uninstall lifecycle with the packaged release artifacts |
| UX30 | Final regular tests/vet pass before the race gate runs; exact source and qualification evidence are recorded |

UX29 needs a disposable ARM64 environment. Real Telegram/Signal accounts, physical
LED visibility and external monitor observations remain explicit manual or opt-in
qualification, consistent with the user's existing decision to test live systems
manually. Automated fixtures must still cover the complete coordinator software.

## Documentation and completion boundary

Make the README's primary worker instructions use the minimal `server.yaml` and
short commands. Add complete configuration reference tables, provider setup guides,
a Pi walkthrough, unattended provisioning, credential rotation, v2 migration and
recovery troubleshooting. Retain advanced runbooks in an administration section
and link to them from exceptional recovery diagnostics.

Create configuration examples beside the existing worker examples and validate
them through the actual parser. Audit outdated help and status text while changing
the relevant commands. Generate or check the reference against public structs so
configuration coverage cannot silently diverge from implementation.

The milestone is complete when the minimal declarations support the promised
short-command workflows, the complete field inventory is accounted for, all
software acceptance scenarios pass, and required external observations are clearly
reported. A YAML parser plus aliases for the old long commands is an intermediate
step, not completion of this usability work.
