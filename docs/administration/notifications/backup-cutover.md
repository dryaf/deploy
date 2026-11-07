# Managed backup notification cutover

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

The gateway must be in `delivery` mode, already enroll the backup writer ID,
producer epoch and route, and have current qualification. Telegram-only backup
installations use version 1 specifications. Legacy Signal installations use the
version 3 adoption procedure below, including a gateway whose admitted Signal
service was published through recovery. Command and LED admission have their own
lifecycle operations.

Upgrade the backup host helper **and its pinned container image** to a build
supporting `backup-worker __notification-cutover-probe` before migrating. The
coordinator checks both capabilities, the host helper SHA256, the installed
configuration, exact legacy unit sources, loaded unit identities, process-group
stop behavior and absence of surviving sender containers. It refuses unfinished
backup setup/recovery, foreign report units and drop-ins. It uses this user's
canonical backup paths and systemd user manager.

## Review and apply

Run on the backup/gateway host as the installation user:

```sh
umask 077
deploy backup-worker notification-cutover --local --review > /absolute/review.json
```

Review includes an exact evidence hash, archive identity, retained journal head,
and every legacy outbox row. Accepted rows default to `accepted`; all other rows
default to `held`. Keep incomplete or ambiguous histories held. Change a row to
`unsent` only when independent evidence establishes that it was never sent, and
record that evidence in its bounded `reason`. Zero counters and an empty journal
are insufficient. Recorded attempts, reservations and counters cannot be
overridden by an `unsent` assertion. Accepted timestamps, historical envelopes,
batch members and `covered_by` links remain intact.

Create a private specification, replacing the placeholders and copying the
`config_hash`, `evidence_hash` and complete `decisions` map from the review:

```json
{
  "version": 1,
  "managed": {
    "root": "/absolute/notification-installation",
    "manifest_hash": "CURRENT_DELIVERY_MANIFEST_SHA256",
    "producer": {"id": "ENROLLED_BACKUP_WRITER_ID", "epoch": "ENROLLED_EPOCH"},
    "generation": "backup-shared-1",
    "route": "ENROLLED_ROUTE"
  },
  "helper_hash": "INSTALLED_BACKUP_HOST_HELPER_SHA256",
  "config_hash": "REVIEW_CONFIG_HASH",
  "evidence_hash": "REVIEW_EVIDENCE_HASH",
  "decisions": {
    "EXACT_OUTBOX_ID": {
      "hash": "REVIEW_ROW_HASH",
      "disposition": "held",
      "reason": "Old delivery history is incomplete; preserve without resending."
    }
  }
}
```

The helper is at `~/.local/lib/deploy/backup-worker/deploy`. Obtain the managed
manifest hash from `notification-worker installation-status --local --root ...`.
Use a new generation token; revoked tokens can never be reused. Empty outboxes
use an empty decisions object. Apply, or resume after an interruption, using the
**same specification**:

```sh
deploy backup-worker notification-cutover --local --apply --spec /absolute/cutover.json
```

If evidence changes before the intent is written, review again. Once an intent
exists, the exact request is retained; changing its pins or deleting evidence is
not a recovery procedure. Errors leave delivery fenced or stopped. Resume the
same request to complete the transition.

## Result and verification

The coordinator retains the backup setup, notifier and attempt leases, and the
gateway operation, installation and worker leases through the handoff. It writes
an independent intent file and SQL anchor, stages one generation, installs the
report units, activates that generation and publishes the shared backup owner.
The gateway restarts and must confirm that exact active generation before report
triggers start. Other producers keep their ownership histories and queues; the
gateway is briefly stopped during the operation.

`deploy-backup-report.service`, `.timer` and `.path` replace the legacy notifier
schedule. The native report process cannot access messenger credential files,
SSH keys, the Signal account directory or the backup archive. It evaluates
current state and uses the local gateway API; the gateway alone sends messages.
The initial report pass must finish successfully. Subsequent passes import
receipts and commit their domain effects before acknowledgements. Backup capture
continues under its existing service and archive locks.

Held legacy rows are excluded from exports and automatic batches, remain visible
as outstanding delivery, block completion of delivery catch-up, and degrade
shared report health. They do not starve fresh reports. The explicit reconciliation
workflow below can release individual held sources. A successful migration does
not certify that all historical notifications were delivered.

Inspect `systemctl --user status deploy-backup-report.{service,timer,path}` and
normal backup status/health. The legacy notifier timer/path remain disabled, and
compatible manual test sends and legacy setup/recovery honor the retained fence.
No legacy units or evidence are deleted.

## Signal account and notification migration

First complete [legacy account adoption](legacy-signal-adoption.md), publish the
gateway recovery, enroll its production Signal service and migrate its outgoing
conversation profiles. The original account receiver remains permanently fenced.
The production service must use the same account directory and CLI account
selector as the old relay; its runtime ancestry must retain the adoption bootstrap.

