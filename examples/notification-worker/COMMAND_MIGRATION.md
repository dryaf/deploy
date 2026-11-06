# Managed command enrollment changes

Use `migrate-commands` to rename worker aliases, remove command workers or
conversations, rotate a conversation generation, and narrow permissions. This
operates on an admitted managed installation, including a published recovered
installation. It preserves the canonical runtime, delivery owner, account IDs,
worker command contracts and original recovery configuration.

Stop command admission first; use the returned manifest hash in the next command:

```sh
deploy notification-worker disable-commands --local \
  --root /home/notify/.local/state/deploy-notification/installation \
  --manifest-hash CURRENT_MANIFEST_HASH

deploy notification-worker migrate-commands --local \
  --root /home/notify/.local/state/deploy-notification/installation \
  --manifest-hash DISABLED_MANIFEST_HASH \
  --configuration-file /home/notify/command-enrollment.json
```

The mode-0600 configuration file contains the complete `worker.interaction`
object: `revision`, `workers`, `destinations`, and `permissions`. Start from the
latest retained command configuration, including prior narrowing. Set a new,
never-used revision. Remove permissions referencing removed conversations or
workers. Keep each retained worker's producer, generation and command descriptors
unchanged. Retained conversations keep their ID, provider, account, native ID and
thread; their generation may rotate to a never-used value. Aliases can be renamed
but historical aliases cannot be assigned to another producer.

The command refuses added authority, redirected audiences, reintroduced removed
enrollments, reused revisions/generations, changed private inputs and older helpers
without enrollment capability. Replacing provider accounts or changing native
audiences remains a separate configuration migration requirement. Keep outbound
notification configuration separate from command enrollment removal: removing an
interactive conversation does not remove its independently configured alert route.

Both pinned binaries must advertise enrollment support before the service stops.
The installer takes its normal operation/installation leases, records immutable
intent, and commits the effective configuration and pending-input fences together.
Unconverted inputs become retained silent rejections; they cannot be interpreted
under renamed aliases. Existing command envelopes, results and receipts remain
historical evidence. Prior-policy replies and new grants are withheld. Pending
provider events do not become new requests through a changed conversation generation.

After inspecting the result, explicitly re-enable with the current delivery
qualification hash and the new manifest hash:

```sh
deploy notification-worker enable-commands --local \
  --root /home/notify/.local/state/deploy-notification/installation \
  --manifest-hash MIGRATED_MANIFEST_HASH \
  --qualification-hash ACTIVE_DELIVERY_QUALIFICATION_HASH --mode read
```

Use `all` only for an installation already qualified for mutation commands. If
credentials or other qualification inputs changed, deactivate delivery, freshly
qualify, and reactivate before enabling commands. Same-account credential rotation
retains provider/account/conversation reply identities; there is no cross-provider
or alert-recipient fallback.

Global `--dry-run` checks the configuration and independent retained history without
opening the live database or stopping services. On interruption, use
`notification-worker resume --local --root PATH`; the input file is not required
after intent is durable. Retain the entire `managed-commands` directory and both
installation and worker state. Missing committed history is refused, never rebuilt.
Later recovery imports the current original policy history over the older snapshot,
preserving removals and renamed aliases. Use a complete configuration with a fresh
revision for subsequent permission changes after an enrollment migration.

Regular tests cover restore before conversion followed by a domain effect, alias
change, publication and reactivation; the effect remains exactly once. They also
cover managed removal/generation changes, dual-provider replies through credential
rotation, independent history loss, interruption and four SIGKILL boundaries.
These fixtures do not qualify real providers or user managers.
