# Signal account linking and interruption recovery

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

Both the legacy backup relay and shared notification relay now use the common
account lease and linking journal in `internal/notification/signalaccount`.
The backup `signal-link` command invokes a fenced deploy entry point inside its
configured digest-pinned relay image. It no longer launches `signal-cli` directly.
An older image without that entry point fails; there is no unfenced fallback.

The backup command now coordinates its enrolled legacy relay service. Before
stopping it, the command captures its activity and binds the exact configuration,
image enrollment, credentials, expected Quadlet and current generated service
fragment. Foreign sources, aliases, drop-ins, pending jobs and stale manager
configuration fail before mutation. A network-disabled, account-free container
probe requires maintenance protocol version 2; upgrade older relay images first.

A durable service gate is published before stopping `deploy-backup-signal.service`.
Both compatible relays honor the gate, including automatic restart attempts.
The command waits for stop completion and requires the account lease before linking.
After a confirmed or explicitly reviewed outcome, it restores the recorded active
or inactive state. It never enables/disables services or changes timers. The
notifier lease excludes legacy sending and notification-owner handoff during this
operation. The installation coordinator lease excludes concurrent setup/update;
setup and recovery also refuse unfinished account/service maintenance after a
coordinator exits. Only the canonical installed worker config/state paths are
accepted. Configuration or credential drift fences resume rather than silently
changing the destination. This adapter manages the existing backup relay. Use
[legacy Signal adoption](legacy-signal-adoption.md) and [Signal production](signal-production.md)
for shared gateway account handoff.

A disconnected/killed coordinator is resumed by repeating the same backup
`signal-link` command. It recovers the unfinished operation and original activity,
rather than capturing the now-stopped service as the intended state. An uncertain
CLI attempt is never repeated: inspect/review it as below, then repeat the backup
command to finish restoration. An account review alone does not remove the service
gate. A surviving child blocks progress until it exits. Upgrade any older unfenced
relay/linking binaries and terminate their old processes before using these controls.

The account directory must be private (`0700`), owned by the runtime user and
accessible without symlinks. All callers use its same persistent
`deploy-account.lock` inode, regardless of socket names. Never remove the lock or
replace the account directory while a relay or CLI/JVM descendant can still live.
The in-container helper and actual CLI child inherit the lease independently of
SSH or the Podman client. Closing a parent's descriptor does not unlock the child's
copy. A surviving child also blocks inspection/resolution until it exits.

Each link operation has an explicit ID. Its immutable intent and head precede a
durable launch marker; the CLI is invoked only after that marker. The entry point
streams its bounded linking output to the requesting command and discards stderr.
No linking URI, CLI output, incoming content or phone number is copied into the
journal or error message. The CLI subprocess group has a five-minute budget and
five-second shutdown grace. A nonzero exit, timeout, process crash or lost result
leaves uncertainty instead of assuming the account was unchanged.

An exact retry may launch only if the journal proves that no launch marker was
written. It cannot replay a possibly executed link. Repeating an acknowledged
operation returns its original outcome; reusing an operation ID with another
device name is rejected. A deliberately new link needs a new operation ID. The
backup command defaults to the retained service operation, including a completed
one. This makes repeating the command after a lost final reply safe. To deliberately
start another link after completion, use `backup-worker signal-link --new-link
<worker>`. For exact automation retries, supply `--operation n-...` with a fresh ID
once, and retain/reuse that ID. The flags are mutually exclusive. A different ID or
`--new-link` cannot supersede unfinished or unacknowledged maintenance.

Inspect the canonical account directory on the worker, using the **host account
path**, not the worker's SQLite state or Signal inbox directory:

```sh
deploy notification-worker signal-link-status --local \
  --state-dir /home/notify/.local/share/deploy/backup-worker/signal
```

The service journal can also be inspected while the relay is running:

```sh
deploy notification-worker signal-service-status --local \
  --state-dir /home/notify/.local/share/deploy/backup-worker/signal
```

Service phases are `prepared`, `stopped`, `settled`, `released` and `complete`;
`none` means no coordinator operation. Status includes the recorded prior activity,
operation ID and pending publication acknowledgement. A pending acknowledgement
requires exact resume and blocks new runtime startup.

Link statuses are `none`, `publication_pending`, `prepared`, `uncertain`, `linked` and
`reviewed`. `runtime_allowed` reports the combined link/service maintenance gates, not live provider
qualification, account correctness, service health or production authorization.
Both relays refuse startup while an operation is incomplete or uncertain. Merely
reading status never changes the journal or restarts a service.

