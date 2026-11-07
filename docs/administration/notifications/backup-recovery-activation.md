# Resume backup reporting after gateway recovery

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

The offline `backup-worker recovery-handoff` reconciles notification and command
history and allocates a new producer generation. It keeps reporting fenced.
`notification-recovery-activate` explicitly admits that generation after the
recovered gateway has been published, adopted and qualified for delivery.
The currently supported managed delivery profile is Telegram. This operation
neither enables gateway commands nor changes their separate admission.

Before the original cutover, install a backup helper that supports
`backup-worker __notification-recovery-activation-probe`. The coordinator checks
its exact hash and capability before changing ownership. General backup helper
replacement after shared cutover remains a separate lifecycle limitation.

1. Complete the existing source reconciliation and `recovery-handoff`, including
   command outcome installation when that producer has an interactive ledger.
2. Complete gateway recovery review, checkpoint, publication and managed
   `adopt-recovery`. Retain the original source databases and all recovery files.
3. Complete the current installation's qualification and
   [activate gateway delivery](delivery-activation.md). Individual producers
   remain staged until explicitly activated.
4. Read the source's retained recovery commit:

   ```sh
   deploy backup-worker notification-recovery-activate --local --review
   ```

5. Create a private specification (mode `0600`) with these exact fields:

   ```json
   {
     "version": 1,
     "managed": {
       "root": "/home/notify/.local/share/deploy/notification-installation",
       "manifest_hash": "CURRENT_GATEWAY_MANIFEST_SHA256",
       "producer": {"id": "RECOVERED_PRODUCER_ID", "epoch": "PRODUCER_EPOCH"},
       "generation": "RECOVERED_GENERATION",
       "route": "RECOVERED_ROUTE"
     },
     "helper_hash": "INSTALLED_BACKUP_HELPER_SHA256",
     "config_hash": "REVIEW_CONFIG_HASH",
     "commit_hash": "REVIEW_COMMIT_HASH"
   }
   ```

   Copy producer, generation and route from the review's `delivery.After`.
   Obtain the current gateway `manifest_hash` from `installation-status` and
   hash the installed `~/.local/lib/deploy/backup-worker/deploy` helper. Both
   installations must use the same canonical user manager.

6. Apply, or resume the same request:

   ```sh
   deploy backup-worker notification-recovery-activate --local --spec /absolute/recovery-report.json
   ```

The coordinator stops report triggers, retains source and installation leases,
verifies the exact handoff against the gateway's immutable checkpoint and
ownership history, and activates only the previously allocated generation.
Independent source intent/completion files and SQL records preserve the original
recovery and cutover evidence. Existing notification sequence numbers continue;
new-epoch receipts start at the committed cursor and advance through normal
listing and acknowledgement. Restored accepted events are not resubmitted.

To stop this recovered report generation permanently:

```sh
deploy backup-worker notification-recovery-activate --local --spec /absolute/recovery-report.json --revoke
```

Retry revocation after interruption. Revocation keeps legacy delivery fenced and
cannot be undone by replaying activation. Keep the exact specification and all
retained history; deleting records does not authorize another generation. Further configuration/identity changes remain separate lifecycle work.

`deploy --dry-run backup-worker notification-recovery-activate ...` validates
arguments without opening state, contacting a manager or invoking a provider.
Automated tests use simulated services and transports. Actual user-manager,
provider receipt and outage qualification remain manual checks.

## Subsequent gateway recovery

Use a recovered managed snapshot, preserving its `recovery-proof` archive. Stop
backup report triggers, revoke the old gateway producer generation, disable gateway
commands, and deactivate delivery before `recovery-prepare`. Keep backup ownership
in shared mode; do not use source activation revocation to prepare another recovery,
since that permanently revokes that source admission.

Run `recovery-notifications` and `recovery-handoff` for the new operation with the
current backup configuration. The source automatically retains the completed prior
activation, reconciliation, export pages and command result floors before fencing
and capturing the new epoch. Historical report generations and receipt identities
remain unchanged. Resume the same operation after interruption; never delete prior
activation or history files to force a retry.

Complete provider history, request sealing and cursor handoff before publishing the
new gateway. Then repeat the review/specification/activation procedure above with
the new gateway manifest, generation and local commit. The installed helper must
also pass `backup-worker __notification-next-recovery-probe`. Fresh reporting is
admitted only against that exact published gateway; old command effects and result
receipts remain historical. Losing any referenced ancestor file, page or SQL proof
blocks activation and reporting.
