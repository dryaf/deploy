# Native verification installation

This lifecycle installs a local user service in **verification mode**. It accepts
durable submissions/status and exposes diagnostics. It does not dispatch messages,
poll Telegram/Signal, grant commands or publish healthy LED leases. Existing fault
states remain retained. Production activation, remote installation and Signal
account/relay installation are still unfinished.

Prepare an owned, private runtime JSON and edit `installation.json` with real
absolute paths. `unit_directory` must equal `$XDG_CONFIG_HOME/systemd/user`, or
`$HOME/.config/systemd/user` when XDG_CONFIG_HOME is unset. The unit directory and
installation root's parent must already exist, belong to the current user and
have no group/other write permissions. The installation root must not exist.
Keep installation, state and unit directories separate. Sources must be outside
the installation root. Runtime JSON must describe exactly the enrolled producers.

Build a native Linux amd64/arm64 binary and record its SHA256 in the spec. Binary
and configuration inputs must be owned regular files, without symlinks, hardlinks
or group/other writes. Use `chmod 700` for the binary and `chmod 600` for JSON.
The installer copies both into an immutable release and checks the selected
binary's schema/lifecycle capabilities before using it. The user-supplied digest
pins a trusted executable; this is not a signature or provenance verification.

Initialize new state explicitly, once:

```sh
umask 077
deploy notification-worker init --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification

deploy --dry-run notification-worker setup --local \
  --spec /home/notify/.config/deploy-notification/installation.json
deploy notification-worker setup --local \
  --spec /home/notify/.config/deploy-notification/installation.json
```

Setup adopts existing initialized, inactive state. Missing state, an older schema,
active/draining producer generations, foreign units/drop-ins/aliases and changed
identity all stop the operation. It never initializes an empty replacement DB.
Rootless user-systemd namespace restrictions must work on the host; an actual
manager/sandbox qualification is still needed. Offline unit validation and the
temporary native executable integration test do not prove those restrictions.

Use `update --local --spec …` with a new digest/binary to install another immutable
revision. It currently requires the same resolved runtime configuration, exclusions
and schema. Changes to destinations, enrollment, sandbox exclusions, LED bindings
need a future explicit managed configuration migration. Schema 9 → 10 uses the
explicit operation below. The initial `recovery-helper`
binary remains pinned independently of later releases unless explicitly replaced
by `upgrade-helper` or a supported migration. Update checks that both
binaries can read this journal/schema. Update of an uninstalled installation
reinstalls verification service without replacing its state identity.

Inspect, resume an interrupted operation, or uninstall:

```sh
deploy notification-worker installation-status --local \
  --root /home/notify/.local/share/deploy/notification-installation
deploy notification-worker resume --local \
  --root /home/notify/.local/share/deploy/notification-installation
deploy notification-worker uninstall --local \
  --root /home/notify/.local/share/deploy/notification-installation
```

If the invoking executable is unavailable, run the retained
`<root>/recovery-helper notification-worker resume --local --root <root>`.
Resume uses frozen inputs, not the original source files. It checks the exact
journal, manifests, binaries, runtime and unit; changed authority requires repair
and is never overwritten automatically. A lost start acknowledgement remains an
unfinished operation until resume verifies the running revision. Runtime holds a
shared installation lease and the worker's exclusive state lease until shutdown.

Uninstall also cancels an interrupted setup/update without first requiring a
healthy start. The cancellation records the superseded immutable intent; if
interrupted itself, resume finishes removal and never restarts the cancelled
installation. Changed artifacts or identity still block the operation.

Uninstall stops/disables and removes only the pinned native unit. It retains state,
pending obligations, credentials, binaries, the helper and all fault/cleanup
history. It makes no LED clear calls. Outstanding LED cleanup is not claimed
complete. It refuses active/draining producers. Repeating uninstall is safe.
There is no automatic rollback across schemas or destructive purge command.

To return to a retained executable, select its installation revision explicitly.
Revision IDs appear in setup/update output and the `<root>/releases` directory;
rollback requires the matching immutable operation intent as well as the files:

```sh
deploy --dry-run notification-worker rollback --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --from-revision n-REPLACE_WITH_RETAINED_REVISION
deploy notification-worker rollback --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --from-revision n-REPLACE_WITH_RETAINED_REVISION
```

The dry-run checks retained manifests and file hashes without probing executables
or contacting the service manager. Apply creates a new immutable revision from
the selected binary, retaining the current runtime JSON, state, identity, schema,
producer ownership and independent recovery helper. It requires matching resolved
configuration and sandbox exclusions. Changed artifacts, unsupported binaries or
a concurrently changed installation block rollback. Selecting an executable
already installed in verification mode returns the current revision without
another restart.

Rollback uses the ordinary update journal, including its executable probes,
stop/file-publication/start/health checks. `resume` and the retained recovery helper
can finish it without the original source files. No database snapshot is restored.
It grants only verification mode. If an earlier update is unfinished, either
resume it or use `uninstall` to cancel it first; rollback can then reinstall the
retained executable from the completed uninstalled state. This path does not
require the failed candidate to become healthy first.

