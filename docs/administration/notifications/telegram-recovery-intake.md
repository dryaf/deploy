# Telegram recovery intake preparation

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

These commands complete the intake preparation for a stopped, quarantined
Telegram gateway. They preserve request identities and uncertainty; they do not
enable delivery or commands. Use the exact canonical runtime configuration used
for `recovery-prepare`. Keep the whole worker and installation state, independent
specialist ledgers, and both the snapshot being restored and the separately
retained latest gateway snapshot.

For a managed installation, deactivate delivery, revoke active producer
generations as required, and stop the gateway before taking the latest retained
history snapshot with `managed-snapshot`. Record its snapshot ID and database
hash. Prepare recovery from the selected older snapshot using the managed
`recovery-prepare --root ... --manifest-hash ...` workflow. Record the resulting
recovery operation. The older restore snapshot and latest retained history
snapshot may differ; their pins must not be interchanged.

Import the independently retained Telegram history:

```sh
deploy notification-worker recovery-telegram-history --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --operation RECOVERY_OPERATION \
  --from /home/notify/recovery/latest-gateway-history \
  --snapshot-id HISTORY_SNAPSHOT_ID --database-hash HISTORY_DATABASE_SHA256
```

The worker verifies the snapshot and its original gateway epoch, then preserves
its provider/request evidence under the exclusive worker lease. It neither reads
bot credentials nor contacts Telegram. Exact retries retain the same completion.
An unavailable latest snapshot is not proof of empty history; preserve the
quarantine and resolve the missing evidence.

Stop **all** other receivers using this bot account, including copies outside
this installation. The local worker lease cannot fence a receiver on another
host. Explicitly drain Telegram into the quarantine:

```sh
deploy notification-worker recovery-telegram-drain --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --operation RECOVERY_OPERATION --receivers-stopped
```

This command contacts Telegram. It checks the account and webhook state, starts
at offset zero, records each response before acknowledging a later offset, and
stops at a recorded empty response or the bounded page/time limit. It sends no
messages and never deletes a webhook. A completed empty response is a dated
observation, not proof that the provider retained every historical update. An
interrupted invocation resumes the same durable operation; a completed invocation
returns its existing observation rather than renewing its age.

Complete the available specialist notification and command reconciliation next.
For the backup producer, use `recovery-notifications` followed by
`recovery-handoff` as documented in [backup activation after recovery](backup-recovery-activation.md).
Other specialists must reconcile, commit and install their command evidence
through the shared recovery contracts. Fence unavailable producers explicitly.
Complete any configured Signal history import before sealing the shared request
inventory. Reconciliation may import command mappings absent from the restored
snapshot; sealing before those imports correctly prevents their later insertion.

After those imports are complete, freeze the request inventory:

```sh
deploy notification-worker recovery-requests-seal --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --operation RECOVERY_OPERATION --reconciliation-complete
```

Known requests keep their original command identity and outcome. Unknown old
inputs remain quarantined and cannot acquire a new command, query or confirmation
identity. The acknowledgement flag confirms the operator's sequencing decision;
it does not claim that unavailable history was recovered. Missing or contradictory
retained evidence remains an error. This command does not contact a provider.

Commit the Telegram cursor handoff while the drain observation remains fresh:

```sh
deploy notification-worker recovery-telegram-cursor --local \
  --config /home/notify/.config/deploy-notification/runtime.json \
  --state-dir /home/notify/.local/state/deploy-notification \
  --operation RECOVERY_OPERATION
```

The cursor binds the completed drain and request seal, with the existing
five-second cutoff and ten-minute freshness/clock requirements. It grants no
intake permission. Finish status review, checkpoint, verification and recovery
publication, then adopt the published recovery in verification mode. Use the
separate managed `renew-telegram`, fresh qualification, delivery activation and
explicit command enable sequence in [command activation](command-activation.md).

All four commands require the exact recovery operation and reject options for
another step. Prefix with `deploy --dry-run` to validate arguments and runtime
configuration without opening worker/snapshot state or contacting Telegram.
Dry-run does not verify the current on-disk operation or snapshot. Ordinary and
integration tests inject synthetic provider responses; real receipt and provider
retention qualification remain manual.
