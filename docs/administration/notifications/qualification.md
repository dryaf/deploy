# Installation qualification evidence

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

The local CLI can now record dated operator observations for a completed managed
verification installation. It does not run the checks, contact providers or the
LED daemon, install services, or enable production delivery or chat commands.
`qualified` and `activation_allowed` remain false even when all evidence is present.
The separate [managed delivery transition](delivery-activation.md) can consume
this evidence for supported initialized/recovered Telegram, native Signal and LED
output. Producer cutover and command admission remain separate explicit operations;
see the linked delivery, command and Signal production runbooks. Existing fixture
tests are not live qualification evidence.

For a declarative installation, `deploy notification-worker qualify manager`
collects the same evidence interactively, including the LED daemon version and
configuration-file digest when LEDs are configured. Each check still requires
an actual observation; the wizard neither tests hardware nor sends messages.
The specialist report below remains available for scripted administration.

Generate a private report before testing the installation:

```sh
umask 077
deploy notification-worker qualification-template --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  > /home/notify/notification-qualification.json
```

The command validates the retained installation artifacts, current unit and worker
identity. It records the exact manifest, including revision, worker/recovery
identity, resolved configuration and route fingerprints, executable/helper/unit
hashes, architecture and runtime UID. Credential and recipient files must exist;
their contents are hashed into `input_hashes`, never copied into the report. Keep
the report private. Missing, unsafe or unreadable inputs prevent collection.
No state database is opened and no provider or service-manager probe is performed.

Fill in `operator` with a stable token identifying the reviewer. For each check,
retain its generated `id` and `scope` and use:

| Field | Meaning |
| --- | --- |
| `result: pending` | Test not completed. Leave `observed_at` zero/absent and `evidence_sha256` absent. |
| `result: passed` | Reviewer observed the required behavior; include observation time and artifact digest. |
| `result: failed` | Reviewer observed failure; include observation time and artifact digest. |
| `observed_at` | RFC3339 time after report creation, no later than import. |
| `evidence_sha256` | Lowercase SHA-256 of the separately retained test log or observation record. |

The CLI checks the digest's syntax, not the contents or truth of the external
artifact. Keep each artifact and the detailed scenario results available for
review. An accepted provider API response alone does not prove actual receipt.
Do not mark a cross-build as native execution, process SIGKILL as power loss,
or mock LEDs as physical observation. There is no `waived` or `not-applicable`
result; applicability comes from the installed configuration.

The generated checks cover:

- Installation: automated release checks, native runtime, actual user-systemd
  restart, reboot/recovery, VM power-loss durability, and external missed-heartbeat
  monitoring with the worker/host or network unavailable.
- Every configured provider: recognizable test receipt at the intended destination,
  payload boundaries, and delivery failure/recovery.
- Every route: outage/daily/catch-up monitoring, plus primary failure with fallback
  receipt and both-provider failure when a fallback exists.
- Every producer: generation cutover, receipt reconciliation and report monitoring.
- Signal when configured: account/reboot continuity, relay/account contention and
  receive maintenance on the native target architecture.
- LEDs when configured: visual patterns under load, healthy TTL expiry during a
  worker freeze, daemon restart, socket loss, persistence failure and exact owned
  state cleanup. Messenger delivery must continue during LED outages. These checks
  confer no permission to test PWR; separate explicit opt-in is still required.
- Interactive destinations/workers: reply identity, sender authorization, replay
  cutoff, read routing and drain/recovery. Configured mutation commands additionally
  require confirmation, idempotency and authorization-expiry observations.

For container Signal, fill `environment.signal_image_digest` with the tested
`sha256:…` image digest. For native production Signal, instead fill
`environment.signal_cli_sha256` with the exact enrolled native CLI hash; do not
supply both fields. For LEDs, fill `environment.led_version` and
`environment.led_config_sha256` from the tested daemon. These are **operator
attestations**; this offline command does not discover or verify running external
components. Socket paths and LED binding/UID configuration are pinned by the local
binding. Recollect evidence after an external image/account/daemon configuration
change even if the local report still matches. Automatic external-component
verification remains part of the unfinished production qualification gate.

Record the report, including partial or failed results:

```sh
deploy --dry-run notification-worker qualify --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --report /home/notify/notification-qualification.json
deploy notification-worker qualify --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --report /home/notify/notification-qualification.json
deploy notification-worker qualification-status --local \
  --root /home/notify/.local/share/deploy/notification-installation
```

