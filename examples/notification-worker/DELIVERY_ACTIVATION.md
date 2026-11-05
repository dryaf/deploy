# Managed delivery activation

`activate-delivery` enables the managed dispatcher for an initialized worker with
fixed-recipient, outbound-only Telegram profiles. It consumes the exact latest
acknowledged qualification record. It supports neither recovered installations
nor Signal, Telegram intake, interaction/commands or LEDs. Paths for the managed
root, state and unit directory must have no spaces or tabs.

Producer generations remain inactive. This transition alone sends no reports;
each producer still needs its separate explicit generation admission/cutover.
The initialized Telegram-only backup profile now supports [managed per-producer
cutover and report-service migration](BACKUP_CUTOVER.md). Signal/recovered
producer handoffs and automatic restoration of legacy delivery remain unfinished. Fixture test results are
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

Actual user-systemd/rootless operation, native ARM64, reboot/power-loss and real
provider receipt still require operator qualification on the selected host and
destinations. The native development fixture uses actual executable/socket peers
with a simulated manager and synthetic credentials; it contacts no provider.
