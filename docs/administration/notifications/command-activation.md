# Managed command admission

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

Initialized Telegram and supported first-recovery Telegram/Signal installations
may enable commands separately from report delivery. Signal requires the admitted
[native production service](signal-production.md). Configure the interaction routes, identities, producers and command
descriptors before setup, then complete the applicable qualification checks and
[activate delivery](delivery-activation.md). Delivery starts with commands off.
Qualified LED output is admitted with delivery. First recovery preserves existing
command history through an explicit drain-only adoption. Keep the original source
database and complete independent history: an older snapshot cannot replace a later
disable. Upgrade both runtime and helper before preparation; unsupported binaries
are refused before preparation changes the installation. Fresh qualification and
explicit enable are required in the new epoch.

Read `manifest_hash` from `installation-status` and `record_hash` from
`qualification-status`. Use the qualification record consumed by the current
delivery activation. To admit read commands:

```sh
deploy notification-worker enable-commands --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash CURRENT_MANIFEST_SHA256 \
  --qualification-hash RECORD_SHA256 --mode read
```

Use `--mode all` only after qualifying the configured mutation descriptors,
confirmation/replay behavior and authorization expiry. Existing command contracts
still govern every request and effect. Switching modes requires disabling first.
Both runtime and retained helper must advertise managed command support.
Prefix the command with `deploy --dry-run` in place of `deploy` to validate local
pins without service or authority changes.

To stop admission, read the current manifest hash again:

```sh
deploy notification-worker disable-commands --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash CURRENT_MANIFEST_SHA256
```

Disable publishes `drain` mode, waits for unexpired execution grants and restarts
report delivery with only the reply loop. New intake, conversions and execution
grants are refused. Already accepted jobs may finish and their durable results
remain available; disabling does not claim to cancel producer-side work.
Healthy qualified report delivery and admitted replies continue. Missing credentials
or failed qualification can refuse preparation before any disable is published.
If they become unavailable after intent publication, the disable remains durable
and `resume` retries it. Use delivery deactivation when credentials are unavailable.

Admission binds the installation, activation and qualification. Independent
worker SQL and file history preserve disable decisions across restart. Keep all
installation and worker evidence; replacing an older manifest or deleting a
history record does not provide a supported way to restore admission.

`resume --local --root ROOT` finishes an interrupted command operation. If its
qualification/credentials are unavailable, `deactivate-delivery` can supersede it
using the **desired** manifest hash reported by installation status. Revoke active
producer generations first. Revocation is allowed during a pending command
operation and leaves the runtime stopped for resume/deactivation. Deactivation
drains grants, clears command admission and retains command history; a later
qualified delivery activation starts with commands off.

The automated fixtures use synthetic Telegram replies and credentials. They do
not establish actual messenger receipt or host qualification. Record those manual
observations separately using [qualification](qualification.md).

## Revoke authorization or narrow subjects

First disable commands as above and wait for that operation to complete. Create a
private JSON file containing the **complete retained permission array**, using the
same fields as `worker.interaction.permissions`. Remove an actor's entry to revoke
that actor; reduce `permissions` or replace `scope.whole_worker` with an explicit
`scope.subjects` list to narrow access. Keep the other actors' retained entries.
An explicit `[]` revokes everyone. The file is limited to 16 KiB; `null`, new actors,
new permissions, broader subjects and duplicate actor/conversation/worker entries
are refused. For example, a retained entry has this shape (use your actual IDs):

```json
[
  {
    "actor": "789",
    "conversation": "private",
    "worker": "YOUR_PRODUCER_ID",
    "permissions": ["read"],
    "scope": {"subjects": ["a"]}
  }
]
```

Read the new manifest hash after disable, then apply the file:

```sh
deploy notification-worker narrow-commands --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash CURRENT_MANIFEST_SHA256 \
  --permissions-file /home/notify/retained-permissions.json
```

Both the installed runtime and helper must support the policy capability. The
operation leaves commands in drain mode and report delivery active. Re-enable
read/all commands explicitly using the resulting manifest and active qualification
hash. Dry-run validates local pins and the retained file history without opening
the live database, stopping the service or changing authority.

Narrowing creates a new policy revision. Previously frozen replies, preparation
previews, confirmations and pagination become invalid, including old commands for
otherwise unchanged actors. Request fresh queries after re-enabling. Already
admitted jobs retain their original identity and results for local reconciliation;
no private result is sent under the old policy. Revocation takes effect at the
committed narrowing, not merely when the permission file is edited.

Each later narrowing must be a subset of the retained policy. Disable, re-enable,
delivery deactivation and supported first recovery preserve it. Lost history or
an older installation/database cannot restore rights. Resume an interrupted
operation using `resume`; credential loss can instead use the documented
`deactivate-delivery` path. Permission expansion and enrollment/configuration
changes still require a future managed migration workflow. Do not edit canonical
configuration or remove history to bypass this restriction.

## Same-account Telegram credential rotation

Deactivate delivery using the current manifest before replacing the token file.
If the file already changed, deactivation remains available even though the old
qualification cannot authorize provider requests. Retain the bot account ID,
conversation IDs, generations and threads. Record fresh qualification, activate
delivery, then explicitly enable commands again. Retained requests and replies
keep their original destinations and IDs. The old qualification cannot admit the
new credential. A different account or conversation is a separate configuration
migration and must not be substituted through credential rotation.

## Recovered Telegram intake

First complete [history import, drain, request sealing and cursor preparation](telegram-recovery-intake.md)
in the quarantine, including available specialist command reconciliation before
sealing the request inventory.

After recovery publication/adoption, use `renew-telegram` to record a fresh drain,
then qualify the current verification installation and activate outbound delivery.
Delivery activation keeps commands off. Explicit `enable-commands` consumes the
exact latest renewal under the worker lease before admitting commands. Allow the
five-second cutoff settling interval and enable within the ten-minute handoff
window. The same native boot/monotonic clock must still match the observation.
An expired unconsumed drain requires deactivation and another renewal, followed
by qualification for the changed manifest.

Consumption records independent intent, SQL and completion anchors. The original
reconciliation cursor remains intact; intake and restored requests use the newer
cutoff. Polling offset advancement and restart require the retained handoff.
Once its SQL decision is committed, that cutoff remains fixed for the recovered
epoch. Disable/deactivate/re-enable retain it; do not try to renew it or remove its
records. Existing authorization and command-age checks still apply.

If command enable is interrupted, resume it or deactivate using its desired
manifest hash. Cancellation archives an attempt that never committed its SQL
handoff decision; a subsequent new drain is then possible. If SQL already committed,
cancellation completes the exact handoff and preserves its cutoff while returning
to verification. Neither path starts command intake. Capability checks require
both runtime and retained helper to support this handoff protocol.
