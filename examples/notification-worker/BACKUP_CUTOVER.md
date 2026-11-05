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
shared report health. They do not starve fresh reports. This version intentionally
has no command to release held outcomes; resolving them requires a future explicit
outcome reconciliation workflow. A successful migration does not certify that all
historical notifications were delivered.

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

**This is a fenced rollback, not automatic restoration of legacy sending.**
Restoring legacy authority requires reconciliation of shared accepted/ambiguous
outcomes and a new legacy generation, which is not implemented here. Do not clear
`notification_owner`, remove `notification-cutover.json`, reset receipts, or
restart an older helper to bypass this requirement.

Fixture validation covers lost responses, all coordinator interruption boundaries,
real `SIGKILL`, retained leases, foreign units, exact pin rejection and independent
producer preservation. Real user-systemd/rootless Podman and live Telegram
qualification remain operator checks on a disposable host.
