# Signal recovery verification endpoint

After `recovery-signal-handoff` or `recovery-signal-podman-handoff` completes,
`signal-verification-prepare` can enroll a separate verification endpoint for
that exact account recovery and inbox installation. `signal-verification-serve`
runs it in the foreground. Neither command starts Signal CLI, reads provider
credentials, changes a service unit, or releases the account/intake/command fence.

This is a local implementation step, not production activation. `adopt-signal`
now adopts the staged configuration into an existing managed gateway verification
service. Use that explicit transition instead of replacing configuration files
manually. `signal-service-setup` now installs a separate native user service for
the verifier. `supervise-signal` upgrades a previously adopted foreground endpoint
to that service without changing gateway configuration. Specifications without a
`runtime` field remain verification-only. Explicit native production enrollment
and subsequent gateway admission are described in [SIGNAL_PRODUCTION.md](SIGNAL_PRODUCTION.md).

For the legacy backup relay without an interactive inbox, first use
[legacy account adoption](LEGACY_SIGNAL_ADOPTION.md). Its completed handoff produces
the same installed inbox/account proofs used below.

## Preparation

Before publishing the quarantined gateway, import its stopped Signal inbox with
the exact operation from `recovery-prepare`. These commands do not contact Signal:

```sh
deploy notification-worker recovery-signal-history --local --config /absolute/private/runtime.json --state-dir /absolute/gateway --operation n-REPLACE --relay-state /absolute/inbox --receivers-stopped
```

For an installed recovered inbox, also pass `--installation-hash` with its exact
`installation_hash`. For a separately prepared inbox restore, use `--restored`
instead. Complete all available provider and specialist command reconciliation,
then run `recovery-requests-seal --reconciliation-complete` with the same gateway
configuration, state and operation. Finally run `recovery-signal-cursor` with the
same arguments as `recovery-signal-history`. This retains the source handoff and
installs the quarantined gateway cursor before review/checkpoint/publication.
Wrong operation pins are rejected before opening the relay. Dry runs validate
arguments and configuration without opening gateway, inbox or account state.

## Subsequent managed recovery

Later epochs retain the original account, service, inbox, gateway and profile
proofs. Use a new inbox destination, endpoint directory and service installation
for each epoch. Keep the previous directories and their removal records.

1. Disable commands and deactivate enrolled producers and gateway delivery.
   Stop the gateway and take a `managed-snapshot` with the current manifest pin.
   Run `recovery-prepare` with that snapshot's ID and database digest. Preparation
   checks the installed gateway/helper and Signal service capabilities before
   recording the next recovery intent.
2. Complete `signal-service-uninstall --local --root OLD_SERVICE_ROOT`. This
   drains the account/child leases and retains the service's removal evidence.
   A stopped but still installed service is insufficient for the next handoff.
3. Run `recovery-signal-history` against the previous installed inbox, including
   its `--installation-hash`. Complete producer, command and LED reconciliation,
   seal requests, and run `recovery-signal-cursor`. Review, checkpoint, verify and
   publish the gateway recovery using its new operation and resulting hashes.
4. Fill [signal-recovery-managed.json](signal-recovery-managed.json) with the
   previous endpoint/service reference, the unchanged canonical configuration
   bytes used for this recovery, the new gateway publication hash and a new empty
   `0700` inbox destination. Run:

   ```sh
   deploy notification-worker recovery-signal-managed-handoff --local --spec /absolute/private/next-signal.json
   ```

   The result contains the new account status and installed inbox proof. Exact
   retries verify the retained result. This command does not start a receiver.
5. Prepare a new verification endpoint using that result, its inbox destination
   and the same original gateway configuration. Install a new Signal service
   with `signal-service-setup`, retaining the qualified CLI/account inputs and
   using the new endpoint. This new endpoint has its own service binding; the
   account recovery chain retains the removed predecessor.
6. Run `adopt-recovery` on the gateway with the new publication hash, the new
   endpoint's staged gateway JSON, and `signal_verification` naming both the new
   endpoint and new service hashes. This operation adopts the endpoint and rebases
   previously enrolled outgoing Signal profiles together. Conversation enrollment
   is preserved; affected routes receive new revisions, so old queued work cannot
   silently acquire a new transport. Do not perform a separate `adopt-signal` for
   this transition.
7. Record fresh qualification for the new installation, activate delivery, then
   explicitly activate the intended producer generations and command mode.
   Recovery and endpoint installation alone grant none of those admissions.

