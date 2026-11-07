# Review an existing worker as YAML

Examples and supporting files: [repository examples](../../examples/workers).
Run repository-relative commands from the repository root unless stated otherwise.

Import reads verified installation records and prints a candidate inventory. It
does not save YAML, adopt an installation, start services, enroll sources, link
accounts or send messages.

```sh
deploy notification-worker import manager > server.import-candidate.yaml
deploy backup-worker import manager > server.backup-candidate.yaml
```

Use a separate output file, review the changes, then merge the wanted policy into
`server.yaml`. `--json` returns the candidate and required review actions in one
versioned result. `--dry-run` contacts no target. Unattended import needs no
credential input or prompts. Concurrent inventory edits invalidate the review.

The inventory must contain the server's SSH connection. A missing worker block
selects default lookup paths only. For an older custom layout, declare its
`paths.installation` (notification) or restore its original path pins (backup).
A static helper bundle is needed for target inspection. Backup source aliases
must exist in the inventory.

Notification review supports completed managed installations with
direct or interactive Telegram, or an owned fresh direct-alert Signal service. It reconstructs routes, producer
policy, limits, LED bindings, Telegram command aliases/permissions and private-file references. Actual state,
installation and unit paths are retained. Identities, epochs, route revisions,
provider fingerprints and credentials stay in runtime records. Config/data/runtime
input paths are coordinator locations selected by the inventory, not instructions
to relocate installed state. Existing LED daemon ownership and artifact selection
remain explicit review items.

Backup review supports declarative setups and completed legacy standalone setups
with verified helper, source-key, runtime, database and archive ownership. Storage,
mount requirements, retention, schedules, notification references and image pins
are preserved. Legacy mount storage does not become home storage under version 3. Review checks
source enrollment receipts against the actual private keys and managed trust-file
paths, including older installations without the newer key-reservation map.

Backup review requires an idle, settled database. It takes the existing schema
lease exclusively and refuses uncheckpointed WAL data, so inspection creates no
SQLite sidecars in the installation. Repeat after current worker activity finishes.
Retained WAL data requires the normal owned-runtime recovery/settling workflow;
import never discards it.

Unknown files, lost ownership, unfinished transitions and drift produce errors.
Legacy/interactive Signal handoff, unrepresentable Signal
inbox layouts and legacy shared-backup ownership need their specific review
workflows. These cases produce no partial candidate. Normalization may make
defaults explicit and replace comments inside the selected worker block;
unrelated inventory content is retained. Interactive candidates include the declared
conversation addresses and actor/permission scopes; they omit generations, command
implementation descriptors and admission receipts. Import preserves existing
delivery/command admission and grants no new rights.

After reviewing and saving a candidate for an unbound direct Telegram or owned
native Signal gateway,
explicitly adopt its existing managed installation:

```sh
deploy notification-worker adopt manager
deploy notification-worker status manager
```

Adoption requires the exact installed helper, state and unit paths, target-private
credential references and equivalent runtime policy. It preserves route labels,
original runtime bytes, gateway identity, credentials and the running process.
Existing-daemon LED output can be retained; this does not take over a managed host
daemon. A pinned private staging record is published atomically as coordinator
ownership. Lost replies and process death resume with `adopt` or `resume` using the
same YAML. Ordinary `apply` never begins adoption of an unmanaged installation.

Adoption does not change policy, enroll producers or grant qualification. Finish
it before editing policy. Later supported limits/routes/activity/helper updates
use normal `apply`. Existing producer, command, shared backup or legacy Signal ownership
requires its separate adoption/handoff workflow, which remains unfinished in the
short interface. Missing journals in an existing coordinator root require recovery;
`adopt` never repairs them or initializes replacement state. Do not delete ownership
records to bypass that check.

For an installation already managed by the declarative coordinator, use `plan`
after merging the candidate and apply only supported changes. Repeated `adopt`
is a no-op while the adoption is the current completed operation; after a later
lifecycle transition use `apply` or `resume`.

For native Signal, adoption requires a completed owned fresh-service manifest,
the original native account reservation/link and inbox records, and a gateway
that already references that service. Keep the imported target account/recipient
files and native executable reference. The gateway and native service must use
the selected helper digest; adoption cannot upgrade either binary. The existing
inbox must use the representable sibling layout described by import.

Adoption publishes the gateway and native child coordinator records together. It
retains the existing account, link attempt, inbox identity, service process and
native unit. Subsequent explicit `resume` can restart an intact stopped native
service using its existing lifecycle; it never repeats linking. A bare account
directory, legacy backup relay, interrupted account transfer, different helper,
missing journal or unrepresentable inbox needs its separate recovery/handoff
workflow. This is coordinator adoption of proven ownership, not permission to
claim an arbitrary existing account.

## Adopt a legacy standalone backup

After saving the reviewed policy, use:

```sh
deploy backup-worker adopt manager
deploy backup-worker status manager
```

This supports completed legacy standalone setups at their retained default layout,
with notifications disabled or using direct Telegram. The installed helper and image,
archive/writer identity, source aliases/endpoints, source keys, known-host entries,
and runtime policy must match the reviewed selection. The command preflights source
connections and selected artifacts but never invokes source enrollment. Shared
reporting, Signal ownership and existing custom-layout takeover remain separate
workflows.

The takeover retains its predecessor and exact request, then atomically adds the
YAML declaration and source-key/trust pins to the existing setup journal. It does
not rewrite runtime files, touch the database/archive, replace keys, restart services
or enable timers. Repeat `adopt`, or use `resume` with the same YAML after an
interruption. Missing adoption history is a recovery error, not a new takeover.

After adoption, ordinary supported schedule/retention/budget edits use `apply`.
Existing service activity is retained; adoption supplies no new qualification or
activation claim. Read `status` for the remaining checks. Keep the original
mount/storage requirements when reviewing a legacy inventory.
