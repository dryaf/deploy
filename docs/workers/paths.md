# Backup paths

Examples and supporting files: [repository examples](../../examples/workers).
Run repository-relative commands from the repository root unless stated otherwise.

For a new installation, defaults use the target account's home and XDG roots.
The helper remains at `~/.local/lib/deploy/backup-worker/deploy` to preserve
existing backup installations. No operator home path is substituted remotely.

Customize only the locations you need:

```yaml
version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    backup_worker:
      sources: [vps-main]
      paths:
        config: ~/workers/backup/config
        runtime: ~/workers/backup/inputs/runtime.json
        state: ~/workers/backup/state
        data: ~/workers/backup/data
        installation: ~/workers/backup/helper
        denied_paths: [~/private]
  vps-main:
    host: vps.example.com
    user: deploy
```

Run `deploy backup-worker manager`. The archive defaults to `<paths.data>/archive`;
`storage.path` overrides it. State, configuration, keys, helper files and archive
must not overlap. An exclusion cannot hide required runtime or credential files.
Backup containers continue to receive only their explicit mounts; native health,
maintenance and report services receive the selected exclusions.

Custom layouts require `busctl` on the target. Setup checks the running user
manager's search paths and XDG configuration. The ordinary unit directory is
`<target XDG_CONFIG_HOME>/systemd/user`, and Quadlets use
`<target XDG_CONFIG_HOME>/containers/systemd`. A `paths.unit_directory` override
must already be in the manager's search path. Setup does not change the manager's
environment. For local shared reporting, both workers currently need the same
unit directory.

Paths may use `~/...` or clean absolute paths. New descendants of an owned target
home or the inspected XDG data root are created privately. An unrelated external
archive override requires an existing owned parent. Filesystem, capacity and
required-mount checks still apply; custom paths do not bypass qualification.

The setup journal and setup lock stay beneath the historical target
`~/.local/state/deploy` location, even when runtime state is elsewhere. Status,
recovery, source-key rotation and uninstall locate the retained paths through
that journal. Losing it is a recovery problem, not permission to initialize a
replacement database. Uninstall retains the archive, state and lifecycle history.

Changing an installed path, XDG root or exclusion set requires a separate path
migration, which the short commands do not yet implement. Restore the original
declaration to resume normal operations. Generated runtime and layout records are
audit/execution inputs; edit desired policy only in `server.yaml`.
