# Backup commands from Telegram

Examples and supporting files: [repository examples](../../examples/workers).
Run repository-relative commands from the repository root unless stated otherwise.

Declare commands when creating a new local shared backup/notification installation,
or add them to an existing Telegram gateway before delivery activation.
The initial short workflow supports one backup alias, Telegram conversations and
read permissions, with optional explicit incident-management permissions. Fixed outbound alerts keep their separate chat-ID file.

For example, extend the notification block in [telegram.yaml](../../examples/workers/telegram.yaml):

```yaml
notification_worker:
  providers:
    telegram:
      token_file: ~/.config/deploy/credentials/telegram-token
      chat_id_file: ~/.config/deploy/credentials/telegram-chat
      account: '123456789'       # Bot ID: numeric prefix of the token.
      bot_name: MyBackupBot     # Bot username, without @.
  commands:
    enabled: true
    mode: read
    workers:
      backup: backup-worker
    conversations:
      ops:
        provider: telegram
        account: '123456789'
        native_id: '-1001234567890'
        # thread: '42'          # Optional Telegram topic.
    permissions:
      - actor: '987654321'      # Authorized Telegram user ID.
        conversation: ops
        worker: backup
        permissions: [read]
        scope:
          whole_worker: true
```

Keep token/chat files private to the target user. Operator-local `token_input` and
`chat_id_input` references are also supported; credentials travel through private
input, never YAML values or command arguments. The supplied token must match the
declared account. Conversation and actor IDs are explicit authorization settings.
For narrower access, replace `whole_worker` with `subjects: [...]` using the
specialist's subject IDs; do not specify both.

Use the same short setup commands:

```sh
deploy notification-worker manager
deploy backup-worker manager
deploy notification-worker qualify manager
deploy notification-worker manager
```

Either worker can be installed first. Initial setup reserves the actual backup
identity and command alias without enabling intake or replies. After you record
the required actual qualification observations, reapply admits reporting, checks
the installed backup helper/report service, stages the receiver, then separately
admits the declared command mode. Missing qualification remains pending. Applying does not
invent observations or send a diagnostic.

The final result is `commands_admitted`. In an authorized conversation, send
`backup status`. The specialist also supports `details` and `why` with explicit
incident `subject` and `target` arguments. Status reports retained incidents; it
does not establish current backup freshness or prove a successful capture.

## Change a notification-first setup

If you installed the notification worker first and it reports
`waiting_for_producers`, you can revise the `commands` block before installing
the backup worker. Add the existing bot's `account` and `bot_name` if needed,
then run `deploy notification-worker manager` again. The declaration is retained
without restarting the gateway, changing its runtime or reserving a backup identity.

Keep credentials, provider selection and backup settings unchanged during this
edit. Ordinary unbound limit/route changes can follow in separate applies while
the future command declaration remains in YAML. Once backup setup has reserved
an identity, finish that setup before changing command policy; the enrolled
verification workflows below then apply.

## Add commands before activating an existing gateway

If your local backup and Telegram gateway are installed but delivery has never
been activated, add the command block above to `server.yaml`. Fill in `account`
and `bot_name` for the existing bot if they were not declared. Keep the token and
alert chat files, backup settings, LED settings and other policy unchanged.

Run `deploy notification-worker manager`. The update retains the existing backup
reservation, archive, source keys, gateway identity and LED history. It reserves
command aliases without starting intake or replies. Both workers record the new
policy; interrupted work resumes with the same YAML.

Next run `deploy notification-worker qualify manager` and record your actual
observations. Reapply to connect reporting, stage the receiver and admit commands.
A gateway already activated for delivery, a recovered gateway or a different
provider/account needs a separate migration; this workflow will refuse it.

## Edit command settings before activation

While the local shared gateway is still in verification and has never activated
reporting or command intake, edit the existing `commands` block and run
`deploy notification-worker manager` again. You can change the alias, actors,
permissions, subject scopes, Telegram conversations/topics and read/mutation mode.
Keep `commands.enabled: true` and retain the same bot, credentials, backup
identity and other worker settings. The enabled declaration requests future
admission; this update does not start intake or replies.

For example, change `mode: read` to `mode: mutation` and explicitly grant
`incident.manage` to the intended actors. Or correct a conversation's `native_id`
before qualification. Old aliases stay reserved to the same producer. The update
preserves the archive, original enrollment and LED output state. Both workers'
apply, resume, status and plan commands use the revised declaration.

