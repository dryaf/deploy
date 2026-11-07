# Raspberry Pi status LEDs

Examples and supporting files: [repository examples](../../examples/workers).
Run repository-relative commands from the repository root unless stated otherwise.

Declare the ordinary managed setup in `server.yaml`:

```yaml
notification_worker:
  providers: [telegram]
  status_leds:
    enabled: true
```

Run `deploy notification-worker manager`. The command discovers an actual ACT
LED, selects the pinned daemon archive from the worker bundle and stages a private
host prerequisite request. If the board exposes only a generic name such as
`led0`, select that observed device with `status_leds.daemon.device: led0`; an
ambiguous device is never guessed. The ordinary worker remains a user service.

The LED daemon needs an administrator-owned system service because the pinned
hardware driver uses system-wide device locks. Setup prints the exact changes,
a review file and an administrator command to run **on the target**. Review the
request before executing the command. It installs only the selected daemon,
configuration, private state and named socket/service; it does not invoke a
package manager or change another daemon's files. Python 3.11 or later at
`/usr/bin/python3` and systemd are host prerequisites. No implicit `sudo` runs.

The fixed managed socket is `/run/deploy-led-host-UID.sock`, accessible only to
that target UID; its peer is the root daemon. Files live beneath
`/var/lib/deploy-led-host/UID` and the two `deploy-led-host-UID` system units. The
service permits writes to its private state and the selected sysfs devices
(directory access covers dynamically created timer attributes). It has no Linux
capabilities and starts with output `off`. By default only ACT is selected.

For an initial installation, explicitly opt into power-LED control with:

```yaml
status_leds:
  enabled: true
  power_led: true
```

This stages a versioned host request for both ACT and PWR. The target must expose
one unambiguous `PWR`/`power` device (also accepted as a colon-delimited name
suffix), separate from ACT. The administrator review names both actual sysfs
directories and the power override. Ambiguous or generically named power devices
require an explicitly configured existing daemon. Adding or removing power control
on an already installed managed daemon still requires a migration.

After the administrator step, rerun the same short command. Interrupted host
installation resumes its retained journal; changed files, missing completion
receipts and a newly selected artifact digest require review. Completed retries
do not restart services. `status manager` distinguishes a pending host prerequisite
from a completed prerequisite and an installed notification worker. Backup setup
also displays this prerequisite when handling its local notification dependency.

Build an offline archive from the clean pinned pi-status-led checkout:

```sh
python3 scripts/build-led-artifact.py /path/to/pi-status-led /tmp/led-daemon.pyz
```

The required source commit is
`fbe71f9f6edd108beceb01d568a0b17b5a6eb85c`. The builder emits the SHA256. The archive
uses only the Python standard library and includes a shutdown guard that refuses
to restore hardware settings changed by another actor. Digest verification pins
the selected bytes; it is not a signature or independent source-provenance proof.
The administrator must trust the selected release artifact. An abrupt daemon
crash cannot prove restoration of its original in-memory hardware snapshot.

Set `WORKER_LED_SOURCE=/path/to/pi-status-led make worker-bundle` to include the
archive for both architectures. Alternatively supply `daemon.artifact` and
`daemon.sha256` in YAML; that file is operator-local and is privately transferred.
The normal catalog is sufficient when it already includes the archive.

## Existing daemon


Short setup can attach to the supported pi-status-led 0.1.0 daemon on a Raspberry
Pi. Select it explicitly in `server.yaml`:

```yaml
version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    notification_worker:
      providers: [telegram]
      status_leds:
        enabled: true
        socket: /run/pi-status-led.sock
        daemon:
          mode: existing
```

Run `deploy notification-worker manager`. Missing Telegram credential references
are collected through the normal guided setup. Signal uses the same LED setup
after its private provider inputs are selected.

The target inspection must identify a Raspberry Pi and actual LED devices. The
existing daemon must expose an available green device matching that inventory,
the supported patterns, and an `off` or `blink-slow` default. Setup checks socket
ownership and kernel peer identity. The worker retains those identities and
checks them on each connection. It does not edit the existing daemon, install
packages, grant device permissions, or alter another application's registry states.

With no custom bindings, the built-in worker LED reflects aggregate health,
including backup health after enrollment. Notification-first setup starts with
unknown observations and no invented producer. Production healthy leases require
fresh evidence; registry acceptance does not establish physical visibility.
Use `deploy notification-worker qualify manager` to record the actual observations.
The wizard also asks for the observed daemon version and an operator-local copy of
the daemon configuration used in those checks. It stores the configuration digest,
keeps incomplete evidence pending, and resumes without renewing old observations.

For a **verification gateway**, change only `status_leds.enabled` to
`false` and run the same apply command. The coordinator stops the owned worker,
fences refresh, clears its proven output, and retains unresolved incidents and
binding identities. An unavailable daemon leaves cleanup pending; repeat the same
declaration after restoring access. Setting it back to `true` reuses the retained
identities and admits fresh reconciliation without creating healthy evidence.
`status manager` reports the retained output permission and any pending operation.
After local backup enrollment, the same toggle updates the staged backup's
notification policy scope with the gateway. Keep its remaining policy unchanged.
Both completion receipts are required before qualification or delivery; interrupted
publication reports `notification_policy_pending` on the backup. Resume using the
same YAML with `deploy notification-worker resume manager`. Bindings and provider accounts remain installed when output is disabled.

