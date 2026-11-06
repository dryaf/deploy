# Managed backup notification cutover

The supported profile is an initialized managed gateway in `delivery` mode,
using fixed-recipient outbound Telegram, and a backup installation whose sole
provider is Telegram. The gateway must already enroll the backup writer ID,
producer epoch and route. Its qualification must remain current. Signal,
recovered gateways, interactive commands and LED admission are not supported by
this operation.

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

## Revoke safely

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
reactivated. Subsequent legacy/shared generation migration, general backup
setup/update after cutover, and restoration across older helper/revision upgrades
remain separate work. Ordinary legacy setup, manual provider test commands and
the original cutover command refuse this retained restored lifecycle.

Fixture validation covers lost responses, all coordinator interruption boundaries,
real `SIGKILL`, retained leases, foreign units, exact pin rejection and independent
producer preservation. Real user-systemd/rootless Podman and live Telegram
qualification remain operator checks on a disposable host.