After inspecting the account and linked-device state, explicitly accept the
reviewed state using the exact operation ID reported above:

```sh
deploy --dry-run notification-worker signal-link-resolve --local \
  --state-dir /home/notify/.local/share/deploy/backup-worker/signal \
  --operation n-REPLACE_WITH_REPORTED_ID \
  --accept-account-state --reason 'Inspected account and linked devices after interruption'
deploy notification-worker signal-link-resolve --local \
  --state-dir /home/notify/.local/share/deploy/backup-worker/signal \
  --operation n-REPLACE_WITH_REPORTED_ID \
  --accept-account-state --reason 'Inspected account and linked devices after interruption'
```

Resolution requires the account lease and records the reason durably. It never
claims linking succeeded, verifies an account, executes the CLI, erases prior
evidence or starts a service. It can also cancel a prepared operation before
launch. Exact retries preserve the original resolution; a conflicting reason is
rejected. For a coordinated backup operation, repeat `backup-worker signal-link`
after review to restore the recorded prior activity. Interactive relays retain their independent account identity
checks at startup.

History is limited to 64 operations and 32 abandoned JSON temporaries in
`deploy-link-history`. Records have digests and link to the prior completed
operation; `deploy-link-head.json` independently detects a missing committed
intent. Missing/corrupt/unknown records block runtime startup. Evidence is not
automatically deleted. The service journal independently retains up to 64 operations,
five immutable phases each, with a hash chain and `deploy-service-link-head.json`.
It pins the exact link result before permitting runtime startup. Checksums protect consistency, not against a compromised
account owner rewriting all state. Global dry-run validates arguments and writes
no account files, starts no children and changes no service.

Automated coverage includes exact retries, busy leases, failed children, bounded
history/output, damaged evidence, clock rollback, six link SIGKILL scenarios and
eleven coordinator SIGKILL boundaries. Tests cover lost stop/start/child replies,
prior active/inactive restoration, changed service inputs and native executable
review/resume. The service-manager tests use controlled fixtures, not a live
user-systemd relay.
The pinned AMD64 Podman fixture runs with `--network=none`, disposable tmpfs and no
real account. It checks actual JVM descriptor inheritance and the real linking
wrapper's timeout/uncertainty behavior. This is not successful live device linking,
provider delivery, native ARM64 execution or managed service-handoff qualification.

## Recovery account fence

The lifecycle API `signalaccount.FenceRecovery` now fences the canonical account
before stopping its pinned service. Its adapter must verify the exact loaded
service/account mount and recovery-capable executable or image, including
`notification-worker __signal-recovery-probe` version 1. The older account-linking
probe alone is insufficient. The native user-systemd adapter below now connects
this gate to offline inbox installation. The rootless Podman backend is described
in [Signal Podman recovery](signal-podman-recovery.md). Native shared endpoint
installation and replacement use [Signal production](signal-production.md).

The operation pins one request ID, service binding, recovery target, directory and
account-lock identities, and the existing linking/service-maintenance history.
The account must already have its stable `deploy-account.lock`, created by the
previous relay. Unsettled linking or service maintenance blocks recovery before
service stop. Immutable intent and fence copies precede stop; the exclusive account
lease then proves that cooperating CLI children have exited. The operation holds
the service-operation lease through completion, preventing concurrent linking
maintenance. The short control lease protects file transitions, not service I/O.

Independent result and completion records retain the outcome of an idempotent
offline action. Resume after a saved result calls its verifier without applying
the action again. A crash before that result is saved requires the action's own
exact retry protocol. Changed service, paths, leases, maintenance history or
result evidence fails closed. A completed fence **still disables runtime**; no
receiver is started and no release/clear operation is provided. Later activation
and repeated recovery epochs need their own protocol. Keep all four
`deploy-recovery-{intent,fence,result,complete}.json` records with the account;
never delete them or replace the lock file to force a restart.

Ordinary relay starts, linking, resolution and link-service maintenance now obey
this gate. The internal installer integration `InstallSignalRecoveryWithService`
combines it with the exact gateway publication and recovered inbox installation.
The stopped gateway's exclusive lease spans service stop and installation;
publication/directory/database/lease identity is rechecked at the action boundaries.
Completed replay independently checks the retained source, installed inbox and
publication. Neither the gateway configuration nor provider admission is changed.

Inspect the gate without a provider call:

```sh
deploy notification-worker signal-recovery-status --local \
  --state-dir /absolute/canonical-signal-account
```

