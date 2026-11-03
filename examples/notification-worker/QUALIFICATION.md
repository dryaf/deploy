# Installation qualification evidence

The local CLI can now record dated operator observations for a completed managed
verification installation. It does not run the checks, contact providers or the
LED daemon, install services, or enable production delivery or chat commands.
`qualified` and `activation_allowed` remain false even when all evidence is present.
The production lifecycle, final recovery publication and migration gates remain
unfinished. Existing fixture tests are not live qualification evidence.

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

For Signal, fill `environment.signal_image_digest` with the tested `sha256:…`
image digest. For LEDs, fill `environment.led_version` and
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