Dry-run validates without writing. Apply serializes with lifecycle changes using
the installation operation lease and writes one immutable record plus a separate
acknowledged head. It changes no worker state or ownership. Report IDs are immutable:
after recording partial results, generate a new template for subsequent testing.
Exact retries retain the original record and timestamp. They can acknowledge an
interrupted historical report even after an update or expiry, but never make it
current again. Status always evaluates the latest record, including a later failure
or pending write; it does not select an earlier successful report.

`binding_matches` requires the exact installation revision, current local input
fingerprints and architecture/UID, with no future observations or clock rollback.
Reports expire 30 days after creation. Every update/rollback revision, configuration
or credential change requires new evidence. `evidence_complete` additionally
requires all generated checks to have passed, required environment attestations,
and a committed acknowledgement. It still does not mean production qualified.
`record_hash` is the exact acknowledged record pin consumed by `activate-delivery`.
During delivery, status evaluates the original activation binding; deactivate
before collecting or importing a new report.

History is limited to 64 records, with at most 32 abandoned JSON temporaries in
the report directory. Records are hash-linked and the independent head detects
missing or modified committed history. No record is automatically evicted. A crash
after the record but before its head leaves `acknowledgement_pending: true`; retry
the exact report before adding another. Status and runtime startup never replay
that pending acknowledgement. This protects accidental inconsistency, not against
an attacker controlling the installation account and rewriting all evidence.

The development host's known `218/CAPABILITIES` user-systemd probe failure remains
unresolved. Physical LED tests and native ARM64 execution remain deferred; leave
those checks pending until they are actually performed. No real provider receipt,
host reboot or external-monitor test was performed by adding this feature.

## Activation admission review

Inspect the prerequisites for the exact managed verification revision:

```sh
deploy notification-worker activation-review --local \
  --root /home/notify/.local/share/deploy/notification-installation
```

The JSON result contains the manifest hash, revision, worker identity, observation
time and sorted `checks` with `scope`, `stage`, `state` and a fixed `reason` code.
States are `satisfied`, `blocked` and `not_required`. Successful JSON output means
the review completed, including any blockers it found. `activation_allowed`,
`delivery_enabled`, `intake_enabled` and `execution_enabled` remain false;
`requires_activation_recheck` is always true. Keep the output private. It is an
observation, not a reusable activation token.

The review reads the latest qualification record and current credential
fingerprints. A newer failed/partial/pending report supersedes earlier success.
It checks local Telegram bot/destination identity without a provider request.
Signal profiles must match the enrolled account, socket and recovered inbox;
old outbound pins and legacy send profiles are reported as requiring an explicit
migration. `migrate-signal-profiles` supplies that proven transition for already
enrolled conversation profiles; legacy fixed-recipient enrollment remains separate.
After migration, the review exposes its `effective_config_hash` and validates
`signal/profiles` before checking the effective outgoing pins. New qualification
evidence must match the new manifest/revision. A configured conversation also
needs its corresponding intake adapter.

Worker-state checks require the gateway to have been stopped by the operator.
The review never stops it automatically. It holds the installation operation lease
and, when the gateway is inactive, the exclusive installation and worker leases.
A running/busy gateway returns blocked drain/state checks. The separate Signal
verifier should stay running: its managed process/socket health and publication
are inspected. Verification health does not imply delivery or intake authority.
No service is installed, enabled, reloaded or restarted by review.

Recovered state checks include exact publication/adoption proof, current database
anchors, historical request seals, retained Signal cursors and the latest Telegram
handoff. Telegram's same-boot wall/monotonic freshness and settling deadline are
rechecked without polling, acknowledging, renewing or re-dating observations.
Missing/latest-incomplete evidence never permits fallback to an older drain.

The database inspection creates no state files and performs no repairs. A nonempty
SQLite WAL or rollback journal blocks inspection; it is neither ignored nor
checkpointed by this command. Use the existing managed recovery/resume procedure
for interrupted runtime work before reviewing again. Dry-run validates only local
flag syntax and defers installation, manager, endpoint and state checks.

For supported initialized outbound-only Telegram installations, the installation
activation check identifies the available `activate-delivery` transition. The
review still grants no authority and keeps explicit blockers for producer cutover,
recovered/Signal runtime handoff and command/LED admission. Qualification evidence
alone does not perform those transitions.
