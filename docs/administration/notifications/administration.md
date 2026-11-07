# Local notification administration

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

These local administrative commands require an existing, stopped notification
worker and its exact runtime configuration. They take the exclusive worker lease
and refuse to run alongside the service. Stop/start the service explicitly; the
commands never manage services, contact providers or change LED state. Managed
verification mode continues to withhold delivery after a retry.

## Command enrollment

See [Managed command enrollment changes](command-migration.md) for retained alias
renames, conversation removal/generation rotation and recovered configuration
changes through `migrate-commands`.

## Producer retirement

Retirement permanently closes one enrolled producer epoch. First publish explicit
subject retirement through the producer, finish LED cleanup through its normal
evidence-checked path, settle deliveries/commands and acknowledge all receipts
and command results. Revoke every generation, including staged generations, then
stop the notification worker. Retirement itself performs none of these steps.

Preview eligibility with the exact installed configuration:

```sh
deploy notification-worker producer-retire --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --producer backups --reason 'Source decommissioned after obligations settled'
```

Add `--apply` to publish the permanent decision. The command rechecks eligibility
under the exclusive worker lease; preview does not reserve eligibility. The JSON
result includes the decision ID, local UID actor, reason, original timestamp and
evidence hash. Repeating the same applied decision returns that original record;
a different reason or actor is refused. Global `--dry-run` checks arguments and
configuration only, without opening state. Ordinary preview opens state and may
complete a previously durable ownership intent during startup recovery.

Retirement refuses active/staged/draining ownership, pending recovery, unsettled
messages or commands, unacknowledged receipts/results, pending command replies,
active policy faults/transitions and subjects not explicitly retired. It also
refuses remaining LED fault/cleanup rows for that producer's bindings or the
worker aggregate. Resolve those through their existing authority; retirement
does not cancel work, waive incidents, acknowledge outcomes or clear LEDs.

The durable ownership journal fences new message/status/policy submissions,
command admission/grants/results and generation activation. Health reports
`retired_producers` and stops requiring those producers' heartbeats. Retirement
does not assert healthy output or hide another producer's faults. Existing queue,
receipt, command and audit history remains available, including historical chat
queries. Fresh `workers` listings mark the producer as retired; worker help
explains retirement instead of advertising executable commands. These diagnostics
still require current chat permissions. Intermediate receipt compaction remains
supported.

Keep the producer's configuration enrollment and the whole state directory,
including canonical ownership history. There is no in-place unretire operation,
in-place configuration removal. Whole-installation UID/socket/configuration and
history changes use [retirement and replacement](retirement-replacement.md), with
fresh specialist enrollment. The retirement record survives ownership recovery
epoch changes; incompatible restored ledgers fail closed. Recovered publication
and repeated adoption use [delivery recovery](delivery-activation.md). Schema-4
ownership events require a compatible runtime and retained recovery helper;
older binaries reject them. This does not change SQLite schema 10 or enable
production delivery.

## Message retry and cancellation

Administrative history uses storage schema 10. Older binaries refuse this state;
schema-6/7/8/9 snapshots remain readable by the new binary in quarantine. Managed
administration refuses to upgrade an older database, and managed lifecycle updates
still require matching runtime/recovery-helper schema support. Do not downgrade
the version field or substitute an older helper to bypass that fence. The existing
unmanaged staging open path upgrades compatible older state transactionally.

Inspect the live queue first with `queue-inspect` (see `INSTALLATION.md`). Then,
with the worker stopped, obtain a private review of an exact event:

```sh
umask 077
deploy notification-worker message-review --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --producer backups --message EVENT_ID \
  > /home/notify/message-review.json
```

Read the JSON before deciding. It contains the complete frozen message text,
route revision, attempts, producer/generation and worker epoch. Protect it as
private data; `message-decide` requires an owned, regular, single-link `0600` file
at an absolute path. Reviews expire in five minutes and after reboot or clock
discontinuity. Changes to the message, receipt history, runtime configuration or
ownership require a new review.

Retry an ambiguous event only after considering that the provider might already
have accepted it:

```sh
deploy notification-worker message-decide --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --review /home/notify/message-review.json \
  --action retry --reason 'Reviewed uncertain provider response' \
  --accept-duplicate-risk
```

Retries apply only to `blocked` or `ambiguous` messages. A blocked event does not
require the duplicate-risk flag. The event's payload, route, expiry, generation
and cumulative attempt count stay unchanged. Expired/superseded events, changed
routes and revoked/draining generations cannot be revived. Retrying resets the
provider selection to the original route's primary; ordinary dispatch still
checks cooldowns, ownership, incident serialization and current producer evidence.
It does not create a replacement event or reset the producer sequence.