Run the cutover review after account adoption. In addition to the ordinary
evidence, it returns `signal_selectors` and `signal_account_selector`, fingerprints
of the current private selector files. A phone-number selector and the gateway's
logical UUID are distinct representations. Explicitly review that the enrolled
conversation identifies the intended old recipient; record that mapping in
`reason`. The operation never infers it from an empty inbox or discarded history.

Use `version: 3` in the cutover specification, retaining the ordinary fields, and
add:

```json
{
  "signal": {
    "adoption": "REPLACE_WITH_ENTIRE_SAVED_ADOPTION_JSON_OBJECT",
    "profile": "REPLACE_WITH_EXACT_EFFECTIVE_SIGNAL_PROVIDER_PROFILE_OBJECT",
    "selectors": "REVIEW_SIGNAL_SELECTORS_SHA256",
    "reason": "Reviewed original recipient selector against the enrolled conversation."
  }
}
```

`adoption` and `profile` must be JSON objects, not placeholder strings. Copy the
profile from the gateway's retained effective `signal-runtime.json` release:
select the Signal provider referenced by the backup route. Include its exact
conversation, destination fingerprint, socket and relay identity. The coordinator
compares it with the admitted effective gateway profile under the gateway leases.
Primary/fallback provider order must match the backup policy. Telegram/Signal
fallback configurations preserve that order. Unknown outcomes remain held; legacy
acceptance never invents a native reply ID or a canonical gateway receipt.

The same cutover, revoke, rollback and remigration commands apply. For rollback,
the native legacy notifier uses the pinned conversation-only `/reply` endpoint of
the shared Signal service under the canonical legacy generation. It does not
start a second Signal receiver or access account storage. Selector changes after
review prevent sends; unavailable or changed relay identities cannot redirect
them. Preserve the adoption receipt, bootstrap inbox, account recovery proofs and
effective profile release throughout these operations.

The host helper must advertise `__notification-signal-migration-probe`. The
coordinator checks the exact old Signal notifier/test/relay units and stops their
process groups before granting shared notification ownership. Provider and real
systemd qualification remain manual.

## Revoke safely

For a later return to legacy delivery after remigration or recovery, use the
cycle procedure below while the current shared generation is active. It performs
its own revocation and receipt reconciliation. Ordinary `--revoke` remains the
terminal stop operation for a particular cutover/remigration specification.

Completed shared installations support ordinary backup setup/update, restricted
source key rotation and activation. Setup journals pin the shared owner and
report credential exclusions; rollback restores the previous files while keeping
legacy senders fenced. Activation still requires recent backup/source and
monitor qualification and enables report triggers instead of legacy notifiers.

After an update changes the backup config, helper or gateway manifest, prepare a
continuation from the retained original cutover specification: set `version` to
`2`, set `predecessor` to the original cutover intent hash (`notification_cutover`
in backup state), and replace only `config_hash`, `helper_hash` and
`managed.manifest_hash` with their current reviewed values. Keep the original
producer, generation, root, route, evidence hash and decisions. A new cutover
review supplies the current configuration hash; its newly observed source rows
must not replace the original evidence in a continuation. Version 2 is accepted
only for revocation of existing authority. Use the same continuation as `cutover`
in a subsequent legacy rollback specification. Retain the original intent file.

```sh
deploy backup-worker notification-cutover --local --revoke --spec /absolute/cutover.json
```

Revocation disables/drains reports, tombstones the selected shared generation and
imports available canonical receipts without messenger credentials. Even an
interrupted handoff that never activated receives a generation tombstone. The
legacy fence and report artifacts remain. An expired gateway qualification can
prevent its restart after revocation; the tombstone remains durable.

Revocation retains the fence. Use the separate restoration workflow below when
its evidence and executable prerequisites are satisfied. Do not clear
`notification_owner`, remove `notification-cutover.json`, reset receipts, or
restart an older helper to bypass reconciliation.

## Repeat a legacy/shared cycle after remigration or recovery

Resolve held legacy source decisions first. Review the current installation:

```sh
umask 077
deploy backup-worker notification-cycle --local --review > /absolute/cycle-review.json
```

Prepare a private cycle specification with `version: 1`, the current `managed`
root/manifest/producer/generation/route object, current `helper_hash`, the review's
`config_hash` and `owner_hash`, and a bounded audit `reason`. Apply or resume:

```sh
deploy backup-worker notification-cycle --local --spec /absolute/cycle.json
```

The helper must advertise `__notification-cycle-probe`. Capture fences local
reporting before revoking the canonical generation. The cycle reconciles its
receipts and retains completed rollback/remigration files, source decisions and
exact export payloads in independently checked history pages. Existing accepted
receipts keep their original recovery identity. A missing current-epoch receipt
is accepted as historical evidence only when its exact eligible acceptance is
present in the immutable gateway publication checkpoint.