After the edits, qualify the current configuration and apply again. Receiver
review and command admission use the latest declared policy. Once reporting or
commands have been admitted, use the separate supported production changes below;
verification edits cannot change an existing receiver's authority.

## Postpone commands during verification

Before delivery activation, set only `commands.enabled: false` and run
`deploy notification-worker manager`. Keep the command block, bot and other
settings. This removes commands from the verification runtime while retaining
its reviewed policy, alias reservations, backup identity and LED state.

To restore that policy, set `enabled: true` and apply again. Re-enable and policy
edits are separate operations: restore first, then edit actors or conversations
while still in verification. Neither toggle admits intake or creates a receiver.
Qualification and later admission still use the current configuration. If a toggle
is interrupted, resume with the same YAML; both workers' ordinary commands remain
available after completion.

## Change Telegram files before command admission

Before delivery activation, save a replacement token or alert chat ID in new
private files on the target. Select them with the provider's `token_file` and
`chat_id_file`, then run `deploy notification-worker manager`. Keep the same bot
account/name, command declaration, conversations and other settings.

This works with configured commands and with a command policy temporarily disabled
during verification. Alerts and future command intake use the same selected bot
token. A token for another bot is refused. The fixed alert chat file is separate
from command conversations; changing it does not redirect command replies.

Keep all previous files for verification and resume. Pending notification messages
must settle before the change. Both workers retain the new references and original
producer identity; qualify the selected configuration before admission. Rotating
an activated gateway or replacing the bot still needs a separate migration.

## Optional incident management on a new installation

Keep `mode: read` for queries only. To permit reminder decisions when creating a
new installation, use `mode: mutation` and explicitly grant `incident.manage` to
the selected actors and scopes:

```yaml
commands:
  enabled: true
  mode: mutation
  workers: {backup: backup-worker}
  conversations:
    ops: {provider: telegram, account: '123456789', native_id: '-1001234567890'}
  permissions:
    - actor: '987654321'
      conversation: ops
      worker: backup
      permissions: [read, incident.manage]
      scope:
        subjects: [my-database-subject]
```

Replace the example IDs and subject with the actual enrolled values. `mutation`
mode makes `ack`, `snooze` and `unsnooze` available; only actors explicitly granted
`incident.manage` can use them, within their declared scope. It does not grant
backup jobs, shell commands or incident resolution. Read-only actors remain
read-only even in mutation mode.

Use `details` first to obtain the current incident ID, evidence revision and
decision revision. For example, replacing every placeholder:

```text
backup details subject=SUBJECT target=INCIDENT
backup ack subject=SUBJECT target=INCIDENT evidence_revision=EVIDENCE decision_revision=DECISION
backup snooze subject=SUBJECT target=INCIDENT evidence_revision=EVIDENCE decision_revision=DECISION duration=30m
backup unsnooze subject=SUBJECT target=INCIDENT evidence_revision=EVIDENCE decision_revision=DECISION
```

Each successful decision changes the decision revision; refresh details before
the next decision. Acknowledgement suppresses reminders for one hour; snooze
accepts one minute through 24 hours. The incident remains open and health remains
unchanged. Retries preserve the original result rather than extending suppression.
Stale evidence or decision revisions fail without changing reminders.

Mutation setup requires a matching backup helper with version-2 scheduled-command
support. An old helper is rejected before command admission. The same qualification
and receiver-before-admission steps apply. Existing read-only receiver records
remain read-only; editing their mode requires the still-pending migration workflow.

## Disable and re-enable existing commands

After command admission, change only `notification_worker.commands.enabled` in
`server.yaml`, keeping the bot, conversations, permissions and backup settings:

```yaml
commands:
  enabled: false
  # Keep the rest of your existing command configuration here.
```

Apply it with the usual command:

```sh
deploy notification-worker manager
```

The result is `commands_disabled`. New command intake stops; shared reporting,
the receiver and saved command results remain. Set `enabled: true` and apply
again to restore the same command mode and permissions. Re-enabling requires
the current delivery qualification to remain valid. If it has expired, the
command remains disabled and reports `command_qualification_required`;
delivery requalification still requires the specialist lifecycle.

An interrupted change can be resumed with
`deploy notification-worker resume manager` and unchanged YAML. Both workers
retain matching policy receipts; ordinary backup apply/resume and both workers'
status/plan commands continue to work after the toggle. This workflow applies to
already admitted local Telegram backup commands. Keep other worker settings
unchanged; initial enrollment on an already activated command-free installation
and receiver upgrades remain separate work.

