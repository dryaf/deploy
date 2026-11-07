# Documentation

Start with the task you want to complete. Most users need the setup guide and a
configuration example; advanced recovery procedures are listed separately.

| I want to… | Read |
| --- | --- |
| Install Deploy and set up workers on a Pi or server | [Getting started](getting-started.md) |
| Deploy a Go application or an existing container image | [Application deployment](applications.md) |
| Configure and operate workers with short commands | [Worker guide](workers/README.md) |
| Define named SSH hosts and application references | [Server inventory](reference/server-inventory.md) |
| Find a YAML setting or its supported behavior | [Worker configuration reference](reference/worker-configuration.md) |
| Use Signal, LEDs or Telegram commands | [Signal](workers/signal.md), [LEDs](workers/leds.md), [commands](workers/commands.md) |
| Choose directories or import an existing installation | [Paths](workers/paths.md), [import and adoption](workers/import.md) |
| Repair, migrate or retire an installation | [Administration](administration/README.md) |
| Run an internal PDF service | [Gotenberg example](examples/gotenberg.md) |
| Understand current limitations and future work | [Roadmap](roadmap.md) |
| Develop or test Deploy | [Development](development/README.md), [validation](development/validation.md) |

Copyable YAML, JSON and scripts remain under [`examples/`](../examples).
Run repository-relative commands from the repository root unless a guide says
otherwise. Application release commands run from your application repository.

## Configuration in plain language

- `server.yaml` lists SSH hosts and the desired settings for their workers.
  `manager` is a name you choose for a host, not a special server role.
- `deploy.yaml` describes one application, its environments and its database
  backup opt-in. A backup worker selects source hosts from `server.yaml`.
- Private files hold passwords, tokens and other secrets. YAML stores references
  to those files. Runtime IDs, queues, receipts and backup history are generated
  state, not configuration you need to write.

## Design and implementation

The original implementation plans have been retired. Their contracts remain as
technical references: [image deployment](design/image-deployment.md),
[backup worker](design/backup-worker.md),
[notification worker](design/notification-worker.md) and
[interactive commands](design/interactive-commands.md).
The [original acceptance criteria](development/acceptance.md) retain their evidence
requirements. These documents are for maintainers, not installation instructions.

The [worker usability plan](plans/worker-usability.md) remains active because its
short-command workflows are not all implemented. Current priorities belong in
the [roadmap](roadmap.md), rather than separate progress journals.