A crash during input preparation may leave a private `.notification-rollback-n-…`
sibling of the installation root. It has no service authority; retain unexpected
contents for inspection. Completed preparation is copied into the normal immutable
release before publication, so later resume does not depend on that sibling.

History is bounded at 32 operations/releases, including abandoned release staging.
At that bound, offline archival is required; no history is silently deleted. A crash
before setup publishes its root can leave a private `.notification-n-…` sibling
under the root's parent. It has no installed service authority; retain it for
inspection. Preparation errors never publish a partially initialized installation.

Use the [qualification evidence workflow](QUALIFICATION.md) to generate and
record dated observations against this exact installation. It accepts incomplete
or failed checks without enabling production. `installation-status` continues to
report `qualified: false`; evidence completeness is reported separately by
`qualification-status`.

The actual user-manager sandbox has an explicit opt-in probe:

```sh
DEPLOY_QUALIFY_USER_SYSTEMD=1 go test -tags=notificationintegration \
  ./internal/notification/install -run '^TestNotificationRealUserSandbox$' -v
```

It starts a uniquely named, automatically collected transient service with
disposable files. It tests state writes, readonly paths, exclusions, Unix sockets
and denied IP socket creation, without opening network connections or accessing
hardware. It does not install the notification service. On the development host,
the 2026-11-01 probe failed before execution with `218/CAPABILITIES` and
`Failed to drop capabilities: Operation not permitted`. That host is not qualified;
no host security policy was changed. The generated unit retains its restrictions,
including an explicit private user namespace.

Queue and receipt inspection is also available against the running API:

```sh
deploy notification-worker queue-inspect --local \
  --state-dir /home/notify/.local/state/deploy-notification \
  --socket /home/notify/.local/state/deploy-notification/api.sock \
  --producer backups --producer-epoch REPLACE_WITH_ENROLLED_EPOCH \
  --after 0 --limit 64
```

Use `receipts-inspect` with the same options for retained receipts. Follow `next`
while `more` is true. These positions are **inspection positions**, not consumer
acknowledgement cursors. Inspection does not change listed/acknowledged floors,
re-evaluate message eligibility, grant attempts or expose message text. Pages are
live observations rather than a frozen export; missing history is not proof that
an event was never delivered.

## Explicit schema migration

An installed **verification-mode** worker using schema 9 can now migrate to
schema 10. This transition preserves worker identity, producer generations,
configuration, credentials, queue/receipt state, command history and LED faults.
It does not enable delivery or interactive commands. Other source versions,
active/draining generations and pending recovery/LED migration are rejected.

Prepare the usual installation spec with the new native executable and digest.
Keep the existing state/root/unit paths, resolved runtime and exclusions:

```sh
deploy --dry-run notification-worker migrate-schema --local \
  --spec /home/notify/.config/deploy-notification/installation.json
deploy notification-worker migrate-schema --local \
  --spec /home/notify/.config/deploy-notification/installation.json
```

Dry-run validates local input files only. Apply requires a completed prior lifecycle
operation and probes both new executables for schema **and migration** compatibility
before stopping the old service. The lifecycle intent blocks automatic startup
of the previous managed revision while migration is unfinished. Under the
installation and worker leases, the migration checks inactive producer ownership,
validates and checkpoints SQLite, and retains the exact original database at
`<state>/schema-migrations/<operation>/before.db` with immutable intent/completion
records. Upgrade and SQL acknowledgement commit in one transaction. A separate
state gate blocks ordinary opens until completion. Missing/corrupt backup or
contradictory checkpoint evidence stops resume; no empty database is created.

The original `<root>/recovery-helper` remains intact. The migration stages its new
compatible helper at `<root>/releases/<new-revision>/recovery-helper`, pinned by
`helper_revision` and `helper_hash` in the target manifest. During interruption,
the target revision is recorded in `<root>/lifecycle.json` under
`Intent.After.helper_revision`. Resume with the new helper, not the older one:

```sh
<root>/releases/<new-revision>/recovery-helper notification-worker resume \
  --local --root <root>
```

Exact resume reuses frozen inputs and the original backup. It can finish after
process death on either side of the database commit without reapplying the upgrade.
An unfinished schema migration must be resumed before uninstall. Once complete,
ordinary updates/uninstall work normally and retain the compatible helper.
Executable rollback cannot cross the schema/helper transition. The pre-upgrade
copy is retained for inspection and deliberate recovery; this operation provides
no automatic database downgrade. Live user-systemd and native ARM64 qualification
remain separate from the isolated process/socket tests.

## Managed LED binding migration

`migrate-led` changes `worker.led_bindings` on an installed **verification-mode**
worker using the current schema. Keep its identity, socket, UID, enrollment,
providers, credentials, limits, paths and exclusions unchanged. Ordinary `update`
rejects a changed mapping. Other configuration changes still need their own
migration protocol; this command does not activate delivery or interactive commands.

Prepare the installation spec with a migration-capable native executable and its
digest, and a runtime file containing only the intended LED binding changes:

```sh
deploy --dry-run notification-worker migrate-led --local \
  --spec /home/notify/.config/deploy-notification/installation.json
deploy notification-worker migrate-led --local \
  --spec /home/notify/.config/deploy-notification/installation.json
```

Dry-run validates local inputs only. Apply rejects changes that merely reorder
bindings/selectors or spell out existing defaults. It checks the old/new runtimes,
reads local state prerequisites, and probes the new executable and helper before
publishing lifecycle intent or stopping the service. The local check also works
while the verification runtime owns its lease; it performs no repairs or daemon I/O.
After stop, apply acquires the installation and worker leases and repeats these
checks. Every changed binding must have no remaining fault or cleanup rows;
resolve those obligations through the normal domain workflow first. Migration does
not waive them or infer resolution from a configuration edit. Unchanged binding
faults remain intact. The daemon must be reachable at the existing socket and its
capabilities/registry must satisfy the pinned mapping preflight.

The installation operation ID also identifies the durable LED mapping intent.
Mapping steps can revoke old healthy leases and set unknown baselines; they never
clear fault IDs or set healthy output. Interrupted steps retain their intent and
are reconciled under the existing mapping protocol before configuration and unit
publication. Missing, mismatched or foreign evidence stops the operation.

The new helper is retained at `<root>/releases/<new-revision>/recovery-helper`.
Find its revision in `<root>/lifecycle.json` under `Intent.After.helper_revision`
and resume with that exact helper:

```sh
<root>/releases/<new-revision>/recovery-helper notification-worker resume \
  --local --root <root>
```

Resume uses the frozen inputs and original operation; it cannot adopt a different
mapping completion. Completed resume and managed startup verify the durable mapping
without daemon writes or repairs. An unfinished migration must be resumed before
uninstall. Later ordinary updates retain the new configuration/helper; rollback
cannot cross this configuration/helper transition. Current qualification evidence
must be collected again for the new revision. Native process/socket tests use the
pinned daemon's mock driver; physical LED, native ARM64 and actual user-systemd
qualification remain separate.

## Explicit helper upgrade

`upgrade-helper --local --spec …` installs the supplied binary as both the new
verification runtime and a new retained helper. It requires an installed service
at the current schema and unchanged resolved runtime, exclusions, state paths and
worker identity. Recovery commit and Telegram renewal pins are retained. Prepare
the usual installation spec with the new binary and SHA256; do not include
`recovery_commit_hash` (that input is reserved for `adopt-recovery`).

```sh
deploy --dry-run notification-worker upgrade-helper --local \
  --spec /home/notify/.config/deploy-notification/installation.json
deploy notification-worker upgrade-helper --local \
  --spec /home/notify/.config/deploy-notification/installation.json
```

The staged binary/helper must understand the upgrade journal and any retained
recovery/Telegram evidence before service stop. Telegram configurations also
require the new renewal capability, even before their first renewal. This allows
an older adopted-recovery installation to gain support without changing its data
schema or identity. Ordinary `update` intentionally retains the existing helper.

The old helper remains intact. If interrupted, use the current external binary
or the new `<root>/releases/<new-revision>/recovery-helper` with `resume --local
--root <root>`. Its revision is recorded at `Intent.After.helper_revision` in
`lifecycle.json`. The previous helper cannot read this new operation type.
Finish the operation before uninstall. Binary rollback cannot cross this helper
boundary. The upgrade starts verification only and grants no provider authority.

## Telegram renewal after recovery

An interactive Telegram gateway installed through `adopt-recovery` can create a
new drain observation with `renew-telegram --local --root <root>`. Both its runtime
and helper must support renewal; use the explicit helper upgrade above when
necessary. Stop all other consumers of that bot account first. The coordinator
stops and checks only its exact local managed service; it cannot prove that an
external receiver is stopped. The caller needs the configured private bot token.

The coordinator owns the operation/installation/worker leases, freezes the exact
predecessor in `operations/ID/telegram-request.json`, and validates unit, service,
configuration and lease identity around provider work. Sanitized pages and the
worker checkpoint become durable before positive provider offsets. A separate
`operations/ID/telegram-receipt.json` binds the completion to this installation.
Successful JSON contains the manifest and receipt. Retain both managed records
with the worker's renewal archives; missing or inconsistent authority stops resume.

After interruption use `resume`, not a new renewal request. Once the worker has
completed its drain, resume needs no token or provider call, even if the managed
receipt had not yet been written. A missing request after worker progress or a
missing receipt after publication cannot be reconstructed. A fresh invocation
following completion creates a new generation; completed resume preserves dates.
An unfinished renewal must be resumed before uninstall.

Only verification mode restarts. The process audits the latest completed renewal
under its lifetime worker lease; health binds `telegram_renewal_id`. Updates,
compatible binary rollback and uninstall retain that pin. An expired observation
can still be audited for verification startup; production admission must perform
its own fresh clock/cutoff check. The standalone `recovery-telegram-check` requires
a stopped verification service. Production cutoff installation, Signal service
handoff and provider activation remain unfinished.
