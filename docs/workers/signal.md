# Fresh Signal from server.yaml

Examples and supporting files: [repository examples](../../examples/workers).
Run repository-relative commands from the repository root unless stated otherwise.

The short commands support a fresh linked Signal account for direct alerts, alone
or alongside Telegram. Coordinator adoption of an already owned native service is
covered in [import/adoption](import.md). Legacy account adoption, relinking, interactive commands
and LED setup are still separate unfinished parts of the usability plan. Start
with [signal.yaml](../../examples/workers/signal.yaml). Keep `status_leds.enabled` false for this workflow.

Supply a supported native `signal-cli` 0.14.9 executable in the worker bundle, or
declare a target executable and its exact SHA256:

```yaml
notification_worker:
  providers:
    signal:
      account_file: ~/.config/private/signal-account
      recipient_file: ~/.config/private/signal-recipient
      # Optional target-native runtime override; otherwise use the bundle.
      # executable: ~/.local/bin/signal-cli
      # executable_sha256: <exact SHA256>
```

The account file holds the account selector used by the native CLI. The recipient
file holds its direct-alert destination. Use private files owned by the target
user. `account_input` and `recipient_input` are alternatives that read private
files on the operator's machine and provision private target files. Do not put
secret values in YAML or command arguments. Missing references can be collected
with `deploy notification-worker configure manager`.

The native artifact must match the target architecture. Native execution checks
its pinned version and linked public account identity before service ownership is
published. A dynamically linked artifact also needs its dependencies on the target.

```sh
deploy notification-worker manager
deploy notification-worker resume manager
deploy notification-worker status manager
```

Fresh linking uses the controlling terminal for the private linking payload; it
does not write that payload to ordinary output, JSON, errors or operation journals.
Complete linking using Signal on your device. JSON and unattended runs can resume
an already linked operation but return `signal_link_required` before a new link.
Resume the same declaration in a terminal. An uncertain native linking outcome
requires review; retries never launch another link automatically.

Defaults place the account and inbox beside the gateway state and the service and
immutable artifacts beneath its data directory. Explicit `data` and `socket`
overrides resolve on the target. The socket must be inside its private service
directory and fit the Unix socket path limit. Target home/XDG values, owned paths,
credential contents and artifact digests are pinned in private operation records.

If a linking attempt has an uncertain outcome, inspect linked devices on your
phone and run the same short resume command in a terminal. The guided review
requires an explicit `yes` before inspecting that retained native account; it never
launches a second link automatically. An absent or unusable account remains a
manual action. JSON and unattended mode report the review requirement and do not
approve it. Status can inspect an interrupted account/service setup before the
gateway exists.

The first install starts verification services, without production delivery or a
test send. Declaring a local backup consumer enrolls its real producer through
`deploy backup-worker manager`; the dependency uses the same linking workflow.
Record actual observations with `deploy notification-worker qualify manager`,
then reapply to admit qualified shared delivery. Backup qualification and schedule
activation remain separate.

```sh
deploy notification-worker test manager
# Explicitly check the other YAML-configured destination, if present:
deploy notification-worker test manager --provider signal
deploy notification-worker uninstall manager
```

The default test selects the sole provider or the `alerts` route's declared
primary. `--provider telegram` or `--provider signal` selects that configured fixed
destination for this diagnostic only; it does not change YAML routing or supply
credentials. There is no automatic fallback. The command records one attempt per
provider and installed revision, verifies the Signal socket's owning process, and retains an
unknown outcome after a lost reply. Provider acceptance does not establish
recipient visibility or production qualification.
Selecting the primary explicitly returns its existing default-test record, so it
cannot repeat an earlier uncertain send. The other provider has a separate record.

Uninstall currently requires an unbound gateway. It removes the gateway service
before its Signal service, resumes interrupted removal, and retains the account,
inbox, credentials and history. It keeps the native ownership fence. Reinstall,
relinking and removal of a gateway with enrolled producers require their remaining
dedicated lifecycle work; deleting retained state is not a supported shortcut.

## Resume an intact stopped service

If the installed native Signal service stops, run:

```sh
deploy notification-worker resume manager
```

Explicit resume checks the original account, inbox, executable digests, unit and
completion records, and requires the owned unit to remain enabled and loaded. It
also checks that the native account lease is free before starting the service.
The original link and installation receipts stay unchanged; a separate bounded
restart journal records the operation. A lost reply resumes that journal without
starting an already healthy process again. This works after gateway helper or
policy-scope changes while retaining the native child's original pins.

Ordinary apply and status do not restart a stopped service. Disabled or changed
units, missing history, busy accounts and removed installations need their own
review; resume does not recreate state or relink an account. The installed helper
must advertise restart support before any restart intent is written. This does
not implement legacy-account adoption, service replacement or account recovery.

## Telegram updates alongside Signal

Before any producer reservation or enrollment, you can select new Telegram token
or chat files in YAML and run `deploy notification-worker manager`. Keep the old
files and leave the Signal declaration and artifact unchanged. This retains the
native account, link, inbox, service receipt and running process while updating
the gateway's Telegram policy. See [credential updates](README.md) for the file
reference rules and explicit diagnostics.

Interrupted updates resume with the same YAML and `deploy notification-worker
resume manager`. Both old/new Telegram inputs and the retained native authority
are checked before completion. A missing provider intent or native receipt must
be restored; retry does not recreate it or relink Signal. An independently stopped
native service may use its existing explicit restart lifecycle after those checks.
Changing the Signal account itself or its service/artifact lifetime still
requires the remaining migration work.

Owned direct Signal gateways with an enrolled local backup can change private
references and recipients through the [shared provider workflow](README.md#owned-signal-recipient-and-reference-changes).
Keep the same account bytes, native service, inbox, socket and executables, and
retain old files. Production changes pause for fresh qualification; immutable
recipient approvals and reporting exclusions retain the original authority.
Command intake, recovered variants and account/service replacement stay separate.