`phase` is `none`, `prepared`, `fenced`, `result` or `complete`. A completed recovery
returns `runtime_allowed: false` and the independent `result_hash`. The linking
and service-status commands also report runtime disallowed while recovery is
fenced. This status is a local audit, not live-provider qualification. The existing
`recovery-signal-install` command remains an offline inbox tool; it does not invoke
a service adapter or create this account fence itself.

## Native systemd recovery handoff

`recovery-signal-handoff` handles an **existing native host relay** named
`deploy-notification-signal.service`. It stops that service after publishing the
account fence, then installs the published gateway's exact offline inbox cursor.
The gateway must already be stopped and its recovery publication complete.
Completion leaves the relay stopped and account runtime disabled. This operation
does not install/reload a unit, change an endpoint/configuration, start a
verification server, release command fences or enable production intake.

Copy [signal-recovery-native.json](../../../examples/notification-worker/signal-recovery-native.json) to a private
mode-0600 file and replace every placeholder. Use one new operation ID of the
form `n-` followed by 32 lowercase hexadecimal digits and retain it for retries.
Pin SHA-256 of the exact gateway JSON, relay JSON, native ELF executable and unit
file; `commit_hash` comes from the completed gateway publication. Paths must be
clean, absolute, owned by the current user and free of symlinks/hardlinks. The
account, gateway, old inbox and new inbox directories must be separate. Keep
configuration, executable and credentials outside those writable state directories
and outside the socket directory. The relay's account/inbox/socket must agree with
the request and gateway, and its private account/recipient files are fingerprinted
into the durable service binding. Neither credential values nor manager stderr
are returned in recovery diagnostics.

The supported native unit profile is intentionally narrow:

- The unit is loaded from the current user's
  `$XDG_CONFIG_HOME/systemd/user/deploy-notification-signal.service`, or
  `~/.config/systemd/user/...` when XDG_CONFIG_HOME is unset. It is enabled or
  disabled, has no aliases/drop-ins/generated source, and needs no daemon reload.
- `Type=exec` or `simple`, `KillMode=control-group`, `SendSIGKILL=yes`,
  `TimeoutStopSec=30s`; a steady running, stopped or failed state with no pending
  manager job/control process. The only start command is the pinned executable
  with arguments `notification-worker __signal-relay --local --config ABS`.
  Shell/container wrappers, execution-prefix flags and auxiliary Exec commands
  are refused.
- No user/group switch, environment overrides/files, chroot, bind/image/temporary
  mounts, private mounts/tmp, protected home, delegation or systemd-managed
  runtime/state/cache/log/config directories. The latter could remove or alter
  account data during stop. Unsupported/missing manager properties fail closed.
  The separate [Podman adapter](signal-podman-recovery.md) handles its own narrow
  interactive Quadlet profile; this native command cannot adopt containers.

The adapter uses the typed
[systemd `ExecStartEx` property](https://www.freedesktop.org/software/systemd/man/latest/org.freedesktop.systemd1.html)
through `busctl --user --json=short`, together with bounded `systemctl --user show`
and `stop` calls. It checks both tools' structured output before stopping. For an
active relay it also verifies the actual executable inode, argv, UID, process
start identity, and the exclusive account and source-inbox lease descriptors in
`/proc`. A lease seen at a different inode inside the process is refused. This also
rejects a configuration redirected to another account/inbox after process start. This requires Linux
`/proc` visibility for the current user's service. The capability probe executes
the already-open pinned binary with no account/configuration arguments, and
service/file bindings are rechecked around the probe and recovery boundaries.

```sh
deploy --dry-run notification-worker recovery-signal-handoff --local \
  --spec /absolute/private/signal-recovery-native.json
deploy notification-worker recovery-signal-handoff --local \
  --spec /absolute/private/signal-recovery-native.json
```

Dry-run validates local input files only: no manager command, probe, state opening
or mutation. Real execution returns `installation` and `account` JSON; the account
phase must be `complete`, with `runtime_allowed: false`. Re-run the identical
command/spec after interruption, including an ambiguous stop response. Saved
results are independently verified, and a stopped/completed relay is not stopped
again. Changed pins, credentials, service binding or publication require review;
do not delete the retained account records to bypass a mismatch.

Tests use a built deploy CLI, a disposable native relay process with a real
account flock, and strict fake systemctl/busctl programs. They cover lost stop
responses, exact replay, replaced executable inodes, account/configuration drift
and permanent fencing. A separate private D-Bus fixture tests the actual busctl
serialization, including rejection of execution flags; it requires dbus-run-session
and Python D-Bus/GLib. This is not qualification against a live user-systemd
service, live Signal, native ARM64 or Podman.