Use `resume --local --root GATEWAY_ROOT` for an interrupted gateway adoption and
`signal-service-resume` for an interrupted service installation. Do not recreate
missing ancestor files from current configuration. Missing proofs, old endpoint
references and rolled-back account heads are refused. These procedures have
offline fixture coverage; live provider and host qualification remain separate.

## Endpoint enrollment

Stop the gateway and finish its published recovery and the account/inbox handoff
first. Retain the original source inbox, all account recovery records, the gateway
publication and the installed destination. The private gateway input must be the
same original JSON used for the account handoff, with its exact SHA-256. Rewriting
an equivalent JSON file changes that pin.

Create an empty, owned `0700` endpoint directory, separate from the account,
gateway, both inboxes and the old socket directory. Its `api.sock` path must fit
Linux's Unix socket path limit. Create a private `0600` specification based on
[signal-verification.json](signal-verification.json), replacing all placeholders:

- A fresh `n-…` operation ID identifies endpoint enrollment; it differs from the
  already completed account recovery operation.
- `recovery` is the exact complete output of `signal-recovery-status`. It must
  have `phase: "complete"` and `runtime_allowed: false`.
- `commit_hash` is the completed gateway publication commit. The recovery result
  hash must identify the inbox installation in `inbox_destination`.
- `gateway_config` and `gateway_sha256` refer to the original private gateway
  JSON, not to a resolved runtime or the newly staged JSON.

```sh
deploy notification-worker signal-verification-prepare --local --spec /absolute/private/spec.json
```

The command prints a manifest with `manifest_hash`. Keep that exact hash for
startup. It holds the gateway, account and both inbox leases while checking all
pins and publishing:

1. Immutable `intent.json` in the endpoint root.
2. An independent `signal-verification-endpoint.json` binding in the destination
   inbox. A different root or operation cannot enroll against the same installation.
3. Immutable `gateway-next.json`, changing only `interactive_signal.socket` and
   `interactive_signal.inbox_identity`. Other provider profiles and raw route
   labels remain unchanged. This is staged verification configuration, not a
   production delivery migration.
4. Independent `complete.json`, after auditing the retained records and databases.

Repeat the same preparation request after a crash. A completed operation never
recreates missing intent, staged configuration, lease or destination receipt.
Do not delete those files or recreate locks to make a retry pass. Losing durable
proof requires investigation; a new endpoint operation is not a repair mechanism.

## Foreground verification

```sh
deploy notification-worker signal-verification-serve --local \
  --root /absolute/private/endpoint \
  --manifest-hash EXACT_MANIFEST_HASH
```

The process holds the existing account operation/account leases, both inbox
leases and the endpoint operation lease until it exits. Maintenance, ordinary
relay startup, duplicate verifiers and offline inbox installation cannot enter.
The gateway lease is released after preparation, so the managed gateway
verifier can run concurrently.

The only permitted request is bodyless `GET /health` on the private `0600`
`api.sock`, from the same Unix UID. Health returns `verification_only: true`,
`intake_enabled: false`, the manifest/installation/commit/recovery pins and the
new Signal account/socket/inbox identity. It grants no activation authority.
Send, receive, list, ACK and all other requests are rejected.

Every health response rechecks the pinned records and source/destination
baseline. Periodic checks also terminate the service on missing or changed
proofs, replaced directories/database/lease inodes or a replaced socket. SQLite
verification uses offline read access; uncheckpointed sidecars are rejected,
not repaired. No gateway or inbox database is modified. Shutdown removes only
the server's own socket inode; a stale socket after a killed process can be
replaced on an exact restart, while foreign/live sockets are refused.

The CLI's global dry-run mode reads the preparation specification and pinned
configuration only. It does not open recovery databases, create coordination
files, bind sockets or inspect/change services. Serve dry-run validates its
arguments only.

## Managed gateway adoption

The gateway must already have completed `adopt-recovery` for the same publication,
using the exact original gateway configuration from the account handoff. Start
the separate Signal verifier, either in the foreground as above or through the
managed service below. Keep it running. The adoption
command does not start, stop or modify that endpoint or the original relay unit.

Create a private specification based on
[managed-signal-adoption.json](managed-signal-adoption.json): keep the existing
installation root, state, unit directory and exclusions; point `runtime_file` at
the endpoint's immutable `gateway-next.json`; pin the new native executable and
set `signal_verification.root` and `manifest_hash` from endpoint preparation.
Do not set `recovery_commit_hash` again; this operation retains the adopted commit.