For a **production gateway with a local shared backup**, change only
`status_leds.enabled` and apply the notification worker:

```bash
deploy notification-worker manager
```

The toggle keeps the daemon, bindings, incidents, producer generations, reporting
owner and saved command results. Both workers support apply, resume, status and
plan afterward; existing Telegram command toggles can be applied separately.
Other clients' LED output stays intact. Interrupted updates resume with the same
YAML and `deploy notification-worker resume manager`.

Both directions require valid delivery qualification because the gateway service
must restart. Re-enabling also requires that output was enabled in the original
qualification. These commands do not renew qualification or create fresh healthy
observations. Changing production bindings, the daemon/device, or a recovered
installation remains a separate unfinished workflow; see the [roadmap](../roadmap.md).

`deploy notification-worker uninstall manager` automatically performs this output
cleanup before removing an unbound gateway. It retains a separate cleanup receipt,
so an interrupted clear or lost removal reply resumes through the same uninstall
command. An unavailable daemon produces `cleanup_pending`; the command cannot
report successful removal before owned output is gone. Completed retries do not
clear again. Unresolved incidents and other applications' output remain intact.

If the gateway owns a managed daemon, uninstall then stages an administrator
removal review and prints its command. Review the listed effects: stopping this
daemon ends API access for every client of that UID's socket. Run the displayed
command on the Pi, then repeat the short uninstall command to verify completion.
Until that receipt is verified, status reports `cleanup_pending`.
The pinned daemon must first confirm that its live registry is empty, including
other clients' persistent faults and temporary leases. Remaining states block
removal and stay untouched. Under the same request lock, the daemon durably
fences new state mutations before acknowledging the removal plan; a concurrent
client cannot create a new obligation between inspection and shutdown. The fence
survives daemon restart and is retained with the host removal history.
The host operation stops socket activation before the daemon, removes only its
unchanged system units, and retains the daemon configuration/archive, persistent
registry, hardware lock inodes and operation history. Interrupted or lost replies
resume the same removal; changed files and missing authority require review.
An existing-mode daemon is not owned or removed by this workflow.

Initial custom bindings for the local backup worker are supported. Declare them
before the first notification apply, alongside the local backup dependency:

```yaml
notification_worker:
  providers: [telegram]
  status_leds:
    enabled: true
    bindings:
      backup:
        producer: backup-worker
        subjects: ['*']
        led: green
        styles:
          critical: {pattern: blink-fast, priority: 95}
backup_worker:
  sources: [vps-main]
```

Notification-first apply explains that these bindings are waiting for a real
producer. The aggregate output remains installed in the meantime. Backup apply
reserves its identity and enrolls it, then applies the mapping through a separate
resumable transition before qualification or delivery admission. The original
enrollment receipt remains intact. Interruptions resume without re-enrollment or
replaying a completed daemon change; foreign output and retained faults remain.
Mapping refuses if changed bindings have unresolved fault/cleanup records. A
completed output-disable/re-enable history is retained through an exact handoff. This does not
qualify physical visibility or activate production reporting.

With `power_led: true`, a binding using `led: power` may select the inspected
available red device of an existing daemon or the PWR device provisioned by initial
managed setup. The opt-in alone does not invent a producer or power binding. Use
power output only when that takeover is intentional.

Completed unbound output dispositions now survive limits updates and initial
producer enrollment through versioned history pins. Enrollment retains disabled
output and faults and leaves the producer inactive. Initial custom mappings after
re-enable retain that same output history. Mapping receipts pin the completed
output record, and missing history blocks migration. The managed domain can also
change mappings while output stays disabled without publishing daemon states;
the short workflow currently installs initial bindings with output enabled.
Mapping does not replay cleanup or resolve faults. Later helper updates must
retain support for these versioned history pins.

After enrollment, while the gateway remains in verification and the backup is
staged awaiting shared-reporting qualification, edit `status_leds.bindings` and
run `deploy notification-worker manager`. This supports changing subjects/styles,
adding/removing bindings and returning to an earlier mapping. Keep the rest of
the policy, helper and daemon unchanged for this operation. Removing all custom
bindings leaves the aggregate status output enabled.

The transition retains the producer, source keys, archive and provider account.
It records both the gateway mapping and the backup's matching policy scope.
If interrupted, keep that exact YAML and run
`deploy notification-worker resume manager`. Backup status reports
`notification_policy_pending` until both publications complete. Qualification
and delivery require matching completion records; no observation is synthesized.
An independently stopped owned Signal service can resume without linking again.

Production binding changes, daemon replacement, and production output toggles
are still being implemented. Those
transitions refuse before service mutation. Disabling output retains the daemon;
it does not remove its registry or erase incident history. Do not remove
retained state to bypass these lifecycle requirements. Managed socket relocation
also requires a migration; an explicitly existing daemon may use its declared
socket override. No live hardware or physical visibility was qualified by the
software tests.
