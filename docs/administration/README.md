# Administration and recovery

For a new installation, use [getting started](../getting-started.md) and the
[short-command worker guide](../workers/README.md). The procedures here expose
specialist lifecycle operations for existing installations, explicit migrations
and recovery. They often require reviewed manifests and retained evidence.

Do not mix short setup commands and a low-level migration partway through an
operation. Read the selected procedure's prerequisites and supported state first.
Keep existing identity, archive and receipt files when investigating a failure.

## Backup operations

- [Backup administration](backup-worker.md): host preparation, stage/activate,
  archives, retention, restore and lifecycle changes.
- [Shared notification cutover](notifications/backup-cutover.md): delivery
  ownership, legacy outcomes, rollback and remigration.
- [Backup activation after recovery](notifications/backup-recovery-activation.md).

## Notification operations

| Task | Procedure |
| --- | --- |
| Install or update a verification service | [Installation](notifications/installation.md) |
| Record checks and review admission | [Qualification](notifications/qualification.md) |
| Enable delivery | [Delivery activation](notifications/delivery-activation.md) |
| Inspect producers, receipts and history | [Administration](notifications/administration.md) |
| Enable commands or migrate their policy | [Command activation](notifications/command-activation.md), [command migration](notifications/command-migration.md) |
| Recover Telegram intake | [Telegram recovery](notifications/telegram-recovery-intake.md) |
| Recover aggregate LED output | [LED recovery](notifications/led-recovery-runtime.md) |
| Retire and replace an installation | [Retirement and replacement](notifications/retirement-replacement.md) |

## Signal operations

For fresh short-command setup, use the [Signal guide](../workers/signal.md).
For existing specialist installations:

- [Verification service](notifications/signal-verification.md)
- [Native production service](notifications/signal-production.md)
- [Maintenance](notifications/signal-maintenance.md)
- [Legacy account adoption](notifications/legacy-signal-adoption.md)
- [Podman relay recovery](notifications/signal-podman-recovery.md)