```sh
deploy notification-worker adopt-signal --local --spec /absolute/private/adoption.json
```

The installer checks the exact old/new configuration, publication and endpoint
before stopping the gateway. Both the replacement executable and its retained
recovery helper must advertise Signal verification support. An independent copy
of the endpoint manifest and the immutable adoption intent preserve the binding.
The normal lifecycle then stops/drains the gateway, installs its new immutable
release and unit, publishes the manifest, reloads, restarts and verifies readiness.
The worker configuration, canonical database identity, provider profiles and
admission fences are unchanged. Account files/inboxes remain with the separate
verifier; no account mounts or write permissions are added to the gateway unit.

After a durable adoption intent exists, recover interruptions with:

```sh
deploy notification-worker resume --local --root /absolute/installation
```

A failure before that intent is published leaves the previous operation current;
retry `adopt-signal` with the original specification. Completed resume checks
health without repeating stop/start. Missing retained endpoint evidence fails
rather than being reconstructed. The original gateway input and relay service
configuration remain available for the original account-handoff audit/retry.

The managed gateway checks the endpoint before exposing its API and periodically
while running. Its status includes `signal_verification_hash` and a
`signal-verification` runtime-loop observation. The installer independently checks
the endpoint's complete health pins. An unavailable or mismatched endpoint ends
the gateway verification process; it cannot leave stale readiness serving.
Only private, same-UID Unix sockets are used. Redirects, oversized/malformed
responses, runtime/intake authority and different identities are rejected.

Updates and compatible binary rollback retain this binding and require capable
executables. Rollback cannot cross the Signal configuration transition. For later
`update` specifications keep the exact staged configuration bytes and omit the
adoption-only `signal_verification` field; the installer inherits its retained
binding. Uninstall of a completed installation preserves all endpoint/account evidence and does not require
endpoint liveness, but still verifies retained publication records. It does not
uninstall the separately running verifier. An unfinished adoption must be resumed
before uninstall.

Existing Telegram renewal evidence can cross only this proven Signal-only
configuration change. Adoption does not poll Telegram or re-date its observations;
a later explicit `renew-telegram` retains the Signal binding. Dry-run validates
local inputs/publication only; it does not inspect a manager, connect to the
endpoint, probe executables, change files or open recovery databases.

## Managed endpoint service

Complete endpoint preparation first. Stop any foreground endpoint process before
setup; it owns leases the installer must acquire. An already adopted gateway will
lose readiness until the supervisor transition finishes. The original recovered
relay service stays fenced and is never rewritten or re-enabled by this flow.

Create an empty owned `0700` service installation directory, separate from the
endpoint, account, inboxes and gateway installation/state. The current user's
`~/.config/systemd/user` directory must already exist and must not be writable by
another user. Use a private `0600` copy of
[signal-verification-service.json](signal-verification-service.json). Supply a
fresh `n-…` operation ID, the endpoint root/hash and the exact native binary hash.
Paths must be absolute, without symlink components; the input executable must
live outside the installation and endpoint roots.

```sh
deploy notification-worker signal-service-setup --local --spec /absolute/private/service.json
deploy notification-worker signal-service-inspect --local --root /absolute/private/service-installation
```

Setup installs `deploy-notification-signal-verification.service`, with a frozen
native executable and independent copy of the endpoint manifest. It validates
binary capability before binding the endpoint or publishing a unit. The private
journal resumes binding, unit/publication writes, daemon reload, enable/start and
health verification. The endpoint permanently retains the service root/hash;
ordinary foreground startup then refuses to bypass that binding.

The service retains the endpoint/account/inbox leases. Its only writable service
path is the endpoint root, and its only address family is Unix. Account and inbox
records remain read-only. Startup and periodic audits check installation paths,
lease identities, binary/publication/unit hashes and the removal fence. Manager
verification checks the fixed unit name, absence of aliases/drop-ins, typed
execution arguments, actual process executable/UID/arguments and the health
socket's peer PID. Health also reports the service manifest hash. No provider
credentials, network transport or intake API are introduced.

After an interrupted setup, use the same spec or resume the pinned root:

```sh
deploy notification-worker signal-service-resume --local --root /absolute/private/service-installation
```

