# Managed delivery activation

`activate-delivery` enables the managed dispatcher for an initialized worker with
Telegram profiles, including first recovery of outbound-only installations.
It consumes the exact latest acknowledged qualification record. Initialized
Telegram interaction configuration is supported, but intake/execution requires
separate [command admission](COMMAND_ACTIVATION.md). Recovered Signal uses the
[native production service and explicit profile enrollment](SIGNAL_PRODUCTION.md).
Qualified LED bindings are also supported; every daemon request checks current
managed admission and unhealthy or inherited-fault evidence still blocks healthy output.
An inherited worker aggregate fault has a separate
[explicit runtime reconciliation procedure](LED_RECOVERY_RUNTIME.md). Recovered interactive
Telegram requires a retained renewal and separate command handoff admission. Paths for the managed
root, state and unit directory must have no spaces or tabs.

Producer generations remain inactive. This transition alone sends no reports;
each producer still needs its separate explicit generation admission/cutover.
The initialized Telegram-only backup profile now supports [managed per-producer
cutover and report-service migration](BACKUP_CUTOVER.md). Recovered Telegram
backup reporting uses [explicit recovery activation](BACKUP_RECOVERY_ACTIVATION.md).
Legacy backup Signal uses [explicit account adoption](LEGACY_SIGNAL_ADOPTION.md)
followed by the Signal cutover procedure. Explicit restricted legacy restoration
and repeated migration cycles are documented in the cutover runbook. Fixture test results are
not live provider or host qualification evidence.

## Activate

Complete the [qualification procedure](QUALIFICATION.md), retaining actual
observations for every applicable check. Inspect both current pins:

```sh
deploy notification-worker installation-status --local \
  --root /home/notify/.local/share/deploy/notification-installation
deploy notification-worker qualification-status --local \
  --root /home/notify/.local/share/deploy/notification-installation
```

Copy `manifest_hash` from installation status and `record_hash` from qualification
status. The latter hashes the acknowledged record, including its report; it is
not the report ID or a digest of the exported JSON text. An unfinished operation
must normally be resumed first. After review, substitute the exact lowercase
SHA-256 values below:

```sh
deploy --dry-run notification-worker activate-delivery --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash MANIFEST_SHA256 --qualification-hash RECORD_SHA256
deploy notification-worker activate-delivery --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash MANIFEST_SHA256 --qualification-hash RECORD_SHA256
```

Dry-run validates local artifacts, pins and qualification without stopping the
service or enrolling state. Manager inspection, executable capability probes and
state/runtime checks occur only on apply. Apply requires both the retained runtime
and recovery helper to advertise delivery support before stopping the service.
An older retained helper must first be upgraded through the explicit helper
transition.

Qualification must be complete, current (within 30 days), and bound to this exact
manifest, UID, architecture, configuration and credential contents. Missing or
changed credentials, clock rollback, a pending acknowledgement or a newer failed
report block admission. The CLI makes no live provider calls while checking these
inputs. Recording qualification evidence or saving an `activation-review` never
grants authority by itself.

Apply journals stop, exclusive installation/worker leases, permanent ownership
enrollment, unit/manifest publication, reload, restart and health verification.
The delivery unit allows Unix and IPv4/IPv6 sockets. The installer checks the
loaded manager profile, command, native process and readiness socket peer. Health
must identify the exact activation hash and a current `delivery-admission` loop.
The runtime checks retained authority before startup, every second and immediately
before each send. Loss of current qualification, credentials, configuration, unit,
ownership or coordination proofs terminates it; admission failure makes no
provider call and claims no acceptance.

## Deactivate and resume

Read the current `manifest_hash` again, then:

```sh
deploy notification-worker deactivate-delivery --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash CURRENT_MANIFEST_SHA256
deploy notification-worker resume --local \
  --root /home/notify/.local/share/deploy/notification-installation
```

Deactivation restores the verification runtime and Unix-only network profile.
It needs the retained structural installation proofs, but does not require live
credentials or unexpired qualification. Deactivate before update, rollback,
uninstall or collecting/importing another qualification report. Qualification
status during delivery evaluates the original activation binding.

Exact retries reuse the frozen intent and qualification. `resume` rechecks current
admission before completing activation. If admission has expired or disappeared
after an interrupted activation, use `deactivate-delivery` with the desired
manifest hash exposed by installation status. It explicitly supersedes that
unfinished activation and returns to verification without granting delivery.

