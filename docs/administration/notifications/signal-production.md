# Recovered native Signal production

Advanced administration: use this guide for explicit lifecycle and recovery operations.
For a new installation, start with the [worker setup guide](../../workers/README.md).

Examples and supporting files: [repository examples](../../../examples/notification-worker).
Run repository-relative commands from the repository root unless stated otherwise.

This workflow retains the completed account recovery, source inbox, installed
inbox baseline and verification endpoint. It adds an explicit production service
enrollment, then separately qualifies and activates the gateway. Live Signal and
native ARM64 qualification must be performed on the selected installation; local
fixtures use a synthetic CLI and simulated service manager.

First finish the recovery and endpoint steps in
[Signal verification](signal-verification.md). Outgoing profiles must name explicit enrolled conversations, or use the explicit
enrollment below. Adoption of a legacy backup relay without an interactive
account/inbox remains a separate workflow.

## Enroll the native service

Use `signal-service-setup --local --spec ABS` for an endpoint without an enrolled
service. For an existing service, stop/deactivate the gateway, uninstall the
predecessor with `signal-service-uninstall`, retain its root and evidence, and use
`signal-service-replace` with a fresh root and the exact `previous` reference.
Retain the normal service specification and add:

```json
{
  "runtime": {
    "cli_file": "/absolute/staged/signal-cli",
    "cli_sha256": "SHA256_OF_NATIVE_EXECUTABLE",
    "cli_version": "0.14.9",
    "account_file": "/absolute/private/account-selector",
    "account_sha256": "SHA256_OF_SELECTOR_FILE_BYTES"
  }
}
```

Adding `runtime` explicitly starts production receive maintenance and enables
replies to the endpoint's enrolled conversations when setup publishes the service.
The gateway still requires its own qualification and delivery/command admission.
Omitting `runtime` retains the verification-only service behavior.

The CLI must be a native ELF executable for the host architecture. The service
copies it into its private root and verifies its hash, reported version and
selected account identity. Shell/JVM launchers, PATH-selected binaries and
container wrappers are not accepted by this native profile. The account selector
is a private file containing the CLI account selector; its bytes are pinned and
copied privately. Input files must be outside the retained account, inbox,
gateway, endpoint, installation and unit directories.

The service derives allowed conversations and actors from the retained gateway
configuration. It exposes the conversation `/reply` and inbox APIs; the legacy
fixed-destination `/send` API is refused. Its private socket and health identify
the exact service enrollment, account, inbox baseline and recovery commit.

Each CLI command runs through an isolated supervisor with inherited account lease
and executable descriptors. The supervisor joins descendants even if the CLI
parent exits or cancellation occurs. Original account and inbox fences remain;
starting an ordinary relay cannot bypass them.

## Admit the gateway

Use `adopt-signal` or `supervise-signal` with the exact new service root/hash. A
replacement requires an explicit rebind; ordinary update cannot silently select
the successor. Run `migrate-signal-profiles` once when outgoing conversation
profiles still name the original socket/inbox. The canonical recovery bytes stay
unchanged; new outgoing traffic uses separately retained effective route revisions.
Existing queued route revisions are not rewritten.

For a legacy fixed-recipient outgoing profile on this same recovered account,
add an explicit mapping to the `migrate-signal-profiles` specification:

```json
{
  "signal_conversations": {
    "signal-profile-name": {
      "previous_destination": "EXACT_OLD_DESTINATION_FINGERPRINT",
      "conversation": "EXISTING_ENROLLED_CONVERSATION_ID"
    }
  }
}
```

The conversation must already exist in the retained interaction configuration for
this Signal account. Review that it is the intended recipient: an old opaque
fingerprint cannot establish that equivalence automatically. The mapping is
retained with the effective configuration and requires capable gateway/helper
binaries. Unknown profiles, changed old fingerprints, absent conversations and
attempts to redirect existing conversation profiles are refused. Remove this
operation-specific field from later update specifications.

Generate a fresh qualification template for this exact gateway manifest. Complete
its required observations on the chosen installation, with artifact digests, and
set `environment.signal_cli_sha256` to the enrolled native CLI hash. Leave
`signal_image_digest` empty for this native profile. A container image identity
cannot substitute for native CLI qualification. Record the report with `qualify`.

Use `activate-delivery` with the current manifest and qualification record hashes.
It admits outbound delivery; recovered producer generations still require their
explicit reconciliation/activation. Commands default to off. Follow
[command activation](command-activation.md) to enable `read` or `all`, disable
new intake/grants, and drain replies. Signal command admission checks the retained
recovery cursor/request handoff against the enrolled runtime inbox; advancing the
live cursor does not replace that proof.

## Stop, replace and resume

Disable/drain commands and deactivate gateway delivery before removing or replacing
the Signal service. `signal-service-uninstall` retains a permanent removal record;
resuming the old enrollment cannot restart production. A new enrollment uses a
fresh root and the exact retained predecessor. Never delete old roots, account
fences, source inboxes or completed endpoint evidence to force a restart.

Use `signal-service-resume --local --root ABS` for an interrupted service lifecycle
and gateway `resume` for its separate lifecycle. Exact staged files and completed
history are required. Missing completed evidence is refused, not reconstructed
from current live state. Later managed recovery uses retained ancestor runtime and
inbox proofs. General UID/socket/configuration or account changes can use
[retirement and replacement](retirement-replacement.md) after settling all obligations;
that path requires fresh enrollment and a fresh interactive account. Existing
account maintenance/recovery retains its separate ownership and cursor fences.