Resume uses the staged executable after staging has completed. A successful
active retry verifies health without repeating reload/start; an inactive exact
installation can be restarted. If systemd also stopped the dependent gateway,
start `deploy-notification-worker.service` again after the verifier is healthy,
then use the gateway `resume` command to verify its completed installation.
Missing durable evidence is an error, not a reason to recreate enrollment. A failed capability check before binding can be retried
only with the same pinned inputs; a different binary needs a new enrollment root
before any endpoint binding exists.

For a gateway that has not yet adopted the endpoint, use `adopt-signal` with
[managed-signal-supervision.json](managed-signal-supervision.json). In addition to
the endpoint pins, set `service_root` and `service_hash` to the setup output.
For a gateway that already used `adopt-signal` with a foreground endpoint, use:

```sh
deploy notification-worker supervise-signal --local --spec /absolute/private/gateway-supervision.json
```

This transition preserves the adopted endpoint and exact runtime bytes, upgrades
the helper and records a new immutable operation referencing the original
adoption. It adds `Requires=` and `After=` for the separate service to the gateway
unit. Both candidate and helper must advertise supervised Signal support. Existing
Telegram renewal evidence remains unchanged; no provider check is repeated.
An interrupted gateway transition uses the normal gateway `resume` command.
Later updates omit `signal_verification` and inherit all retained pins. Rollback
cannot undo supervision by crossing to a revision without the same binding.

Uninstall the gateway first, then explicitly uninstall the verifier:

```sh
deploy notification-worker uninstall --local --root /absolute/gateway-installation
deploy notification-worker signal-service-uninstall --local --root /absolute/private/service-installation
```

Verifier uninstall writes a durable removal fence, stops and drains the owned
process, disables and removes only its exact unit, then reloads and verifies
absence. `signal-service-resume` finishes an interrupted removal. The executable,
manifest history, endpoint binding and recovery records remain; removal grants
neither foreground fallback nor account/intake permission. For permanent removal, uninstall the gateway first. For executable replacement,
stop the gateway and follow the explicit replacement procedure below; ordinary
update/resume cannot adopt a changed verifier binding. After both are uninstalled, the retired
gateway can still validate retained removal evidence and repeat its completed
uninstall; that evidence never authorizes a running gateway.

Each service installation is immutable. Binary replacement and re-enrollment
use a fresh root through the procedure below; later recovery-epoch replacement
remains unsupported. The service supervises a native verifier even if the original
account relay used Podman.
It does not create a new receiving container. `signal-service-status` remains the
older account-relay status command; use `signal-service-inspect` for this separate
verification installation. Setup dry-run validates inputs only and creates no
files, leases, sockets or manager activity.

## Verifier executable replacement and re-enrollment

Stop the gateway first. Remove the current verifier using
`signal-service-uninstall`, and finish any interrupted removal with
`signal-service-resume`. Keep the old installation, account/inboxes and endpoint
publication. Create a fresh private service root and stage the desired executable
outside both old and new roots. Use a new operation ID and the same endpoint and
unit directory in the service spec. Add `previous` with these exact fields:

```json
{
  "root": "/absolute/verification-endpoint",
  "manifest_hash": "ENDPOINT_MANIFEST_SHA256",
  "service_root": "/absolute/previous-service-installation",
  "service_hash": "PREVIOUS_SERVICE_MANIFEST_SHA256"
}
```

The `previous` object is a field in the normal service spec, alongside `endpoint`,
`binary_file`, `binary_sha256`, the fresh `root` and operation. Then run:

```sh
deploy --dry-run notification-worker signal-service-replace --local --spec /absolute/private/replacement.json
deploy notification-worker signal-service-replace --local --spec /absolute/private/replacement.json
```

Replacement requires completed predecessor removal, an absent owned unit, capable
candidate executable and exclusive predecessor/endpoint/account/inbox leases.
Independent immutable intents precede the endpoint binding change. Completion
records stay in the old installation, endpoint and new installation. Interrupted
replacement resumes from the **new** root with `signal-service-resume`; it does
not need the original input executable after staging. Missing committed evidence,
a second replacement branching from the same predecessor, or a restored older
endpoint binding blocks admission. Replacement never enables provider intake.

For an installed gateway, copy the replacement's `service_root` and
`service_hash` into `signal_verification` in its supervision spec; retain the
exact canonical runtime bytes and endpoint pins. Use a replacement-capable gateway
binary and run `supervise-signal --local --spec /absolute/private/supervision.json`.
This is an explicit journaled rebind and is resumable. Ordinary update/resume
continues to refuse the old service binding. The transition preserves migrated
outgoing profiles, worker state and Telegram renewal evidence. Qualification must
be collected for the resulting installation; replacement is not qualification.