Save the returned `cycle_hash`. Prepare a new rollback specification as below,
using a distinct legacy generation. Its nested `cutover` uses `version: 4` and
`predecessor: CYCLE_HASH`; retain the original cutover `evidence_hash` and
`decisions`, but use the just-revoked current shared generation, current managed
manifest, configuration and helper pins. For Signal, include the current admitted
conversation profile and original adoption proof. Review the legacy credential
pin, apply the rollback, and revoke that legacy generation when returning to
shared delivery. Then use `notification-remigrate --review` and its normal
procedure with a fresh shared generation.

Every cycle has separate rollback/remigration artifacts under
`notification-migration-cycles/CYCLE_HASH/`. Preserve all original root artifacts,
cycle records, export pages and recovery proofs. Retries use the exact retained
request; completed retries do not restore an earlier owner. During interrupted
capture, reporting, backup activation and other ownership transitions stay
fenced until that cycle is resumed. The current implementation bounds a retained
chain at 32 cycles and 10,000 exports per capture; reaching a bound is a refusal,
never permission to delete evidence or reset a generation.

## Resolve one held legacy source

```sh
umask 077
deploy backup-worker notification-resolve --local --review --source EXACT_OUTBOX_ID > /absolute/resolution.json
```

Review the original row, current row, owner and attempt journal. Leave `review`
and `review_hash` unchanged. The template's `action: "held"` is intentionally
not applicable. Select one of these actions and supply a bounded, concrete reason:

- `unsent`: independent evidence establishes no send. Recorded attempts, counters,
  reservations or acceptance cannot be overridden by this assertion.
- `retry`: an explicit retry despite incomplete or ambiguous old outcomes.
  Set `ack_duplicate_risk: true`. The recipient may already have received the
  original message. Known accepted attempts cannot be retried through this command.

Apply a new review within five minutes:

```sh
deploy backup-worker notification-resolve --local --apply --spec /absolute/resolution.json
```

This operation records an immutable file and a matching SQL grant under the
notifier and legacy attempt leases. It does not send, manufacture a receipt, or
mark any period delivered. Original cutover rows/dispositions, messages and attempt
records are retained; old helpers continue to treat those rows as held. New report
passes export them only after checking the resolution. Normal canonical acceptance
and eligibility remain necessary for coverage and catch-up completion.

After interruption, retry the exact specification. An already committed decision
does not reopen accepted work. Changed authority/evidence and missing independent
files refuse progress. After intent publication, harmless report-text/due refreshes
can resume; changed attempts or batch membership cannot. Historical acceptance
assertions, cancellation/waiver decisions and arbitrary replacement of a committed
resolution are not implemented by this command.

## Restore legacy delivery under a new generation

This first restoration supports the same initialized Telegram-only profile as
cutover. The installed backup host helper must already support
`backup-worker __notification-legacy-probe`; both pinned gateway executables must
support `notification-worker __managed-legacy-probe`. Use these capable binaries
before the original cutover. This command does not replace old helpers, migrate
an older installation revision or rewrite the original cutover pins.

First resolve all held legacy rows and run `notification-cutover --revoke` with
the original specification. Create a private restoration specification:

```json
{
  "version": 1,
  "cutover": {"COPY": "the complete original cutover specification object"},
  "generation": "backup-legacy-2",
  "reason": "Explain the reviewed decision to restore legacy delivery."
}
```

Replace the illustrative `cutover` object with the exact original object; `COPY`
is not a supported field. Choose a previously unused generation token.
Review the native sender's current credential binding. This prints the specification
with a combined `credential_hash`, never the token or chat ID. Save it privately
and use that exact result for apply and revoke:

```sh
umask 077
deploy backup-worker notification-rollback --local --review --spec /absolute/rollback.json > /absolute/reviewed-rollback.json
deploy backup-worker notification-rollback --local --apply --spec /absolute/reviewed-rollback.json
```

The coordinator stops old sender containers, shared report units and any restored
native sender. Under retained setup/notifier/attempt and gateway leases, it checks
the canonical shared tombstone and outcomes. Never-attempted shared envelopes need
canonical no-attempt proof; absence below the submission watermark is insufficient.
Accepted exports need retained eligible canonical acceptance. Ambiguous, attempted
unaccepted, ineligible accepted, missing or compacted required evidence blocks
restoration. Resolve those outcomes through the shared worker's supported
administration/recovery workflow before attempting restoration; this command
does not acknowledge duplicate risk on their behalf.