To cancel, use `--action cancel --reason '…'` and omit
`--accept-duplicate-risk`. Any nonterminal event without an in-flight attempt can
be cancelled. Cancellation records an unsuccessful terminal receipt. It does not
resolve the producer's underlying incident, satisfy a delivery monitor or authorize
an LED clear. The producer still imports the receipt through its normal protocol.

The JSON result includes the decision ID (`decision.review.id`) and original
receipt. If output was lost or the command was interrupted, inspect that ID:

```sh
deploy notification-worker message-decision-status --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --decision n-REPLACE_WITH_REVIEW_ID
```

Repeating the exact committed decision returns its original result, even after
its review expires or the event subsequently progresses. It cannot reset an
accepted/cancelled event. Changing an instruction under the same decision ID
fails. An uncommitted intent never applies on startup: explicitly retry it while
the review remains current, or obtain a fresh review. A lost result file can be
recreated only from the matching committed database record. Conflicting/missing
audit evidence blocks normal startup pending recovery.

Inspection and administration never advance the producer's listed/acknowledged
receipt floors. Administrative receipts carry no delivery acceptance proof.
Snapshot recovery retains old-epoch decisions, including decisions newer than the
snapshot, solely as audit. They never enter the new acknowledgement stream or
reapply their actions. The preserved original database and independent
`administration` directory are required for this reconciliation; preserve the
whole state directory. Recovery publication and first managed adoption are separate explicit steps;
see [delivery activation](delivery-activation.md).

History is bounded to 1,024 distinct decisions, including interrupted intents.
At the limit new decisions fail; existing pending decisions can finish. No audit
or queue data is silently evicted. At the bound, settle outstanding work and use
[retirement and replacement](retirement-replacement.md) to retain the audit archive
and begin a fresh installation. There is no in-place administrative history deletion. `--dry-run` validates arguments and the review file without opening
or modifying worker state; it does not prove that the review is still current.

## Receipt compaction

For a stopped worker, preview a bounded pass over one producer's intermediate
receipt history:

```sh
deploy notification-worker receipts-compact --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --producer backups --limit 128
```

Add `--apply` to delete eligible rows. Each pass rechecks the current state in one
transaction; a prior preview does not freeze eligibility. The default audit window
is 90 days. Set `worker.receipt_audit_days` in runtime JSON to a value from 90 to
3,650 to retain intermediate history longer. Omission uses 90 days and preserves
existing configuration hashes. Managed configuration changes still require an
explicit migration; editing a pinned release is not supported.

A receipt is eligible only when its event is terminal, no attempt is in flight,
the producer has acknowledged the event's complete receipt history, and both the
receipt and the final outcome are older than the retention window. The cutoff is
conservative at subsecond boundaries. Compaction preserves:

- Each event's latest receipt, including its original acceptance-time evidence.
- All pending/unacknowledged event history and administrative decision receipts.
- The acknowledgement endpoint and listed/acknowledged cursor values.
- Full immutable messages, sequence/event-ID fences, incident/report revisions,
  command ledgers and native/opaque chat references with their existing expiry.

Results include exact candidate positions, `more`, logical bytes removed and a
cumulative compaction summary. `through` is the greatest discarded position;
retained records can exist below it. It is **not** a contiguous history floor or
an acknowledgement cursor. `receipts-inspect` exposes the same summary. An old
producer cursor still requires reconciliation; compaction never invents delivery
success. Snapshots retain and validate the compaction summary. After a recovery
epoch change, old summaries remain historical data and cannot authorize new acks.

Deletion and audit counters commit together. A process crash leaves either the
old history or a complete pass; rerunning continues with currently eligible rows.
No empty pass increments the audit. The command makes no provider, LED or service
manager calls. Preview does not delete receipts or change audit/cursor values;
opening state still performs normal startup validation and interrupted-attempt
recovery. Global `--dry-run` opens no worker state and checks arguments only.

Each pass selects at most 1,024 rows and the CLI has a 30-second operation budget.
Compaction needs writable SQLite journal space and outcome headroom; it can fail
on a completely full filesystem. Freed SQLite pages can be reused, but logical
bytes removed are not a promise of immediate filesystem shrinkage. No automatic
`VACUUM`, message-body deletion, final-outcome deletion or administrative-audit
compaction is performed.
