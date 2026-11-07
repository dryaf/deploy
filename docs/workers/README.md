# Worker setup and configuration

For a first installation, follow [getting started](../getting-started.md).
This guide is a reference for short commands, configuration changes and lifecycle
behavior. Example files remain in [examples/workers](../../examples/workers);
repository build commands run from the repository root.

Version-3 YAML supports fresh Telegram and direct-alert Signal setup, standalone
backups, and a backup worker sharing its local notification worker. Setup starts
in verification mode: production delivery, command access and backup schedules
require their separate checks and activation. Reapplying unchanged settings
preserves identities and avoids restarting unchanged services.

Not every configuration change is implemented yet. Check the [roadmap](../roadmap.md)
and the supported transitions below. Existing version-2 inventories and advanced
commands retain their behavior. For an existing installation, review
[import and adoption](import.md) before changing ownership.

Optional features have their own guides: [Signal](signal.md), [LEDs](leds.md),
[Telegram commands](commands.md), and [paths](paths.md).

## Choose a configuration

Start with [standalone backup](../../examples/workers/backup-only.yaml), [Telegram](../../examples/workers/telegram.yaml), [fresh Signal](../../examples/workers/signal.yaml), [Signal with LEDs](../../examples/workers/signal-leds.yaml), or the
[customized example](../../examples/workers/customized.yaml). Save the chosen inventory as `server.yaml`,
then preview it without SSH or writes:

```sh
deploy --dry-run notification-worker manager
deploy --dry-run backup-worker manager
```

Use `--servers PATH` to select a different inventory, including `server.yml`. Short
commands accept this option before or after the command. They do not search parent
directories or merge inventories.

Existing managed unbound Telegram gateways can be imported for review and then
explicitly adopted; see [import and adoption](import.md). Shared/Signal takeover
remains separate.

## Command reference

Replace `manager` with your server alias. Commands operate on the worker settings
in the selected inventory. Use `notification-worker` or `backup-worker` as shown.

