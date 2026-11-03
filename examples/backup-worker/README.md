# Backup worker runbook

This implementation targets Linux AMD64 and ARM64 rootless Podman hosts. Live ARM,
storage-failure, provider, and external-monitor qualification remains deferred.
Existing deployment/database commands remain supported. Archive access and isolated
pull validation currently require Linux; a macOS build preserves the existing CLI.

## Prepare artifacts and hosts

`make backup-artifacts` uses the exact Go version in
`packaging/backup-worker/go-version` and produces static helpers plus `SHA256SUMS`
in `dist/backup-worker`. The worker image must contain the exact same helper bytes
as the corresponding host artifact; setup compares their self-test digests.

For a local image build, supply `BACKUP_DEBIAN_IMAGE` as a verified Debian Bookworm
image digest, `BACKUP_DEBIAN_SNAPSHOT` as its immutable package snapshot timestamp
(`YYYYMMDDTHHMMSSZ`), and `BACKUP_IMAGE_NAME` as your local manifest name. Then run
`make backup-images`. Cross-building the image's package installation requires a
native builder or configured emulation for each architecture. The script never
pushes images. Run the repository release gates and the live qualifications before
explicitly publishing your own manifest and using its resulting digest.

Signal uses `packaging/backup-worker/Signal.Containerfile`, with a separately
qualified `SIGNAL_CLI_IMAGE` digest that supplies signal-cli, its JVM/native
libraries, and certificates on the selected architecture. Build with `TARGETARCH`
and the same helper artifacts. The [signal-cli manual](https://github.com/AsamK/signal-cli/blob/master/man/signal-cli.1.adoc)
documents linking and receive behavior; the relay receives periodically, discards
incoming content, and ignores attachments. Receiving never executes commands.

Worker prerequisites: a rootless account with lingering enabled, a working user
systemd manager, cgroup v2, Podman/Quadlet, SSH tools, bubblewrap, and an actual local
ext4 mount at `storage.require_mount`. The archive directory must be empty for
first enrollment. Source hosts need Python 3, rootless Podman/systemd, sqlite3 with
`-nofollow`, `prlimit`, and `timeout`. Use the repository's supported Podman/systemd
versions. No command automatically enables lingering or reformats/mounts a disk.

Install private mode-0600 provider and monitor files at the configured worker-host
paths. Source host keys must already be verified in the operator's known-hosts
file. Setup creates a different Ed25519 key on the worker for every source; it
never copies administrator keys or forwards an agent. Source configuration must
use the standard rootless namespace and an explicit numeric OCI `USER uid:gid`
for non-root images. Database and managed control files must not be group/world
writable; symlinks, hardlinks, unknown mount overrides, and drop-ins are refused.

## Stage, exercise, then activate

Use the supplied `server.yaml` and `deploy.yaml` as configuration examples; replace
hostnames, paths, and secrets with your own values. Redeploy a test application
with `database.backup.enabled: true` to commit its metadata and ownership proof.

```sh
deploy --servers ./server.yaml validate --server
deploy ls --managed vps-main
deploy --servers ./server.yaml --dry-run backup-worker setup \
  --image "$WORKER_IMAGE_DIGEST" --relay-image "$RELAY_IMAGE_DIGEST" home-backup
deploy backup-worker setup --image "$WORKER_IMAGE_DIGEST" \
  --relay-image "$RELAY_IMAGE_DIGEST" home-backup
deploy backup-worker signal-link home-backup
deploy backup-worker run home-backup
deploy backup-worker status --json home-backup
deploy backup-worker logs home-backup
deploy backup-worker list --source vps-main --instance foodlog-prod home-backup
deploy backup-worker pull --backup "$BACKUP_ID" --output ./restore-test home-backup
```

Omit `--relay-image` and `signal-link` for Telegram-only installations. Signal
linking is explicit and retains its account state across upgrades. Start its
staged relay with `systemctl --user start deploy-backup-signal.service` on the
worker before notification tests. `run` starts a manual pass asynchronously.

Signal linking now uses the common account lease inside the pinned relay image;
update older relay images before invoking it. A running relay or surviving CLI
child prevents a second account owner. Interrupted linking may require explicit
account-state review before either relay can restart. See the
[Signal maintenance guide](../notification-worker/SIGNAL_MAINTENANCE.md).
The backup command now stops its enrolled relay and restores its recorded prior
activity. Repeating it resumes interrupted service maintenance without replaying an
uncertain link. Review an uncertain account first, then repeat the command to
restore activity. After completion the default command reports the retained
operation. Use `signal-link --new-link home-backup` for a deliberately fresh link,
or retain an explicit `--operation n-...` for exact automation retries.
Compatible maintenance-protocol version 2 images are required;
the command probes compatibility without account mounts or network before stopping
the service. Shared notification relay installation/handoff remains unfinished.

Check status/logs for completion. Repeating setup with the same inputs resumes an
interrupted installation or verifies owned files without creating another identity.
Dry-run performs local validation without contacting hosts, creating keys, or
sending messages. Command options precede the final worker/server name.

Timers stay disabled until qualification. On the worker, run the host probe once
with the installed helper (`backup-worker __health --local`) before manual heavy
verification. Set a bounded maintenance pause when the next scheduled slot leaves
insufficient time. A pre-activation probe can report unarmed timers; its fresh
observation still permits explicitly requested verification.

```sh
deploy backup-worker pause --until "$MAINTENANCE_END_RFC3339" home-backup
deploy backup-worker verify --backup "$BACKUP_ID" --full home-backup
deploy backup-worker restore-check --source vps-main --instance foodlog-prod \
  --backup "$BACKUP_ID" home-backup
deploy backup-worker notify-test --channel signal home-backup
deploy backup-worker resume home-backup
```

`restore-check` requires a configured `restore_checks` entry. It runs an image
pinned by digest with an argv array, no network, a private writable database copy,
and no production credentials. The image must already be available in the worker
Podman store. Its program reads `DEPLOY_RESTORE_DATABASE`, proves actual application
startup and representative queries, and prints exactly this JSON shape:

```json
{"schema":1,"input_sha256":"<hash before application mutation>","application_started":true,"query_ok":true,"application_version":"<tested version>","database_schema":"<observed schema>"}
```

It must exit nonzero for an absent database and the supplied incompatible SQLite
schema. A no-op, wrong input hash, timeout, or missing image cannot grant a recovery
pin. Structural integrity, foreign-key findings, and application recovery are
separate outcomes. Add entries under `backup_worker`:

```yaml
restore_checks:
  - source: vps-main
    instance: foodlog-prod
    image: YOUR_ACTUAL_CHECKER_IMAGE_WITH_DIGEST
    command: ["/app/check-backup"]
    timeout: 2m
```

Copy `qualification.template.json`, fill every identity/hash/image/architecture
from this installation, and record actual dated evidence for each check. Exercise
power-loss/crash recovery, disk removal, restricted-key denial, a real message,
a missed-worker alert while the Pi is stopped, daily-delivery monitoring, and an
isolated application restore. A provider API acceptance alone is not receipt.
The report must be recent (within seven days) when recorded.

```sh
deploy backup-worker qualify --report ./completed-qualification.json home-backup
deploy backup-worker run home-backup
# After that manual pass completes successfully:
deploy backup-worker activate home-backup
```

Activation requires separate backup and daily-delivery monitor secrets. The
adapters use HTTPS success endpoints and append `/fail` for failures; configure
external grace periods and timezone/calendar expectations accordingly. Confirm
that each real external route alerts independently. A worker cannot detect its
own power loss. Successful hourly backups do not send chat messages. Incidents,
reminders, recovery, and one current daily report use a durable outbox; prolonged
outages are batched without inventing historical delivery. Ambiguous network
failures can produce duplicate delivery. State corruption makes the host probe
signal failure directly through the monitor endpoints.

## Archives, retention, and recovery

List/pull use stored immutable bundles and archive-local leases. They do not
contact the source or trigger a backup. An exact ID can remain downloadable even
when an unrelated bundle is damaged. `--latest` requires an explicit source and
instance plus complete current selection evidence. Source aliases reused for
multiple UUIDs are ambiguous; select the UUID. Pull publishes a new directory
containing `database.db`, the original `receipt.json`, and `download.json` only
after successful transfer, checksum and isolated SQLite validation. It never
overwrites an existing destination. Keep test/prod restores separate; these
commands never import a download into production.

Pull/export refuse a snapshot with a recorded checksum or full-integrity failure,
including when a later quick check passed. Selection never substitutes an older
snapshot for that failure. Explicit `verify --backup ID --full` remains available
to investigate and recheck it. Reconciliation keeps missing catalog entries as
unavailable; the independent health probe also checks the latest bundle on disk,
so an older snapshot cannot silently restore a healthy coverage claim. File-presence
probes do not compete with a full verification's bundle lease.

Publication checkpoints retain the newest capture identity for each generation.
A wholly missing newest bundle or an interrupted publication makes `--latest`
fail; list the archive and select an explicit older ID to recover deliberately.
This applies to detached archive reads too. Older catalogs are reconciled into
this evidence when available; a catalog reconstructed from surviving files cannot
prove the latest legacy capture. Use an exact ID or complete a new capture in that
case.

```sh
deploy backup-worker list --local --archive /mnt/backup/deploy --json
deploy backup-worker pull --local --archive /mnt/backup/deploy \
  --backup "$BACKUP_ID" --output ./offline-restore
deploy backup-worker recover --local --archive /mnt/backup/deploy --scan --json
deploy backup-worker recover --local --archive /mnt/backup/deploy --apply \
  --state-dir /home/backup/.local/state/deploy/backup-worker/state
```

Recovery `--apply` requires a new state directory on writable storage, separate
from the archive. It reconstructs historical catalog/control evidence in read
mode; it does not grant freshness or permission to delete. Exact reads require
neither this catalog nor a working source. A copied archive on another filesystem
is readable, but writer use requires explicit takeover.

For a replacement worker, first disable/fence the old writer and revoke obsolete
source authorizations. Reconstruct state at the path above, preserve the archive
control/pin checkpoint, then use `setup --recover --fenced` with the new host's
policy and matching image/artifacts. The takeover drains archive leases and
advances the writer epoch. Repeat interrupted setup with identical arguments.
Missing control, conflicting identities, or foreign incoming files require
investigation; preserve them instead of guessing ownership. An old writer must
never keep writing an independent copied disk after takeover.

```sh
deploy backup-worker reconcile home-backup
deploy backup-worker prune --list home-backup
deploy backup-worker prune --apply --source "$SOURCE_UUID" \
  --instance foodlog-prod home-backup
```

Absent retention disables deletion. A policy keeps snapshots younger than its
age limit **or** among its newest `keep_last`, plus full-integrity/application
recovery pins. Unknown, retired, and historical-generation scopes remain protected.
Use `pin --backup ID --pin-label manual/NAME --reason TEXT WORKER` for an
operator hold. Release an exact label with `unpin --backup ID --pin-label LABEL
--reason TEXT WORKER`; status JSON exposes verification labels. Removing a
verification pin is an explicit operator decision and is recorded as an intent.
Known checksum or integrity failures in either local state or archive verification
history stop pruning in the selected scope, even when the stored checksum still
matches. Clock uncertainty also blocks deletion until corrected and acknowledged;
read-only previews and explicit diagnostic checks remain available. Incomplete
ownership/control, live leases, and unresolved work stop pruning as well.
Reconciliation finishes owned interrupted trash cleanup and abandons
incomplete unpublished transfers; it never marks an old run successful. Shared
leases protect long exports and restore drills without writable lease records.
Maintenance runs bounded checks and retention when a 25-minute slot is available;
large/slow deployments may require an explicit pause to complete drills.

## Lifecycle and operational changes

- `update --image ... [--relay-image ...] WORKER` stages new policy/images, checks
  generated units, preserves data, and uses recorded before-images for rollback.
  Source SSH host keys are qualified from private staging files. Live trust files
  are published only during protected installation, with before-images that
  restore the previous pins on failure or interruption.
  Previously enabled timers are disabled during staging. Before that happens, an
  independent `deploy-backup-recovery.timer` is enabled with an immutable helper
  copy and durable rollback journal. After a killed coordinator or boot, it
  restores the previous helper, configuration, policy, and recorded timer activity.
  A state-access gate and shared/exclusive schema lease exclude ordinary processes
  during installation. Recovery remains pending until timer restoration succeeds.
  A successfully staged update leaves normal timers disabled for qualification.
  Signal starts through notifier dependencies; generated Quadlet services are
  never passed to `systemctl enable`.
  It requires renewed qualification/activation for the changed configuration.
  Runtime state schema changes are refused rather than silently downgraded.
- `rotate-key --source ALIAS WORKER` journals a replacement private key on the
  worker, rotates only its owned source entry, tests actual SSH restrictions, and
  resumes the prior backup timer. Retry the same command after interruption.
- `uninstall WORKER` stops owned services, revokes owned entries on every configured
  source, and removes unchanged owned configuration/units. Archives, state,
  revoked key material, and the archive-reading helper are retained. It never
  removes an administrator key or a helper still used by another worker.
- `retire --source UUID [--instance SERVICE] --reason TEXT WORKER` persists a
  retirement exclusion and closes scoped incidents as retired-by-operator,
  without claiming successful backup recovery. Pending obsolete alerts are
  superseded; unrelated alerts in mixed batches remain deliverable. Retry if
  notification delivery currently holds the notifier lock. Rediscovery cannot
  undo retirement. `reactivate` explicitly
  removes that exclusion, requires a UUID/reason, and resets freshness evidence.
- A binding/database replacement requires `source-reconcile --source-id UUID
  --instance SERVICE --previous GENERATION --reason TEXT SOURCE_SERVER`, followed
  by `accept-generation --source ALIAS --instance SERVICE --generation NEW_ID
  --reason TEXT WORKER`. Historical generation data and pins remain protected.
- A rebuilt source gets its own identity. Do not copy an old UUID onto a different
  machine to hide the change; preserve/retire the old coverage explicitly.
- After an actual clock correction, inspect the clock and use `ack-clock --reason
  TEXT WORKER`. UTC/monotonic divergence remains visible until acknowledged.
- Rotate provider files atomically at their configured paths and relink Signal
  explicitly when required. Repeat message/monitor qualification after changing
  provider accounts or external check identities.

Source helpers cap one snapshot at 10 GiB, snapshot creation at five minutes,
and a session at seventeen minutes. Each worker/source pair has an hourly budget
of 1,024 requests, 1,800 reserved capture seconds, and 60 GiB of exports. In-flight
captures retain export and deployment locks independently of the SSH client and
Go supervisors. Native watchdogs and SQLite inherit those descriptors through
Podman unshare, retaining the locks until database work exits. Streaming does
not retain them. Ordinary opens never invent missing source generations or state.

A pause remains unhealthy for coverage reporting, including after its deadline
until host admission. `resume` and expiry atomically create a catch-up request;
the host retries failed starts and clears the request only after a complete run.
Clock uncertainty prevents a paused worker from resuming until acknowledged.

Later checksum/integrity findings are separate from immutable receipt validation.
`list` exposes later verification evidence; current health and alerts consult it,
and recovery/reconciliation import it from the archive. An execution error or
quick check cannot clear a known full-integrity failure. Backup and daily-delivery
monitoring must address different HTTPS checks, even when separate secret files
are configured. Delivery recovery uses its own catch-up event, which daily report
coalescing cannot consume.

Run `make test-backup-worker` for focused tests and `make test` for regression,
race, and vet checks. `make test-backup-worker-isolated` uses a separate temporary
Podman store and disposable rootless systemd account to test SIGKILL update
recovery, including SSH trust restoration, with the production helper, plus its
isolated SQLite validator. Real SSH connections also prove that rejecting a
staged trust candidate leaves the live connection usable. The harness also runs
uninstall after a completed rotation journal through the production helper and
real systemd, including idempotent retries and archive preservation. It also kills
capture supervisors across Podman unshare, verifies that native SQLite work keeps
the deployment locks, and waits for the native watchdog to release them. A normal snapshot also proves
that the locks can be released before streaming. Source
revocation acknowledgements in that lifecycle fixture are supplied locally. It does
not test full source capture/provider delivery or VM reboot/power-loss behavior.
`make release-check` also requires the existing disposable
Podman and VM crash harnesses. These commands do not replace the live backup-worker
qualification described above. Keep previous backup arrangements through the
acceptance window; image publication and production rollout are separate actions.
