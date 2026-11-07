# Adopt the legacy backup Signal account

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

The old backup relay has an account directory and fixed-recipient sender, but no
interactive inbox. Adoption keeps that account directory and its lock inode. It
permanently fences ordinary legacy account consumers, stops the exact enrolled
Quadlet, drains inherited CLI leases, and creates an empty offline inbox. It does
not claim that discarded provider input has been recovered. It starts no receiver
and does not change backup notification ownership.

The installed relay image must advertise `backup-worker __signal-adoption-probe`.
Upgrade it through the backup installer before beginning migration if necessary.
The capability check runs without account mounts or networking. Keep the installed
backup configuration, image enrollment, original Quadlet and generated service
fragment; the coordinator checks their exact binding throughout the handoff.

1. Stop backup notification triggers and finish current sends. Review the installed
   account service as its owning user:

   ```sh
   deploy backup-worker signal-adoption-review --local --home /home/backup
   ```

   Save `config_hash` and `service_binding`. A changing or ambiguous service
   definition must be corrected before adoption. Read the logical Signal account
   UUID from the account's existing enrollment; do not put a phone number, token
   or account secret into a specification.

2. Create a new empty directory with mode `0700` for the offline source inbox,
   separate from backup state, account storage and configuration. Fill
   [backup-signal-adoption.json](../../../examples/notification-worker/backup-signal-adoption.json), using a fresh
   `n-` operation ID followed by 32 lowercase hexadecimal characters. Keep the
   specification private (`0600`). Apply it and save the JSON result privately:

   ```sh
   deploy backup-worker signal-adopt --local --spec /absolute/private/adoption.json
   ```

   Retry that exact specification after interruption. The bootstrap keeps its
   original identity across retries. Existing inboxes or foreign files are refused.
   A child still holding the old account lease prevents bootstrap until it exits.
   Missing committed evidence is a refusal, never permission to recreate an inbox.

3. Use the returned `inbox.Identity` and public account UUID in the gateway's
   `interactive_signal` configuration. Keep its old socket separate from the new
   endpoint. Stage the gateway in verification mode and prepare a recovery using
   the ordinary managed snapshot/recovery procedure. Import the bootstrap source
   with `recovery-signal-history`, seal requests after all source reconciliation,
   then run `recovery-signal-cursor`. No provider polling occurs during this import.
   Complete gateway review/checkpoint/publication. The empty inbox does not replace
   any required backup producer or command reconciliation.

4. Fill [backup-signal-handoff.json](../../../examples/notification-worker/backup-signal-handoff.json), copying the entire
   saved adoption result into `adoption`. Use a new operation ID, the exact gateway
   configuration digest and publication commit, and another empty private directory
   for the installed inbox. Apply or resume:

   ```sh
   deploy backup-worker signal-adoption-handoff --local --spec /absolute/private/handoff.json
   ```

   This retains the bootstrap account epoch and installs the published gateway
   cursor under the next account fence. The legacy service stays stopped. The
   output contains `installation` and `account` proofs for the existing
   [Signal verification and production service procedure](signal-verification.md).
   The runtime inbox keeps the legacy bootstrap proof as part of its installation
   hash; losing that source evidence blocks subsequent verification and startup.

5. Prepare the new endpoint, enroll the native production service, adopt it into
   the managed gateway, map fixed-recipient profiles through explicit conversation
   enrollment, and freshly qualify/admit delivery and commands as documented in
   [Signal production](signal-production.md). Preserve the original account,
   bootstrap inbox and both account recovery epochs. Account adoption is separate
   from the backup producer's notification cutover and its ownership journal.
   Continue with the [version 3 Signal notification migration](backup-cutover.md#signal-account-and-notification-migration)
   after the shared service and its outgoing conversation profile are admitted.

`deploy --dry-run backup-worker signal-adopt ...` and
`signal-adoption-handoff` validate pinned local inputs without opening account or
inbox state, probing a binary, contacting the manager or sending messages. Tests
use synthetic services and local transports; live Signal and host-manager
qualification remain manual.
