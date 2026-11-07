# Retiring and replacing an installation

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

Use this procedure to change the operating UID, API/LED socket location, general
runtime limits, provider account/audience enrollment, or to start a new installation
after reaching a bounded history limit. It deliberately ends the old producer
lifetimes. The old installation and state remain at their original paths for audit;
the replacement has new worker and producer identities, empty queues and fresh
qualification. This is suitable for a planned outage after all obligations settle.

For continuity of an existing account or producer, use the existing
[command enrollment migration](command-migration.md),
[LED binding migration](installation.md#managed-led-binding-migration),
[repeated recovery](delivery-activation.md),
[Signal service replacement](signal-production.md), or
[backup migration cycles](backup-cutover.md). Retirement never transfers pending
commands, delivery acceptance, conversation history, fault-clear authority or
provider cursors into a fresh identity. A fresh interactive account must also be a
fresh native provider account, not just a changed local account label. Retaining a
native account requires its recovery/cursor handoff procedure instead.

## Settle and stop the old installation

1. Stop new domain work and retire its coverage explicitly through each producer.
   For backup workers, use the documented `backup-worker retire --source UUID
   --reason TEXT WORKER` for **every configured source** and allow the final report
   pass to publish. Once every configured source is explicitly retired, no active
   instances remain and no incidents are open, the aggregate `backup` subject also
   becomes retired. An empty discovery or retiring only the known instances does
   not retire the aggregate. Keep archive/storage and source-uninstall evidence.
2. Resolve or explicitly decide pending/ambiguous deliveries using
   [administration](administration.md), and let producers import and acknowledge
   all receipts. Finish commands and acknowledge results and replies. Handle any
   pending provider inputs before disabling commands. Explicit cancellation is
   unsuccessful delivery; it never satisfies a delivery monitor.
3. Let the normal evidence-checked LED path finish fault cleanup, including any
   [recovered faults](led-recovery-runtime.md). Retiring an installation cannot
   clear fault IDs. Confirm there are no outstanding LED rows or unknown states
   in its namespaces; a daemon/state mismatch requires reconciliation first.
4. Revoke every producer generation, including staged generations, through its
   existing ownership protocol. For backup reporting, use the exact cutover,
   remigration or recovery-activation revocation operation that owns the current
   generation. Stop producer report/transfer triggers after the last acknowledgement.
5. Disable commands, deactivate delivery, and stop the worker. For every producer,
   preview and then apply permanent retirement with the exact canonical runtime:

   ```sh
   deploy notification-worker producer-retire --local \
     --config /home/oldnotify/runtime.json --state-dir /home/oldnotify/state \
     --producer backups --reason 'Coverage decommissioned for replacement'
   # After checking eligibility, repeat with --apply.
   ```

   Every subject must already be retired; every generation must be revoked.
   Retirement does not acknowledge receipts or waive outstanding obligations.
6. Uninstall the gateway through `notification-worker uninstall --local --root
   /home/oldnotify/installation`. Uninstall its separately managed Signal service,
   if present, with `signal-service-uninstall --local --root SERVICE_ROOT`.
   Retain all account ownership and endpoint records. Uninstall old backup/source
   installations using their own lifecycle if those producer identities are ending.
   Archive data is retained. Do not reactivate an old producer after its epoch has
   been permanently retired in the gateway.

Normal installation history remains bounded at 32 operations/releases. There are
three additional operation slots reserved for command disable, delivery deactivate
and uninstall so a full history cannot prevent this shutdown. They create no new
release. Receipt compaction remains available for its narrower purpose; historical
operation, administrative and recovery evidence is never silently pruned.

## Review the replacement and publish terminal evidence

Stage a private proposed runtime with fresh producer epochs supplied by newly
initialized specialist installations. Use the new UID's intended absolute secret
and socket paths. Provider account or audience changes require fresh enrollment
and qualification. Do not edit the old pinned runtime or copy its database.

Run as the **old UID**, using a current executable:

```sh
umask 077
deploy notification-worker replacement-target --local \
  --config /home/oldnotify/proposed-runtime.json \
  --root /home/newnotify/installation --state-dir /home/newnotify/state \
  --uid 1002 --reason 'Move service ownership and start a new history' \
  > /home/oldnotify/replacement-target.json

deploy notification-worker installation-status --local \
  --root /home/oldnotify/installation
# Copy the exact returned manifest_hash from the completed uninstalled manifest.
deploy --dry-run notification-worker installation-retire --local \
  --root /home/oldnotify/installation --manifest-hash MANIFEST_HASH \
  --target /home/oldnotify/replacement-target.json

deploy notification-worker installation-retire --local \
  --root /home/oldnotify/installation --manifest-hash MANIFEST_HASH \
  --target /home/oldnotify/replacement-target.json \
  > /home/oldnotify/retirement-result.json
```

`replacement-target` preserves raw route labels and validates their resolved
runtime; repeated decoding does not change the pinned route revision. The target
binds the exact UID, new root, state, configuration and reason. Target and certificate
inputs must be owned, single-link regular `0600` files at absolute paths.

`installation-retire` requires the exact completed uninstall and verifies every
producer retirement, all settled inputs/replies, original UID and LED layout. It
publishes a terminal installation fence, then clears only exact reserved baseline
and bounded healthy-lease IDs owned by the old UID at the old LED socket. It never
clears a fault or another application's state. It confirms absence before publishing
completion. Worker evidence is retained independently in SQL and private files.
Old runtime LED publication and managed updates are fenced after retirement starts.

The result contains `certificate` and its canonical `hash`. The standalone
certificate is also retained as `/home/oldnotify/installation/retirement.json`.
Keep the printed hash separately when transferring that file. It is an explicit
operator pin, not a public signature or proof from an untrusted sender.

A failure or process death is resumed by repeating the **same** command and target.
Lost clear responses are reconciled against the original daemon registry. Changed
UID, socket, target, manifest or independent history is refused. Do not delete an
intent or completion record to retry. The original UID must remain available until
cleanup completes; changing ownership of its files is not an ownership migration.

## Initialize under the new UID

An administrator creates the new user's private state directory and non-writable
parent directories and transfers a copy of the certificate, owned by the new UID
and mode `0600`. Keep the old directories owned by the old UID; retain their full
contents, including SQLite sidecars, ownership journals, helpers, installation
operations, request/receipt histories and recovered endpoint/account evidence.
Maintain an offline copy according to the host's archive procedure before removing
an old user account. No automated garbage collection removes these records.

As the **new UID**:

```sh
deploy --dry-run notification-worker replacement-init --local \
  --certificate /home/newnotify/retirement.json --certificate-hash CERTIFICATE_HASH
deploy notification-worker replacement-init --local \
  --certificate /home/newnotify/retirement.json --certificate-hash CERTIFICATE_HASH
```

The target state directory must initially be empty. The command pins the certificate
before initializing state and retains the resulting new identity independently.
Repeating it resumes/reuses the same identity. Missing identity publication can be
completed only while the new state has no work or generation ownership. Corrupt or
partially initialized SQLite state is refused, never replaced with an empty database.

Write the exact reviewed `target.runtime` to a private runtime input, then run the
normal `setup --local --spec NEW_INSTALLATION_SPEC`. Setup checks the target UID,
root, state, configuration and new identity before publishing a service. The API
socket is the new state's `api.sock`; the reviewed LED socket may also change.
The service starts in verification mode. Enroll fresh specialist producer epochs,
collect qualification, activate delivery, and separately enable commands and
producer generations. Fresh observations must establish health; no old receipt,
command result or LED acknowledgement counts as new evidence.

Automated tests use disposable installations and simulated managers/LEDs. They cover
interrupted and SIGKILL retirement/replacement, foreign LED preservation, missing
proof refusal, UID/path/configuration pins, full-history shutdown, and the actual
native replacement CLI. Real cross-UID host administration, systemd, provider and
hardware qualification remains a manual gate.