| Task | Command | Notes |
| --- | --- | --- |
| Apply settings | `deploy notification-worker manager` / `deploy backup-worker manager` | Prompts for missing inputs; supported changes only |
| Resume | `deploy notification-worker resume manager` / `deploy backup-worker resume manager` | Continue the same operation with unchanged YAML |
| Inspect | `deploy notification-worker status manager` / `deploy backup-worker status manager` | Read installed state and remaining actions |
| Preview changes | `deploy notification-worker plan manager` / `deploy backup-worker plan manager` | Read-only target inspection; requires SSH |
| Fill missing inputs | `deploy notification-worker configure manager` / `deploy backup-worker configure manager` | Update YAML and private credential references |
| Test a provider | `deploy notification-worker test manager` | Explicit send; optionally select `--provider signal` or `--provider telegram` |
| Record observations | `deploy notification-worker qualify manager` / `deploy backup-worker qualify manager` | Record real evidence; does not perform tests |
| Request a backup | `deploy backup-worker run manager` | Check completion before activation |
| Enable the backup schedule | `deploy backup-worker activate manager` | Requires current qualification and a successful manual backup |
| Review an existing installation | `deploy notification-worker import manager` / `deploy backup-worker import manager` | Print a YAML candidate without taking ownership |
| Remove a supported installation | `deploy notification-worker uninstall manager` / `deploy backup-worker uninstall manager` | Read [lifecycle limits](#disabling-and-removing-workers) first; shared removal is incomplete |

Add `--dry-run` for an offline preview without SSH or writes. Use
`--non-interactive` for automation and `--json` for versioned structured output.
For inventory migration, see [server inventory](../reference/server-inventory.md).

## Planning and status

Live `plan` compares YAML with the completed installation's retained declaration
and prints changed field names with their lifecycle classification. Values such as
credential references and destinations are omitted. Backup archive/path changes
and monitoring changes are identified separately from ordinary backup updates;
derived freshness can change separately through the coordinated workflow below.
Backup plans also compare each retained source's host, user and SSH port with its
server inventory entry. Changed endpoints report `source_migration_required` and
field names without printing their values or contacting source hosts. Host-key
trust and source helper artifacts are checked during apply; endpoint planning
does not qualify or perform a source migration. Status also reports changed
endpoints. Run, qualification and activation compare inventory endpoints under
the setup lease before starting a backup or recording lifecycle evidence.
Matching policy is reported as `up_to_date`, which does not imply that qualification
or service health is current. Drift, pending operations and missing snapshots
require review. Older development installations may need import before a complete
diff is available. The SSH inspector can stage a temporary helper, but plan does
not change retained worker files, credentials, journals or service state. Add
`--dry-run` for a completely offline preview without SSH or target inspection.

## Configuration and credentials

`configure` preserves comments and unrelated settings. It asks only for missing
inputs. Credential questions accept an existing absolute/`~/...` target path,
`local:PATH` for an operator file, or `new` for hidden terminal input. New secret
values are stored in private files beneath the operator's
`$XDG_CONFIG_HOME/deploy/worker-inputs` (normally `~/.config/deploy/worker-inputs`).
YAML receives file references, never the secret bytes. These files are staged
inputs. Apply transfers them over the bounded SSH stdin protocol into private
target files; it refuses implicit rotation of existing credentials. Private input digests also
detect changes to target-referenced credentials that leave the YAML unchanged. A cancelled session can leave an unreferenced
private input; it never activates a worker or overwrites an active credential.

A read-only inventory produces a YAML candidate on stdout instead of being
modified. New secret staging requires a writable inventory. Concurrent edits
cause a conflict; the other editor's content is retained. Version 3 rejects YAML
anchors, aliases and merges to keep editing unambiguous.

Use `--non-interactive` for automation. Missing declaration inputs are listed
before SSH or mutations for apply operations. `--json` produces a versioned plan
or status result. With a matching helper bundle, notification status checks the
coordinator receipt, canonical installation, service and runtime identity. An
unbound gateway reports `waiting_for_producers`, with unknown production health.
Without a helper bundle, inspection reports filesystem/service observations and
explicitly leaves domain admission unverified. Existing state with a missing
manifest requires recovery review. No status result substitutes for provider or
hardware qualification.

## Disabling and removing workers

Removing a worker block from YAML does not uninstall its services. The short
apply/resume, plan/status and uninstall forms then perform only a read-only
presence review. If files or a service remain, restore the previous declaration
(including custom paths), inspect status, and use explicit uninstall. If default
paths and the service are confirmed absent, the result is `not_requested`.
Unavailable service inspection remains `presence_unverified`; no setup or removal
is inferred from it. Offline `--dry-run` reports the omission without SSH.

Unbound notification uninstall clears its owned LED output before removing the
gateway service. If cleanup is pending, restore daemon access and repeat
`deploy notification-worker uninstall manager`. It retains incident history and
foreign registry entries. For an owned managed daemon it stages an administrator
removal review; run the displayed command on the Pi, then repeat uninstall to
verify its completion. Host registry/configuration/history remain retained.
Existing-mode daemons remain untouched. See [LED setup](leds.md) for details.

For an unbound gateway, change only `notification_worker.enabled` to `false` and
run `deploy notification-worker manager` to disable its service. Owned LED output
is cleared first; identities, incidents, configuration and credentials remain.
Set it back to `true` and apply to restore verification with the same identity.
An interruption reports `activity_pending`; use `resume manager` with the same
YAML. Repeated apply performs no new restart. A native Signal account service, if
installed, remains running during this gateway disable; explicit `uninstall`
removes both services. An explicit uninstall cannot be undone by ordinary apply.
For a standalone backup, change only `backup_worker.enabled` to `false` and run
`deploy backup-worker manager`. This fences capture and stops/disables the owned
units while retaining archives, source access keys, identity and history. Set it
back to `true` and apply to return to staged mode. Timers stay disabled until
current qualification, a successful manual backup and explicit
`deploy backup-worker activate manager`. Interrupted activity uses the same
`resume manager` command and YAML; it does not repeat a confirmed stop.
Bound gateways and shared backups still require their coordinated activity
workflow. Do not combine an activity switch with other policy edits.

The schema reference and current limitations are in
[worker configuration reference](../reference/worker-configuration.md).
Initial custom LED bindings for the local backup worker can be declared before
setup. They wait for a real producer, then apply through a separate resumable
mapping transition before qualification. See [the LED guide](leds.md) for the
YAML and supported daemon/device combinations.

## Local artifact bundles

```sh
make worker-bundle
```

This builds static Linux amd64/arm64 helpers with the pinned repository Go version
and writes `dist/workers/catalog.json`. It does not publish a release, push images,
fetch credentials or install anything remotely. It can run without registry
access when the Go dependencies are already available.

A qualified release pipeline supplies pinned images through
`WORKER_BACKUP_IMAGE_AMD64`, `WORKER_BACKUP_IMAGE_ARM64` and, when used,
`WORKER_RELAY_IMAGE_AMD64`/`WORKER_RELAY_IMAGE_ARM64`. Optional local Signal and LED
artifacts use `WORKER_SIGNAL_AMD64`/`WORKER_SIGNAL_ARM64` and
`WORKER_LEDS_AMD64`/`WORKER_LEDS_ARM64`. LED inputs must be the verified offline
zipapp; `WORKER_LED_SOURCE` builds it from the pinned clean upstream checkout
for both architectures. See [the LED guide](leds.md) for the reviewed host
prerequisite. `WORKER_RELEASE` labels the catalog.
The assembler validates helper and supplied native Signal ELF architecture,
static helper linkage, LED archive contract and image reference syntax before
publishing a catalog. Artifact names include their SHA256; an interrupted build
leaves the previous catalog and its referenced files usable. Rebuilding resumes
by verifying those files, and refuses changed files at an existing digest name.

Development bundles report missing optional inputs explicitly. A release pipeline
can require helpers, Signal, LEDs and both image references for both architectures:

```sh
WORKER_BUNDLE_COMPLETE=1 make worker-bundle
```

Missing or incompatible inputs fail before catalog publication. This checks
packaging completeness; it does not download/push images or qualify a release on
a Pi or with a provider. Copy the whole bundle directory when moving a catalog;
retain its referenced digest-named files.

A bundle can be placed in `workers/` beside the CLI, selected explicitly with
`artifacts.catalog`, or used from `dist/workers` during development. Catalog paths
are relative to the catalog; YAML catalog/helper/input-file paths are relative to
the inventory. Helpers are checked for digest, architecture, executable entry
point and static linking. Floating backup/relay image tags are rejected.

## Testing

Use focused regular tests while implementing and `make test` for the complete
regular suite plus vet. Run `make test-all` after the usability implementation and
regular suite are complete. Live Telegram/Signal, physical LED and disposable-host
qualification remain separate operator checks.

On rootless Linux with Bubblewrap/user namespaces and an ext4 workspace,
`make test` also runs the backup/shared CLI through disposable manager and source
homes. The fixture checks both installation orders, lost enrollment replies,
unchanged service lifetimes, backup policy updates and missing-journal refusal.
It mounts test service/container/SSH adapters and isolates external networking;
its container and source-restriction evidence is synthetic. The test skips with
an explicit reason when those local prerequisites are unavailable. These checks
do not replace the separate live, hardware or VM qualification gates.

## Bootstrap and enrollment boundaries

The gateway requires a rootless Linux target, trusted SSH, Python 3, a user systemd
manager and lingering. The helper is checked for digest and architecture before
transfer. Installation holds an outer operation lock and uses the domain's
managed-service journal. Policy or artifact changes during an unfinished operation
are conflicts, not permission to retarget it.

An unbound database has no dummy producer, cannot activate delivery, and retains a
reserved gateway identity across interrupted publication. Private unfinished
staging attempts are retained for diagnosis (at most 16 per bootstrap). A missing
reservation, missing completed state or changed ownership requires recovery review.

Before producer enrollment, edit `notification_worker.limits` and run the same
short command to change queue, payload, database, free-space or receipt-audit
limits. `deploy notification-worker plan manager` lists the affected fields;
`deploy notification-worker manager` applies the reviewed YAML. The update checks
existing database/WAL usage and the durable outcome reserve, stops verification,
records the new limits separately from the original bootstrap reservation, and
restarts verification with the same gateway and provider identities. It does not
send a test message or activate delivery. Later backup enrollment uses the revised
limits. Retry with unchanged YAML after interruption; missing history is an error.

Apply limit changes separately from provider, route, artifact and activity changes.
This transition supports unbound gateways, including completed LED output
disable/re-enable dispositions. A versioned receipt pins the original output
record and its history without replaying daemon changes; disabled output stays
disabled and faults remain retained. Pending or missing output authority blocks
the update. Initial backup enrollment also preserves that completed output disposition,
including after limit updates; the producer remains inactive until qualification.
For a local shared backup that is still awaiting delivery qualification, the same
command also applies `notification_worker.limits` after producer enrollment:

```sh
deploy notification-worker plan manager
deploy notification-worker manager
```

Both workers retain the revised YAML policy, producer identity, source keys and
queued work. Reducing a budget below retained queue or storage usage is refused.
The automatically derived backup queue quota follows `limits.max_messages`;
explicit producer quotas stay unchanged. Configured Telegram commands stay
unadmitted until qualification and the separate command-admission step.

This shared transition requires no prior delivery activation or recovery. An
existing LED endpoint, mapping and completed output disposition stay unchanged;
disabled output stays disabled, and unresolved faults are retained. Recovery of
migrated history still needs its separate workflow. Run qualification
for the revised configuration before delivery admission. Interrupted applies
resume with the same YAML; helpers must support the retained budget history.

For an already delivering, unrecovered gateway without command history, change
only `notification_worker.limits` and use:

```sh
deploy notification-worker plan manager
deploy notification-worker manager
deploy notification-worker qualify manager
deploy notification-worker manager
```

The first apply reviews retained queue and storage usage, pauses delivery and
changes the effective budgets. Reporting is briefly fenced while both workers
record the new policy. The original delivery owner, active producer generations,
queued messages, credentials, native Signal service and LED disposition remain.
Record fresh qualification before the final apply resumes delivery. Use
`deploy notification-worker resume manager` with the same YAML after interruption.
You can alternate completed budget changes and supported credential rotations;
keep those edits separate. Recovered gateways still need their later migration
workflows.

For an unrecovered Telegram gateway with already admitted backup commands:

1. Set `notification_worker.commands.enabled: false` and apply it before changing
   limits. Keep the command mode, aliases, conversations and permissions declared.
2. Edit only `notification_worker.limits`, then use the plan → apply → qualify →
   apply sequence above. The update retains the original receiver, permission
   restrictions, pending commands, saved results, original reply destinations and
   reporting ownership. Existing LED settings and other clients' output remain.
3. Delivery resumes with command intake disabled. To enable it, set
   `notification_worker.commands.enabled: true` and apply explicitly.

Both read and mutation command modes support repeated budget changes and later
qualification renewals. Installed helpers must support command-preserving budget
changes; otherwise review refuses the change before pausing delivery. A retry must
use the same YAML. Reductions below retained message or database usage are refused.
Global message limits also adjust a producer's derived message limit. Command
queue budgets are separate and are not changed by `notification_worker.limits`.
Keep provider, audience, command-policy and backup edits separate from this step.

You can alternate completed policy updates and LED mapping updates before
activation. Apply one kind of change at a time; mapping edits require enabled
output. The worker verifies the intervening history and retains unresolved
faults. Apply backup-only edits, such as a new schedule, before changing gateway
policy, LED mappings or output permission. An unapplied backup edit is reported
before a gateway operation is retained.

Before delivery activation, the same gateway also accepts changes
to the local backup producer's `max_silence_seconds` and `max_evidence_seconds`.
These control how long a heartbeat or evidence may remain current. Edit them in
`notification_worker.producers.backup-worker`, keep all other producer settings
unchanged, then run `deploy notification-worker manager`. The change preserves
recorded observation times and requires qualification for the revised effective
configuration. Explicitly writing the existing effective defaults records the
YAML declaration without restarting the gateway or renewing observations.

To move staged backup reports to another route, first add that route under
`notification_worker.routes` and apply the notification worker. Then change only
`backup_worker.notifications.route` and apply either worker. The operation retains
the producer identity, source keys and backup runtime. With the default producer,
its route permission changes too, so pending messages must settle first. With an
explicit producer declaration, grant the route in its `routes` list first; selecting
an already allowed route records the reporting choice without restarting the
gateway. Qualification and activation remain separate. Activated and recovered
installations still require their migration workflows.

The same pre-activation workflow accepts the existing producer's explicit queue
quota, requiredness, allowed routes, policy-health setting and context capabilities.
The producer ID and epoch stay fixed. Quota reductions must fit retained messages;
changes to routing, requiredness, policy health or context access require a settled
queue. The backup reporting route must remain allowed. Context capability changes
do not admit command intake or create interactive alert destinations.

For an already delivering, unrecovered gateway without command history, edit
only `notification_worker.producers.backup-worker` and use the same
plan → apply → qualify → apply sequence shown for production budgets above.
Timing and quota changes preserve queued messages and original observation times.
Routing, requiredness, policy-health and context changes require the queue to
settle first; the active backup reporting route must remain allowed. The original
producer identity, delivery owner, native Signal service and LED disposition stay
in place. Reporting is fenced while both workers record the revised policy.
Resume an interrupted apply with the same YAML. Qualification covers the changed
configuration; it does not refresh old observations. Recovered gateways,
command-bearing gateways remain separate workflows.

If an explicit producer declaration exactly matches the current derived policy,
apply records the declaration in both workers without restarting delivery,
changing observations, draining the queue or renewing qualification. Removing
that explicit declaration works the same way when the derived defaults still
match. `plan` identifies this case. Normalized budget and single-provider route
defaults already compare equal to their omitted forms. A later effective change
still follows its pause/qualify/apply workflow.

A new explicit producer declaration replaces the derived defaults, so specify the
intended settings and review `plan` first. These settings can be present at the
first install with Telegram, fresh Signal, or both. Notification-first setup keeps
the gateway unbound until the backup worker reserves its actual identity; it does
not invent an active producer from the YAML name.

If you use the default producer, change only `backup_worker.freshness` and run either `deploy backup-worker manager` or
`deploy notification-worker manager`. The same operation updates the backup health
deadline and gateway evidence deadline. Both runtime files and the archive
checkpoint retain their identities and history. A running backup reader blocks
publication; retry when it finishes. An interrupted publication fences backup
runtime access until the same YAML is resumed. Complete qualification for the
revised configuration before activation. If you declare the producer explicitly,
its evidence deadline is independent: apply backup freshness and explicit producer
timing separately. On an unrecovered, command-free gateway with active delivery,
the same apply also pauses delivery and stages backup capture. Qualify the
notification worker and apply to resume delivery; complete backup qualification
and activate separately to restart scheduled capture. Reporting keeps its original
owner, and unchanged owned Signal and LED settings are retained. Recovered and
command-bearing installations still need their separate migration workflows.

When both workers are declared on the same server, `backup-worker manager`
reserves the backup producer before source enrollment, applies the notification
dependency, enrolls that real producer, and stages backup. Running the notification
command first creates an unbound gateway; later enrollment retains that gateway's
identity. The managed transition stops verification service before changing state
and resumes after interruption. Producer generations initially remain inactive.

Shared backup staging writes a permanent archive-control fence against direct
sending and installs no direct notifier or provider-test units. Pending incident
messages remain in the backup outbox. This first result is
`qualification_required`, not an operational notification route.

## Verification and activation

Record completed observations with `notification-worker qualify manager`. For each
pending check, choose `pending`, `passed` or `failed`. A passed/failed observation
requires a local evidence file (a log, screenshot or test report, up to 16 MiB);
only its digest travels to the private target report. The wizard does not run a
provider test or infer a passing result. Each answer is saved, so cancellation or
EOF leaves a resumable draft. A partial report leaves checks pending; failed checks
can be retried in a new report, and current passing observations keep their original
expiry window. `--non-interactive` never fabricates unattended evidence. The
[specialist qualification workflow](../administration/notifications/delivery-activation.md)
remains available for scripted reports.

For a gateway with LEDs, the same wizard also asks for the observed daemon
version and an operator-local copy of the daemon configuration used during the
checks. Only the configuration digest is stored. Leave the version blank to defer
this part; the report stays incomplete until both details and all required checks
are present. These details remain separate from physical visibility, timing and
restart observations. Resuming keeps the original evidence window and cannot
silently replace recorded daemon details.

Once current qualification is recorded, reapply can admit the gateway and connect
the reserved backup producer's report service:

```sh
./deploy notification-worker manager
# or: ./deploy backup-worker manager
```

This can start production reporting, including queued incidents. The coordinator
freezes the exact acknowledged qualification record before activation. Missing,
stale, changed or incomplete evidence leaves the gateway in verification mode.
It never performs or marks manual observations for you. Status lists the remaining
checks without exposing credential or evidence hashes. Qualification and activation
remain separate commands, and unchanged apply never repeats a diagnostic.

Fresh shared connection has its own retained setup/reservation proof. It does not
pretend an old direct sender existed and cannot restore one through legacy rollback.
Lost activation/report replies resume their original transactions; a completed
retry inspects services without restarting them. Backup capture remains staged
until its own qualification and successful manual backup allow schedule activation.
Changed gateway/producer policy still needs its migration; it is not silently applied across
an existing ownership transition.

### Renew qualification after delivery activation

For a local shared gateway using Telegram, owned native Signal, or both, without
command history, run
`deploy notification-worker qualify manager` again after delivery admission.
This pauses provider delivery while preserving the queue, producer identity and
reporting ownership. The wizard saves new observations; it does not send a test
message. If interrupted, repeat `qualify` or use `resume` to finish the retained
pause. Delivery stays paused until a newly recorded report passes the checks and
you run `deploy notification-worker manager` again.

Keep YAML and helper artifacts unchanged during renewal. Apply any pending backup
setup first. A previous report cannot automatically reactivate this renewal, even
when the effective configuration is unchanged. Backup capture and its schedule
are separate from gateway delivery; this command does not stop backup capture.

Owned Signal renewal retains the linked account, service and inbox records.
An explicit `resume` can restart that same owned service; renewal does not relink
the account or replace its executable. Each pause collects fresh observations,
even when its configuration matches an earlier pause.

Renewal also works after production LED output toggles. It preserves bindings,
output permission and the backup configuration receipts. Renewing while LEDs are
disabled retains previously qualified output permission; it cannot invent that
permission on an installation originally admitted with output disabled.

For an unrecovered Telegram gateway with admitted backup commands, first set
`notification_worker.commands.enabled: false` in `server.yaml`, then run:

```sh
deploy notification-worker manager
deploy notification-worker qualify manager
deploy notification-worker manager
```

The first apply disables intake. Qualification pauses delivery and records fresh
observations; the final apply resumes delivery with commands still disabled.
Receiver ownership, pending commands, saved results and permission restrictions
remain retained. You can reduce permissions while disabled, or set
`commands.enabled: true` and apply to re-enable the retained command mode.
Both installed gateway executables must support command-preserving qualification
renewal. Missing history or a different helper requires explicit recovery or
upgrade; repeating qualification never restores removed rights.

Legacy Signal and recovered gateway renewals remain on the roadmap.

## Standalone backup installation

[backup-only.yaml](../../examples/workers/backup-only.yaml) installs without a notification worker or
provider credentials. Both the worker and sources need trusted SSH and their
existing rootless backup prerequisites. Source application declarations must opt
their databases into backups. The coordinator inspects every source, verifies
the selected per-architecture helper and reads existing verified host-key entries
before beginning installation. It never accepts a new host key implicitly.

The archive defaults to `~/.local/share/deploy/backup-worker/archive` on the target.
`storage.path` can customize it; an external required mount keeps its strict mount
guard. Local ext4 and capacity requirements still apply. New installations support
custom config/state/data/runtime/helper paths and target XDG roots. The user
manager must discover the selected unit directory; custom layouts require
`busctl` for that preflight. Existing installations are not relocated by changing
YAML. See [path defaults, overrides and recovery](paths.md).

Backup-only mode retains incidents and archive history without queuing provider
messages, installing notifier units, or claiming delivery health. Storage,
restricted-source, rootless-runtime and application-restore qualification still
apply. Changing reporting ownership later requires a migration; it cannot be
enabled or disabled by silently dropping pending deliveries.

Short backup apply also accepts the legacy direct Telegram profile with existing
target credential files. Local shared gateway staging supports Telegram and fresh
Signal as described above; direct backup-owned Signal setup remains a legacy
workflow. A successful first setup reports `qualification_required`; unchanged
retries preserve identities and do not re-enroll sources. Status checks retained
setup/runtime/helper/archive authority and lists applicable pending qualification.
An `active_recorded` result reports the activation record, not fresh service health.

`backup-worker qualify manager` uses the same evidence-file prompts as the
notification wizard. It binds the report to the installed image, helper, source
keys, desired policy and private monitor/provider inputs. Pending or failed checks
do not pass a qualification gate. Partial sessions and their observation times
survive cancellation; recording a report never starts a backup or enables a timer.

`backup-worker run manager` verifies the installed YAML revision, helper and loaded
service ownership, then requests a manual backup. It can run while qualification
is pending. It returns `start_requested` after submission or `running_or_busy`
when the service is already occupied; neither result proves a successful backup.
After an interrupted reply, inspect `status` or `logs` before requesting another
run. The command does not enable schedules. Then `backup-worker activate manager` verifies a
successful backup from the last hour, current evidence from the last seven days,
source restrictions, monitoring requirements and any shared reporting owner before
enabling schedule units. A retained intent reconciles lost activation replies.
Completed retries inspect enabled/active units without restarting them. Changed
credentials, incomplete evidence or a stopped previously activated unit require
review. Pending activation prevents setup updates, key rotation, removal or a new
qualification report from changing its pins.

To change monitoring destinations, save the new HTTPS URLs in private target files
and update `backup_worker.monitoring.backup_ping_url_file` and/or
`daily_delivery_ping_url_file`. Apply the short backup command. The two URLs must
refer to different checks. The update validates the files before retaining its
intent, preserves the archive and any shared gateway ownership, and returns the
installation to staged qualification. Applying never sends a heartbeat or records
an external observation. Keep the previous files available for rollback; qualify
the replacement checks and explicitly activate schedules afterward. Removing a
reference is also a policy change; remaining activation requirements still apply.

The local helper-only bundle does not contain backup image digests by default.
Supply a release catalog containing the matching immutable image, or set
`backup_worker.artifacts.image`. The existing runtime self-test verifies that the
image and helper come from the same build. No image is pulled during planning.

An installed shared backup can change only `backup_worker.artifacts.image` to
another pinned image compatible with the retained helper. Apply the backup worker;
its existing update/rollback transaction keeps source keys, archive identity and
the original reporting handoff, including after shared policy and source changes.
The gateway stays unchanged. Capture returns to staged qualification: check the
new image and activate the backup schedule explicitly. Keep source, policy and
relay-image edits separate.

For a shared backup retaining its original fresh reporting ownership, you can replace the backup
helper and its matching image in one edit. Set `backup_worker.artifacts.helper`,
`helper_sha256` and `image`, then run:

```sh
deploy backup-worker manager
```

Keep the source helpers, notification helper and other policy unchanged. The
update checks the old helper's ownership and the candidate's image self-test,
retains the old release for rollback, and preserves archive, source and reporting
identities. Capture returns to staged qualification. If interrupted, keep the
same YAML and artifact and run `deploy backup-worker resume manager`. A rollback
reports `helper_upgrade_pending` until that exact upgrade completes; changing back
to old YAML does not silently cancel it. Later notification edits use the upgraded
backup. A backup with a completed command receiver can use the same upgrade command
when the candidate supports its existing commands. Receiver version, permissions,
pending requests and saved results stay unchanged. Missing command records or
ledger data are refused, as are unsupported candidate capabilities. If the backup
is busy or has retained WAL data, let its current operation finish or recover it
before retrying. Enabling or disabling commands afterward uses the upgraded backup.
Completed command toggles retain their original YAML snapshot across later backup
upgrades. Recovered reporting ownership still requires its migration workflow.
This does not grant new command rights. Gateway/source helper upgrades and receiver
capability expansion still need separate migration workflows.

## Backup policy updates

For standalone workers, schedule, timezone, freshness, budgets, retention,
restore checks and reminder/daily policy changes use the existing journaled update
and rollback. Source additions have their own retained enrollment
transaction, described below. Retained-identity endpoint moves and explicit
source retirement are also supported below. Archive paths and reporting ownership
require separate migrations. Source-key fingerprints are
reserved before enrollment and checked on retries; missing or replaced keys need
recovery. Standalone disabling/re-enabling retains that layout. Shared removal
and relocation of an existing installation remain unfinished short-command
workflows.

Shared workers also accept backup-only schedule, timezone, budget, retention,
restore-check and reminder edits when the gateway policy stays unchanged. Apply
reuses the producer identity and retains the original delivery admission and
cutover evidence. Backup updates stage the timers again; complete the current
backup qualification and activation steps before expecting scheduled capture.
An unchanged retry inspects that ownership without restarting reporting.
Supported backup-only policy updates can also follow completed production
notification changes. For example, keep an explicit freshness deadline while
changing the backup schedule, or adjust a backup budget after updating gateway
limits. Completed update records preserve both histories and the original
reporting handoff. Apply backup-only edits separately from a new notification
change; the coordinator checks that the backup edits have actually completed.
An explicit producer deadline remains independent of later backup freshness edits.
If an update is interrupted before runtime publication, resume finishes the exact
backup transaction before asking the gateway to inspect the completed dependency.
The retained reservation is not new enrollment or delivery admission.

Freshness is part of the default gateway producer policy. For example, changing
an hourly schedule to daily changes its derived freshness from 2h to 26h, so that
combined edit needs two applies. First set an explicit
`freshness: 26h` and apply either worker. Then change the schedule to daily while
keeping that deadline and apply the backup worker. The deadline must remain valid
for the current schedule at each step. With active delivery, follow the freshness
qualification sequence above before applying the schedule change.
Route, provider, account and shared-worker artifact changes remain separate
migrations. Supported source changes are described below. Earlier bound/shared development installations with
whole-policy notification fingerprints require explicit conversion/import; do
not delete their journals to make them look like fresh installations.

## Change a standalone Telegram alert destination

For a standalone backup worker that uses Telegram without a fallback, write the
new numeric chat ID to a new private file on the target. Change only
`backup_worker.notifications.telegram.chat_id_file`, then run
`deploy backup-worker manager`. You can select a new token file in the same edit
if it belongs to the same bot. Keep the old files available through completion
and any rollback.

The update requires all retained outbox rows to have accepted delivery and no
reserved attempt to remain unresolved. Covered or ambiguous rows are not treated
as accepted. Resolve that delivery work before changing the destination; the
update does not send it to the new chat. The installer checks again after stopping
senders, retains archive/source identities, and stages timers for qualification
of the new configuration. No provider message is sent by apply.

## Helper upgrades

For an unbound gateway, review a helper change with `deploy notification-worker
plan manager`, then apply with its other notification settings unchanged. The
managed update keeps its identity, runtime policy, native account and recovery
helper; verification restarts once for a changed executable. Equivalent catalog
or helper references do not restart it. An older unbound whole-policy snapshot
can also convert to the current notification scope through apply without a new
identity or restart. Snapshot-less installations still need import/review.

[producer-policy.yaml](../../examples/workers/producer-policy.yaml) shows a custom local backup producer
quota, liveness and evidence bound. The same two short setup commands support this
declaration. Notification-first setup retains the desired policy while waiting for
the real backup reservation; it grants no placeholder producer or active generation.
The backup reporting route must occur in the producer's allowed routes. Changing
an enrolled producer's policy remains an explicit migration.

Standalone `backup-worker uninstall` uses the retained stop/revoke/remove
transaction. It verifies source administrator endpoints, revokes source access,
and removes only owned files. Repeating an interrupted uninstall reconciles the
same operation. Archives and lifecycle history remain; reinstallation requires
explicit reenrollment. Pending shared ownership still requires its domain removal
transition.

`notification-worker test` records one explicit Telegram diagnostic per installed revision. Repeating
it returns the retained result. If the process stops after a send might have
started, the result is `delivery_unknown`; retrying does not send again. Provider
acceptance is not qualification or production activation. A fresh diagnostic after
an ambiguous result needs the specialist review workflow; the short command does
not discard the unresolved attempt.

Short uninstall currently handles the unbound gateway installed by this interface.
It pins the current manifest, retains state and private inputs, and resumes a lost
removal response without repeating the transition. Removing gateways with enrolled
consumers, changing paths and reinstalling retained state still require the domain
lifecycle/adoption workflows.

Fresh Signal setup, native artifact inputs, private linking, diagnostic selection
and retained-state removal are described in [the Signal guide](signal.md).


## Notification routes and providers

Before any producer has been reserved or enrolled, notification route names and
primary/fallback ordering among the existing configured providers can be changed
in YAML and applied with `deploy notification-worker manager`. Apply route changes
separately from limits or other policy changes. The gateway identity, credentials
and completed LED output disposition are retained; interrupted changes use the same
short resume command. This requires bootstrap-policy capability 3. Routing changes
after enrollment are also supported for a local shared backup before delivery
activation without recovery. Existing LED bindings and output history remain. Keep providers, credentials,
producer subscriptions and command policy unchanged. Pending notification messages
must be settled first; the update never redirects queued work. Both workers retain
the new declaration and their original identities. Retry interrupted changes with
unchanged YAML, then qualify the revised configuration before activation.

For an already delivering, unrecovered gateway without command history, edit only
`notification_worker.routes`. Keep providers, producer permissions and the selected
backup reporting route unchanged. Once the queue has settled, use:

```sh
deploy notification-worker plan manager
deploy notification-worker manager
deploy notification-worker qualify manager
deploy notification-worker manager
```

The first apply pauses delivery and records the changed routes in both workers.
It retains original delivery ownership, active producer generations, terminal
message history, owned Signal and LED disposition. The final apply resumes
delivery after qualification. Use the same YAML with `resume` after interruption.
Adding/removing an unused route and changing primary/fallback order among existing
providers are supported. Changing a provider's account or destination, moving the
backup report to a different route, recovered gateways, command-bearing gateways
still need their respective workflows.

A backup can reserve its producer before the gateway enrolls it. During that
interval, `plan` and `apply` report `producer_migration_required` for gateway
limits, LED output, activity, provider/routing and helper/scope changes. Finish the
pending backup setup with its original YAML. An unbound gateway alone is not proof
that its declared policy has no consumers; later shared changes still require the
coordinated migration workflow.

An unbound direct Telegram gateway also supports changing credential references
and its destination, including when an unchanged owned native Signal service is
configured alongside Telegram. Create new private files on the target and edit the YAML:

```yaml
notification_worker:
  providers:
    telegram:
      token_file: ~/.config/deploy/secrets/telegram-token-v2
      chat_id_file: ~/.config/deploy/secrets/telegram-chat-v2
```

Keep the old files unchanged. Run `deploy notification-worker plan manager`, then
`deploy notification-worker manager`. Changing references is the explicit rotation;
overwriting an already pinned file is drift and is refused. The new references
must use target files, even if initial setup used operator-local `_input` files.
Apply this edit separately from routes, limits, artifacts and activity changes.

The coordinator checks both sets of credentials, retains the gateway identity and
LED output disposition, and records an immutable migration before replacing the
derived runtime. Same-bot token rotation preserves route audiences; changing the
bot or chat records a route-policy successor with an empty production-history
check. A lost start acknowledgement verifies the exact running revision before
recording completion. Resume with the same YAML; missing history or changed
credentials requires restoring the reviewed inputs.

Apply does not send a provider test or qualify the new destination. Use
`deploy notification-worker test manager` for an explicit test, then record actual
observations when qualification is required. Previous diagnostic outcomes remain
in history. This workflow requires the bootstrap-providers capability (version 2)
before producer reservation/enrollment. When Signal is also configured, its YAML
profile, native artifact, account, recipient, child receipts and service reference
must stay unchanged. The coordinator pins that retained authority on every retry;
the native service is neither relinked nor restarted by the Telegram update. An
explicit `resume` may separately repair an intact stopped native service after
checking the pending provider intent. Keep provider inputs as saved target-file
references for this update.

The same short command also changes Telegram credentials or its fixed alert
destination after local backup enrollment, before delivery activation. This
requires an unrecovered gateway and a settled queue. Configured commands can
retain the same bot account, name and permissions while selecting new private
files; command intake remains unadmitted. Its LED
endpoint, bindings and output setting remain unchanged. Both workers publish the revised declaration together; producer, archive
and source identities remain. Same-account token rotation and destination changes
retain the original bootstrap files plus every reviewed private-file set. Further
budget or producer-policy edits use the selected provider references. Qualification
is required for the revised runtime. Command bot replacement, Signal account changes and other provider additions still
need their separate admission workflows. Production direct-alert changes use the
flow below.

For an already delivering **gateway without command history**, you
can rotate private token/chat-file references for the **same bot and chat**, or
select a new chat/bot after pending messages have settled. Keep
other settings unchanged and retain the old files. This supports an unrecovered
local shared backup installation, including unchanged LED settings and an unchanged
owned native Signal service.

```sh
deploy notification-worker plan manager
deploy notification-worker manager
deploy notification-worker qualify manager
deploy notification-worker manager
```

The first apply pauses delivery and selects the new files. Backup identities,
terminal message history and the original reporting owner remain. Same-destination
rotations also retain queued work; a destination change never redirects queued
messages. The report service is
briefly stopped while its restrictions are expanded to exclude both old and new
credential files, then its original triggers resume. The apply succeeds and tells
you that qualification is needed. Perform any provider tests explicitly and record
actual observations before the final apply resumes delivery.

If interrupted, run `deploy notification-worker resume manager` with the same YAML.
A changed file or missing retained record is an error, not permission to use an
unreviewed credential. Further rotations and ordinary qualification renewal retain
all previous file exclusions. Both installed helpers must support this workflow;
a helper mismatch is refused before pausing. When Signal is also configured, its
account, recipient, native artifact, service and inbox stay unchanged. An explicit
resume can restart its intact stopped service; apply never relinks the account.
Changed native inputs or damaged rotation records block that restart. Command
intake, recovered installations and Signal account changes remain separate
workflows. An explicit `account` or `bot_name` remains a constraint on the selected
Telegram token; changing those declarations requires its own workflow.

An owned Signal gateway can add or remove its direct Telegram alert profile before
activation, both before and after the local backup is enrolled. Keep Signal's
account, recipient, artifact and service settings unchanged. This workflow does
not add command intake.

1. To add Telegram, create private token and chat-ID files and add the `telegram`
   provider block shown above. Keep routes unchanged and apply the notification
   worker. Then edit routes to use Telegram and apply again.
2. To remove Telegram, first change every route to use Signal and apply. Then
   remove the `telegram` provider block and apply again. Keep the old credential
   files: retained migration records still verify them.

Each apply is resumable with the same YAML. Shared changes require a settled queue
and preserve backup enrollment and LED state. A profile still selected by a route,
changed Signal settings, pending command authority or mixed policy edits are
refused. Qualification must match the final runtime; adding a profile does not
send a message or activate delivery.

For a delivering, unrecovered gateway without command history, the same
add/remove ordering works beside an unchanged owned Signal service. Use target-side
private files and keep other settings unchanged. Each effective edit follows
`plan` → apply → `qualify` → apply before making the next edit. Adding an unused
Telegram profile preserves Signal delivery ownership and the existing report
route. To remove Telegram, first complete a route change that stops selecting it,
then remove the provider block in a separate apply. Both changes require a settled
queue. Reporting, original delivery ownership, LED disposition and every retained
credential-file exclusion remain. Recovered gateways and explicit account/name
or command declarations still need their separate workflows.

## Standalone Telegram credential updates

For a standalone backup using direct Telegram, create new private target files
and change `backup_worker.notifications.telegram.token_file`, `chat_id_file`, or
both. This supports token rotation for the same bot and moving credential files
while keeping the same recipient. Keep every old file unchanged, and apply this
edit separately from other worker policy and artifacts. Run
`deploy backup-worker plan manager`, then `deploy backup-worker manager`.

The update pins all old and new token/chat files, preserves accepted and unknown
delivery records, and stages the replacement runtime with schedules disabled.
It does not send a test or resolve an earlier uncertain delivery. Use the same YAML
to resume interruption. Changed pinned files or missing rotation evidence require
restoring the reviewed inputs; rollback will not reactivate the old sender with
changed credentials. A failed candidate-only file does not prevent rollback when
both original files are intact; a file shared by the old and new configuration
must still match its original pin. After completion, record actual qualification and explicitly
activate schedules. For a new chat, follow [destination changes](#change-a-standalone-telegram-alert-destination); that path requires settled delivery history. Changing the bot, fallback or native account requires its separate migration workflow.

## Adding backup sources

To add a backup source, add another server alias to `backup_worker.sources`
and run `deploy backup-worker manager`. Keep existing aliases, host/user/port,
verified host keys, artifact selections and other worker policy unchanged for this
edit. The command reserves a key for each added source, enrolls restricted access,
qualifies the source restrictions and publishes the expanded installation while
retaining the existing worker and archive identities.

If interrupted, run `deploy backup-worker resume manager` with that same YAML and
source trust. A local rollback can restore the old runtime while the new remote
authorization already exists. Its key, enrollment receipt and exact pending
declaration remain reserved; status and plan report `source_enrollment_pending`.
Reverting the YAML does not cancel enrollment or revoke that access. Complete the
retained update before another change or uninstall. Missing reserved keys require
recovery rather than automatic replacement.

After a successful source addition, the schedule is staged with timers disabled.
Run a manual backup, complete the relevant qualification checks and use
`deploy backup-worker activate manager`. This covers standalone workers with
notifications disabled or unchanged direct Telegram settings, and shared backups
whose gateway policy and artifacts stay unchanged for this edit. Additions also
work after completed shared notification and backup-policy updates. Their retained
history includes the exact enrollment intent, so rollback and a lost remote reply
cannot replace the reserved source key. Later notification updates use the expanded
backup declaration. Shared additions retain the
same producer epoch/generation, routes, delivery admission and sender fence. The
backup update finishes before the gateway inspects the completed dependency; it
does not create another enrollment or rewrite the original cutover evidence.

## Moving a source endpoint

To move an existing source to another host name, SSH port or user, change that
server entry and apply the backup worker with other policy/artifacts unchanged.
The selected endpoint must already retain the same source identity, authorized
worker key and helper artifact. The source inspector only reads those records;
it cannot enroll an empty/different source, restore revoked access or replace a
helper. Missing or changed source authority refuses before local publication.
Existing sources with unchanged helper pins use the same inspection during
ordinary updates, avoiding unnecessary enrollment writes.

The worker keeps its archive, keys and source identities, qualifies the restricted
connection against the selected verified host-key entry, and publishes the new
endpoint through its existing update/rollback transaction. Restricted capability
checks consume the source's normal request-rate budget. A lost inspection reply
resumes with the same YAML; a failed local publication restores the prior runtime
and trust. Successful publication stages renewed qualification with timers disabled.
Shared reporting keeps its existing producer and gateway ownership when their
policy scope is unchanged. Endpoint moves also work after completed shared
notification and backup-policy changes: the move records its exact predecessor,
and later notification updates retain that history. Notification delivery stays
qualified and unchanged during the endpoint move. Keep source additions,
retirement and artifact updates in separate edits.

Run `plan` and apply before requesting another run,
recording qualification or activating schedules; those actions refuse an endpoint
mismatch. A moved SSH user must already contain the intact source installation.

## Retiring backup sources

To retire a source, remove it from `sources` and record its reason in
`retire_sources`, then run `deploy backup-worker manager`:

```yaml
backup_worker:
  sources: [vps-main]
  retire_sources:
    vps-old: service decommissioned
```

Retirement also works after completed shared notification, backup-policy and
source-addition changes. The history remains pending after local publication
until the old source acknowledges revocation. A lost acknowledgement resumes the
same disposition; completing retirement preserves the original reporting handoff
and allows later notification policy changes.

Keep the old `servers.vps-old` administrator endpoint until apply completes. The
worker first publishes the remaining source policy with schedules staged, records
retirement in the archive, and closes affected incidents as retired by the
operator. It then revokes the exact retained key at the old source. Archive data,
unrelated faults, accepted/uncertain delivery history and old key/trust files stay
retained. Retirement does not claim a successful backup recovery or delete data.

If the connection drops, `status` and `plan` report `source_retirement_pending`.
Restore access and run `deploy backup-worker resume manager` with the same YAML;
revocation retries verify the retained disabled authorization. Pending retirement
blocks activation and competing lifecycle edits. A failed publication restores the
old runtime before any retirement or revocation; the pending intent still requires
the same candidate YAML on retry. After completion, keep `retire_sources` in YAML
as the recorded disposition; the retired server inventory entry may be removed.
Qualify the remaining protection and explicitly activate its schedule.

Retirement supports standalone disabled/direct Telegram reporting and shared
backups with unchanged gateway policy and producer ownership, including backups
still awaiting shared qualification. At least one source must remain. Apply
helper/artifact updates separately before retirement; do not combine removal with
endpoint, route, freshness or other policy changes. Changing a source identity,
reusing a retired alias, or retiring the whole worker requires its separate
migration/lifecycle workflow. Shared worker uninstall remains unfinished.

## Replacing a source under a new alias

Use two applies when you want to qualify the replacement before retiring the
old protection. For example, with `vps-old` already installed:

1. Define `servers.vps-new` with its trusted SSH settings and application backup
   declarations. Keep the old server entry available.
2. Set `backup_worker.sources: [vps-old, vps-new]` and run
   `deploy backup-worker manager`. Keep other worker settings unchanged.
3. Check the replacement with a manual backup and the required qualification
   before retiring the old source. A successful enrollment alone does not prove
   that its application data can be restored.
4. Set `sources: [vps-new]`, add `retire_sources: {vps-old: replaced by vps-new}`,
   and apply again. The old administrator endpoint must remain reachable until
   revocation finishes. Use `resume manager` after an interrupted apply.
5. Keep that retirement reason in YAML, complete qualification for the remaining
   protection and explicitly activate its schedule. You can then remove the old
   server entry from the inventory.

Both stages retain the worker/archive identity and shared reporting ownership.
The old archive and retirement history remain available; this workflow does not
copy application data to the new server.

You can also declare both changes in one apply when the retirement decision is
already made:

```yaml
backup_worker:
  sources: [vps-new]
  retire_sources:
    vps-old: replaced by vps-new
```

Combined replacement also works after completed shared notification and backup
updates. The worker retains both source intents through local publication and
records completion only after remote revocation is acknowledged. A lost enrollment
or revocation reply resumes the same keys, sources and reporting owner.

Keep both server entries and their trusted SSH endpoints available. Run
`deploy backup-worker manager`. It enrolls and checks the new source, publishes
the replacement policy with schedules staged, records the old source's retirement
in the archive, and revokes the old key. It retains the original worker, archive,
producer and reporting owner. Other settings must stay unchanged.

If either acknowledgement is lost, `status` and `plan` report
`source_replacement_pending`. Run `deploy backup-worker resume manager` with the
same YAML. An installation rollback retains new keys and enrollment receipts;
it cannot cancel an authorization already accepted by a source. The old source
is not revoked before the replacement runtime is published. Activation stays
blocked until revocation is acknowledged; application backup/restore
qualification and schedule activation still follow completion.

This supports new aliases, including replacing every active source with new
ones. Reusing a retired alias or a retired source identity remains a separate
unfinished workflow.