Ownership is independently retained in `managed-delivery-owner.json` in worker
state and a SQL metadata anchor. It survives deactivation, compatible binary
updates/rollback and uninstall. Unmanaged worker startup refuses this state even
if the ownership file is lost; loss of either sealed anchor also blocks managed
startup. Resume does not recreate a lost anchor after enrollment completed.
Removing the service therefore does not authorize a foreground ownership bypass.

The first enrollment changes the verification manifest after deactivation, so
collect matching qualification before reactivation. Later revisions or changed
credentials also need new evidence. Supported updates/rollback retain the owner
and require delivery-capable runtime/helper binaries. Configuration migrations
for enrolled delivery state require future explicit transitions.

## First recovery of enrolled outbound Telegram

Keep the installation, original worker directory and their independent ownership
records. Do not remove enrollment files to use unmanaged recovery. Before a
planned recovery, revoke producer generations and deactivate delivery. An offline
snapshot can be created after stopping the managed service:

```sh
deploy notification-worker managed-snapshot --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash MANIFEST_SHA256 --to /home/notify/snapshots/NEW_DIRECTORY
```

The command requires the service already stopped; it never starts it, sends
messages, or overwrites an existing snapshot. Record the returned snapshot ID
and database hash. Snapshot verification remains available through
`snapshot-verify`. A snapshot taken before delivery enrollment is supported when
the original state still proves enrollment independently.

For recovery preparation, use the current **verification** manifest hash:

```sh
deploy notification-worker recovery-prepare --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash MANIFEST_SHA256 --from /home/notify/snapshots/SNAPSHOT \
  --snapshot-id SNAPSHOT_ID --database-hash DATABASE_SHA256
```

Preparation validates the exact snapshot, records immutable intent, stops the
verification service, and retains installation/worker coordination while entering
quarantine. Retry the same command after interruption. Pending preparation
prevents update or delivery reactivation. No producer is admitted by preparation.

Continue the existing offline producer reconciliation, ownership fencing,
checkpoint, publication verification and `recovery-publish` procedure. An absent
producer may remain fenced, but cannot stage or activate delivery until reconciled.
Then run `adopt-recovery` with the publication commit pinned in the installation
spec. Both candidate runtime and helper must support recovered delivery ownership.
Adoption links the retained original owner to the new epoch without rewriting the
original owner file or hash and starts verification only.

Collect qualification for the new manifest before `activate-delivery`; the old
qualification cannot authorize the recovered epoch. Delivery activation leaves
producer generations inactive. Deactivation and compatible binary updates retain
the lineage; rollback cannot cross recovery. Missing independent owner, lineage,
publication or adoption anchors block startup/admission, rather than reconstructing
committed authority. The quarantine and original recovery evidence stay retained.

First enrollment of an originally unowned recovered outbound Telegram installation
is also supported at explicit qualified activation: both original file and SQL
evidence must prove the absence of prior enrollment. Adoption alone does not enroll
delivery. Recovered interactive Telegram uses the separate
[command handoff](COMMAND_ACTIVATION.md#recovered-telegram-intake). Prior managed command history is preserved by drain-only adoption and requires
fresh explicit admission.

For a subsequent epoch, take a recovered managed snapshot including its independent
proof archive. Revoke producer generations, disable commands and deactivate delivery,
then repeat `recovery-prepare` with the current verification manifest. Runtime and
helper must advertise `__managed-next-recovery-probe`. The coordinator retains the
original managed owner, prior publication and current runtime audit history before
rotating the epoch. Complete all source/provider reconciliation and publish/adopt the
new recovery, then qualify and activate again. Telegram handoffs and renewals are
scoped to the new epoch. For Signal use the subsequent-recovery procedure in
[SIGNAL_VERIFICATION.md](SIGNAL_VERIFICATION.md); backup reporting uses
[BACKUP_RECOVERY_ACTIVATION.md](BACKUP_RECOVERY_ACTIVATION.md#subsequent-gateway-recovery).
Missing prior proof is a refusal, never permission to regenerate history.

Actual user-systemd/rootless operation, native ARM64, reboot/power-loss and real
provider receipt still require operator qualification on the selected host and
destinations. The native development fixture uses actual executable/socket peers
with a simulated manager and synthetic credentials; it contacts no provider.