## Reduce existing permissions

First set `commands.enabled: false` and apply, keeping the existing permissions.
Then edit only `commands.permissions` while commands remain disabled. You can
remove actors, remove rights such as `incident.manage`, or restrict
`scope.whole_worker: true` to explicit subject IDs. A subject list can be reduced
further. Keep conversations, aliases, providers and other worker settings unchanged.

For example, replace an existing whole-worker grant with:

```yaml
permissions:
  - actor: '987654321'
    conversation: ops
    worker: backup
    permissions: [read]
    scope:
      subjects: [source/my-database]
```

Use the subject IDs from your installed backup configuration. Apply with
`deploy notification-worker manager`. The result stays `commands_disabled`;
the gateway enforces the reduced rights and both workers retain matching policy
receipts. Interrupted changes use the usual `resume` command with unchanged YAML.
You can also set `permissions: []` to remove all grants while intake is disabled.

To resume intake, set `enabled: true` in a separate edit and apply. At least one
permission and valid delivery qualification are required. Re-enabling preserves
the reduced policy. Re-enabling alone never restores removed rights.

To restore previously removed access, disable commands and apply first. Then edit
only `commands.permissions` to restore an originally admitted actor, permission
or subject scope, and apply again. For example, you can restore whole-worker
access if that actor originally had it for the same worker and conversation.
Intake stays disabled until a separate enable. Old pending provider input is
rejected so a later grant cannot reinterpret it with broader rights.

Restoration also works after an alias rename, using the current alias. It cannot
restore a retired conversation, change an audience, add an actor the receiver
never admitted, or exceed an original scope. Those changes still need a receiver
permission or audience migration. Change aliases and mode separately; restoring
`incident.manage` requires mutation mode and an originally capable receiver.

Saved command results, the original receiver and shared reporting are retained.
Commands already accepted before disabling keep their recorded disposition;
changing permissions does not erase history.

## Rename an alias or retire a conversation

Disable commands and apply first. While `commands.enabled` remains `false`, you
can rename the backup alias in `commands.workers`; update each matching
`permissions[].worker` in the same edit. For example, change both `backup` entries
to `archive`:

```yaml
workers: {archive: backup-worker}
permissions:
  - actor: '987654321'
    conversation: ops
    worker: archive
    permissions: [read]
    scope: {subjects: [source/my-database]}
```

To stop using a conversation, remove its entry from `commands.conversations` and
all permission entries that refer to it. Keep at least one enrolled conversation
and the one backup-worker alias. You can reduce permissions in the same edit;
keep other worker settings unchanged.

Apply with `deploy notification-worker manager`. Intake stays disabled. Review
the resulting policy, then set `enabled: true` and apply separately when ready.
The same qualification requirement applies. Further permission reductions use
the migrated policy and do not restore retired conversations.

Existing results and their original reply destinations are retained. Pending
provider input from before the migration is fenced so it cannot be interpreted
under a new alias. This workflow cannot add conversations, restore a retired
conversation or redirect an existing chat/topic. Those changes need new audience
admission, which is still pending.

## Change command mode within the installed receiver's capabilities

An installation originally admitted with `mode: mutation` can switch to read
mode. Disable commands and apply first, then remove every `incident.manage` grant
and apply the reduced permissions. With commands still disabled, set
`commands.mode: read` and apply. Re-enable in a separate apply when ready.

You can later select `mutation` again while intake is disabled, then re-enable
with valid delivery qualification. This restores the mode only: removed actor
permissions stay removed. A mode change never grants incident-management rights.

A receiver originally installed for read-only commands cannot gain mutation
capabilities through this edit. The command reports that receiver upgrade
admission is required; that workflow is still pending. Existing reporting,
receiver records and command results remain intact.

## Processing and retries

The existing report timer processes commands every 30 seconds, with an eight-second
command budget before its normal report evaluation. Replies can take longer when
the gateway/provider is unavailable. A command error does not suppress backup
reporting. Exact retries preserve aliases, the original reporting handoff and
prior command outcomes. Backup capture qualification and schedule activation
remain separate.

Permission expansion, new or redirected audiences, receiver upgrades and native Signal intake
and interactive alert destinations still need their short migration
workflows. Keep the initial declaration unchanged when resuming; do not delete
receipts or databases to force a new installation. Live Telegram and Raspberry Pi
qualification remains an operator task.
