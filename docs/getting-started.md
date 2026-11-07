# Getting started

Deploy runs on your computer and connects to Linux hosts over SSH. Describe each
host and its worker settings in `server.yaml`, then run a short command for that
host. For application releases instead, follow the [application guide](applications.md).

This guide uses a Raspberry Pi named `manager` and a backup source named
`vps-main`. Replace the example hostnames and users with your own.

## 1. Install the CLI

From this repository, with Go matching [`go.mod`](../go.mod) and Make installed:

```sh
make install
export PATH="$HOME/bin:$PATH"
deploy --help
```

`make install` puts the CLI in `~/bin`. Add that directory to your shell's `PATH`
permanently if it is not already there.

## 2. Prepare hosts and artifacts

The worker host needs trusted SSH access, Python 3, a non-root account with a
working systemd user manager and lingering enabled. Backup workers also need
rootless Podman/Quadlet and the tools described in
[host preparation](administration/backup-worker.md#prepare-artifacts-and-hosts).
Default backup storage uses a directory on a local ext4 filesystem; an external
disk is optional. See [paths and storage](workers/paths.md) for overrides.

Source applications must explicitly enable SQLite backups in `deploy.yaml`.
See the [application example](../examples/backup-worker/deploy.yaml). Listing a
source host does not opt all its databases into backups.

You also need a matching artifact catalog: worker executables and pinned images
for the host architecture. From this repository:

```sh
make worker-bundle
```

This creates a **partial development bundle** in `dist/workers`. Backup image
references and, for Signal, native Signal artifacts must be supplied separately.
Follow [artifact bundles](workers/README.md#local-artifact-bundles) before applying
the configuration. A complete ready-to-install release bundle is still roadmap
work. Building ARM64 executables does not qualify a Pi installation.

## 3. Write `server.yaml`

Create this file in the directory where you will run Deploy:

```yaml
version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    notification_worker:
      providers: [telegram]
    backup_worker:
      sources: [vps-main]
  vps-main:
    host: vps.example.com
    user: deploy
```

`manager` is an ordinary SSH alias. Default worker directories are under the
target user's home; override them in YAML when needed. Use
`deploy --servers /path/server.yaml …` to select another inventory.
Other starting points include the [Telegram](../examples/workers/telegram.yaml)
and [backup-only](../examples/workers/backup-only.yaml) examples.

Preview without SSH or file changes:

```sh
deploy --dry-run notification-worker manager
deploy --dry-run backup-worker manager
```

## 4. Set up the workers

```sh
deploy notification-worker manager
deploy backup-worker manager
```

Setup prompts for missing provider details. Credential prompts accept an existing
target path, `local:PATH` for a file on your computer, or `new` for hidden input.
Secrets stay in private files; YAML receives file references. See
[configuration and credentials](workers/README.md#configuration-and-credentials).

Backup setup can prepare its declared local notification dependency, so either
installation order works. Repeating unchanged configuration preserves identities
and avoids restarting unchanged services. Installation stages verification;
production delivery and scheduled backups are not yet active.

## 5. Verify and activate

Request a diagnostic message and check the actual result:

```sh
deploy notification-worker test manager
deploy notification-worker qualify manager
deploy notification-worker manager
deploy backup-worker manager
```

`qualify` records observations you supply; it does not perform tests for you.
Follow the prompts for each required check and provide a real evidence file.
Reapplying after the required observations allows delivery and connects shared
backup reporting. Complete any remaining action shown by status before continuing.
See [verification and activation](workers/README.md#verification-and-activation).

Request a manual backup:

```sh
deploy backup-worker run manager
deploy backup-worker status manager
```

Wait for a successful completed backup and inspect its result. `start_requested`
or `running_or_busy` does not mean a backup succeeded. Check status before retrying
an uncertain request. Record the remaining checks, then enable the schedule:

```sh
deploy backup-worker qualify manager
deploy backup-worker activate manager
```

## Everyday commands

| Task | Command |
| --- | --- |
| Inspect notifications | `deploy notification-worker status manager` |
| Inspect backups | `deploy backup-worker status manager` |
| Preview changes against the installed state | `deploy backup-worker plan manager` |
| Apply supported YAML changes | `deploy backup-worker manager` |
| Continue an interrupted operation | `deploy backup-worker resume manager` |

Notification workers have the same `plan` and `resume` forms. Keep the same
configuration when resuming. For automation, use `--non-interactive`; required
missing inputs are reported instead of prompted.

Removing a YAML block does not uninstall a worker. Read the
[supported lifecycle](workers/README.md#disabling-and-removing-workers) first.
Shared removal and several migrations remain unfinished; see the [roadmap](roadmap.md).

## Common results

| Result or term | Meaning and next action |
| --- | --- |
| Missing catalog/artifact | Finish the bundle setup; check its architecture matches the host. |
| `waiting_for_producers` / unbound | The notification service has no connected worker yet. Apply the declared backup worker. |
| Qualification required | Perform the requested checks, record real observations, then reapply. |
| Admission | Permission to enable delivery or commands after the required checks. |
| `up_to_date` | Configuration matches the recorded installation. Check health and qualification separately. |
| Interrupted or pending | Use `resume` with unchanged YAML and retain the existing state files. |
| Recovery or drift review | Inspect the mismatch and use [administration](administration/README.md); do not initialize replacement state. |

For optional features, continue with [Signal](workers/signal.md),
[Pi LEDs](workers/leds.md) or [Telegram commands](workers/commands.md).