A compatible binary rollback uses the same replacement procedure with that
binary in another fresh root, followed by gateway supervision. It does not revive
an older removed service root. Executables lacking the replacement capability
are refused. All predecessor evidence stays required, with at most 32 service
replacement predecessors; archival/history compaction remains future work.

## Outgoing profile migration

After supervised adoption, the canonical gateway configuration still retains the
original outgoing Signal socket/inbox pins. Migrate already enrolled conversation
profiles explicitly:

```sh
deploy --dry-run notification-worker migrate-signal-profiles --local \
  --spec /home/notify/managed-signal-profiles.json
deploy notification-worker migrate-signal-profiles --local \
  --spec /home/notify/managed-signal-profiles.json
```

Adapt [managed-signal-profiles.json](managed-signal-profiles.json) to the exact
installation, state, unit directory and exclusions. `runtime_file` must contain
the unchanged canonical bytes used by the current gateway, for example the
endpoint's retained `gateway-next.json`. Keep that endpoint publication intact.
Supply a native executable with the Signal profile migration capability; both
the new runtime and its replacement recovery helper are checked before stopping
the gateway. The existing service/account/inbox recovery records remain required.

Each outgoing Signal profile must already have an enrolled `conversation` and
match the original account/socket/inbox retained by endpoint preparation. The
installer derives the new transport from the adopted supervised endpoint. It
refuses unrelated pins, a missing Signal profile, an unsupervised endpoint and
legacy fixed-recipient profiles. Legacy `/send` recipient configuration needs a
separate explicit conversation enrollment; this command does not infer one.

The original canonical runtime and worker recovery configuration hashes remain
unchanged. The migration release retains `signal-profiles.json` and a separate
effective `signal-runtime.json`, independently bound to the immutable operation,
installation, worker identity and endpoint. Only outgoing Signal transport pins
and the affected derived route revisions change in the effective configuration.
Even a surviving inbox with the same identity receives a new route revision when
its socket moves. Telegram-only routes and every conversation/destination remain
unchanged.

Startup verifies the original recovery proof first, then applies the proven route
revisions to the verification process. It does not rewrite queued route snapshots,
receipts, provider cursors or recovery anchors. Old queued work keeps its original
revision and cannot silently acquire the new transport. Its eventual handling
belongs to explicit production admission/administration. The separate endpoint
still exposes verification only; this migration enables no delivery, intake,
replies or command execution.

The ordinary lifecycle journal covers stop/drain, publication, reload, restart
and health. Use `notification-worker resume --local --root …` after interruption;
retained artifacts suffice after the original external binary/spec-runtime inputs
are removed. Retained endpoint publication and account/inbox evidence must remain.
An exact completed resume checks health without restarting again. Updates and
compatible binary rollback retain the profile proof; rollback across its
introduction is refused. Uninstall retains it and removes the gateway before the
separate verifier as usual. No automatic history cleanup removes these artifacts.

Health reports `signal_profiles_hash`. Readiness requires that exact hash, and the
periodic Signal audit stops the gateway if the effective bytes or retained proof
are lost or changed. `activation-review` reports `effective_config_hash` and a
`signal/profiles` check; matching profile pins satisfy only their binding check.
Collect new qualification evidence for the new revision. Original Telegram
renewals are neither polled nor re-dated during migration, and explicit subsequent
Telegram renewal retains the effective Signal binding.

## Validation and limits

Tests cover local Unix HTTP, same-user peer checks, denied operations, retained
leases, changed/missing evidence, no-write filesystem observations, configuration
revision preservation, four publication SIGKILL boundaries and a built executable
killed and restarted with a stale socket. These fixtures use synthetic account
state; they do not contact Signal/Telegram or qualify ARM64/physical LEDs.

Managed adoption tests additionally cover lifecycle interruption/SIGKILL,
configuration/endpoint drift, executable capability rejection, compatible rollback,
retained Telegram renewal and two separate native processes. Tests use fake
service managers. Separate service tests include setup/removal interruption and
SIGKILL, lost evidence, foreign units, capability rejection, Telegram preservation,
real process restart, Unix peer PID and evidence-loss shutdown. Generated units
are syntax-checked, but actual user-systemd supervision still needs disposable-host
qualification. Production activation must independently
authorize provider intake, delivery and command admission. Neither adoption nor
a healthy verifier satisfies those gates.
