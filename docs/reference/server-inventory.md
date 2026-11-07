# Server inventory

`server.yaml` gives SSH hosts stable names. Version 3 also holds worker policy.
Use [getting started](../getting-started.md) for a new worker installation and the
[worker configuration reference](worker-configuration.md) for all worker fields.

```yaml
version: 3
servers:
  vps-main:
    host: vps.example.com
    user: deploy
    ssh_port: 22
    ssh_key: ~/.ssh/vps-admin
    ssh_known_hosts: ~/.ssh/known_hosts
```

In an application's `deploy.yaml`, reference that name:

```yaml
environments:
  prod:
    server: vps-main
    target_dir: /home/deploy/apps/my-app
    # Add the application's Quadlet and other runtime settings here.
```

A server reference replaces inline `host`, `user`, `ssh_port`, `ssh_key` and
`ssh_known_hosts` fields. Do not mix them, even with empty inline values.
Application directories, runtime and database backup opt-in stay in `deploy.yaml`.

For application commands, the default inventory is beside the application file.
Server-only commands use `./server.yaml`. `--servers PATH` selects another file;
there is no parent-directory search or inventory merging. Relative SSH paths
resolve against the inventory; `~` means the operator's home. Worker storage paths
resolve on the target as described in [worker paths](../workers/paths.md).

Version-2 inventories and legacy inline application SSH settings remain supported.
`server init` currently creates a version-2 inventory. To review a version-3
candidate without overwriting an existing inventory:

```sh
deploy server migrate --to-version 3 > server-v3.candidate.yaml
```

Keep candidates beside the original if they contain relative paths. Review
existing worker ownership through [import and adoption](../workers/import.md)
before changing how an installed worker is managed.

`deploy server provision vps-main` provisions only an explicitly configured
infrastructure stack. A host does not need a stack declaration to run workers.
Validate inventory with `deploy --servers ./server.yaml validate --server`.
Verified SSH host keys and usable authentication are prerequisites; naming a host
does not establish trust or prove two different names refer to different machines.