A distinct `legacy-active` canonical generation is published before the local
legacy owner. Shared dispatch cannot grant this generation. Each bounded legacy
provider pass retains a shared canonical ownership lock, rechecks its file/inode
identity before sends, and journals the generation with its provider reservations.
Each native pass also audits the full ownership history and its directory tail;
a rolled-back head cannot hide a later revocation behind a missing adjacent record.
Canonical revocation needs the exclusive lock. A lost ownership anchor fails closed.
Changed/missing canonical worker identity or a recovery/publication/schema-migration
gate also fences native delivery, even before the producer owner is migrated.
The reviewed native Telegram policy and credential fingerprint are retained in
the rollback intent. Changes fail admission, and the request constructor checks
the exact token/chat values it uses before any network request.

New native `deploy-backup-legacy.service`, `.timer` and `.path` run the existing
legacy notifier semantics. They can read the enrolled Telegram/monitor credentials;
source SSH keys, Signal account storage and the archive remain inaccessible.
The native credentials and canonical gateway state must be outside those denied
roots; preflight rejects an overlapping layout before ownership publication.
Original sender containers and shared report triggers stay stopped. Capture stays
on its existing schedule. The gateway restarts for other producers.

Exact retries resume the retained transaction. Revoke restored legacy delivery with:

```sh
deploy backup-worker notification-rollback --local --revoke --spec /absolute/reviewed-rollback.json
```

Revocation stops the native schedule and tombstones the new generation; it does
not need current messenger credentials/qualification. A gateway restart can still
fail after durable revocation. Evidence remains and the same generation cannot be
reactivated. Return to a fresh shared generation is described below. General backup
setup/update after cutover and restoration across older helper/revision upgrades
remain separate work. Ordinary legacy setup, manual provider test commands and
the original cutover command refuse this retained restored lifecycle.

Fixture validation covers lost responses, all coordinator interruption boundaries,
real `SIGKILL`, retained leases, foreign units, exact pin rejection and independent
producer preservation. Real user-systemd/rootless Podman and live Telegram
qualification remain operator checks on a disposable host.

For explicit reporting after the first gateway recovery, see
[backup activation after recovery](backup-recovery-activation.md).

## Return to shared delivery after legacy rollback

`notification-remigrate` supports the initialized Telegram profile after the
original shared generation and its restored native legacy generation have both
been durably revoked. It allocates a distinct shared generation. Keep the original
cutover, rollback and source state; this is not a new initial installation.

```sh
umask 077
deploy backup-worker notification-remigrate --local --review > /absolute/remigration-review.json
```

Build a private specification using that review and current gateway installation
pins. The nested `cutover` uses the same fields as the original cutover spec,
including the exact installed helper hash and managed producer/route. Select a
fresh `managed.generation`, preserve the producer identity, and use the new review's
`config_hash`, `evidence_hash` and row hashes. The outer object is:

```json
{
  "version": 1,
  "rollback_hash": "SHA256_FROM_REMIGRATION_REVIEW",
  "cutover": {
    "version": 1,
    "config_hash": "SHA256_FROM_REMIGRATION_REVIEW",
    "helper_hash": "INSTALLED_HELPER_SHA256",
    "evidence_hash": "SHA256_FROM_REMIGRATION_REVIEW",
    "managed": {},
    "decisions": {}
  }
}
```

Populate `managed` with the current root, manifest hash, producer, route and fresh
generation fields described above. Populate a decision for every reviewed row:
`accepted` only when retained native evidence proves acceptance, `unsent` only
when no source attempt or journal reservation exists, otherwise `held`. Every
decision needs the exact row hash and a bounded audit reason. Review does not
select these decisions or activate anything.

```sh
deploy --dry-run backup-worker notification-remigrate --local --spec /absolute/remigration.json
deploy backup-worker notification-remigrate --local --spec /absolute/remigration.json
```

Dry-run validates arguments only. Apply audits exact installation, helper, units,
source evidence and both canonical revocations under retained leases. It stops
native legacy scheduling, journals the new owner, installs report units and
activates the fresh generation. Capture continues independently. Compatible helper
probes are mandatory; the new owner schema makes older report helpers refuse it.

Accepted native outcomes remain historical acceptance; they do not create a shared
provider receipt or a new acceptance time. Proven-unsent work receives a fresh
export ID and increasing sequence. Ambiguous work stays held, including work that
first appeared during the legacy interval. Use `notification-resolve` with a fresh
review and explicit duplicate-risk acknowledgement to release a held source.
Its approval is bound to this remigration; an earlier approval cannot release the
new uncertain outcome. Earlier source rows and approval files remain immutable.

Retry the exact specification after interruption. Missing independent intent,
completion or source-decision evidence blocks sending. To revoke the new shared
generation, use the same specification:

```sh
deploy backup-worker notification-remigrate --local --spec /absolute/remigration.json --revoke
```

Revocation is permanent. For additional cycles, use the migration-cycle procedure
above. Recovered gateway epochs, adopted Signal accounts and shared-owner backup
setup/update retain their independent owner and helper proofs as described above.
