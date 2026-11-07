# Deploy

Deploy Go applications and registry images to Linux hosts using rootless Podman
and systemd. Run backup and notification workers on a server or Raspberry Pi.

Configure applications in `deploy.yaml`. Define SSH hosts and worker settings in
`server.yaml`; credentials stay in private files referenced by that configuration.

- **[Get started](docs/getting-started.md)** — install the CLI and set up your first workers.
- **[Application deployment](docs/applications.md)** — configure, release and roll back applications.
- **[Documentation](docs/README.md)** — guides, configuration reference and administration.
- **[Roadmap](docs/roadmap.md)** — what works today and what remains unfinished.

For example, declare a Raspberry Pi and its backup source:

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

With the CLI, matching worker artifacts and host prerequisites ready:

```sh
deploy notification-worker manager
deploy backup-worker manager
```

Setup asks for missing provider details and uses directories under the target
user's home by default. Follow the [getting-started guide](docs/getting-started.md)
to test notifications, check a manual backup and activate the schedule.
Installation alone does not enable production delivery or scheduled backups.

The short YAML workflow is still being completed. The roadmap distinguishes
implemented features from planned changes and manual qualification.

For development, use `make test` for regular tests and vet. `make test-all` adds
race tests after implementation and regular checks pass. See
[development and testing](docs/development/README.md).
