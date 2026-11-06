# Resolve an inherited worker LED fault after recovery

After offline LED preservation, adoption and epoch handoff, complete recovered
publication and managed delivery activation. Reconcile producer-specific inherited
faults through the existing producer evidence/cleanup/import workflow first.
Retain those records and the original recovery sources.

An inherited aggregate worker fault remains visible even after fresh producer
status becomes healthy. Ordinary LED reconciliation never clears it implicitly.
The running managed gateway now provides an explicit review and cleanup operation:

```sh
deploy notification-worker led-recovery-review --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash CURRENT_MANIFEST_SHA256
```

Use the current installation hash from `installation-status`. The review reports
current aggregate health and proposed inherited worker fault IDs/hashes. It grants
no cleanup authority by itself. If the condition is not `ok`, inspect worker
status and resolve stale producer evidence, inactive ownership, delivery ambiguity,
failed runtime loops or outstanding producer-specific inherited faults first.
Do not edit ownership or cleanup records to make the review pass.

For one reviewed fault:

```sh
deploy --dry-run notification-worker led-recovery-clear --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash CURRENT_MANIFEST_SHA256 \
  --fault EXACT_FAULT_ID --review-hash EXACT_PROPOSAL_SHA256
deploy notification-worker led-recovery-clear --local \
  --root /home/notify/.local/share/deploy/notification-installation \
  --manifest-hash CURRENT_MANIFEST_SHA256 \
  --fault EXACT_FAULT_ID --review-hash EXACT_PROPOSAL_SHA256
```

Dry-run validates arguments without contacting services or writing state. Apply
uses the exact managed process/socket peer and keeps lifecycle changes excluded.
The runtime checks current admission, producer evidence, all monitored loops,
remaining inherited bindings and the exact daemon entry. The evidence deadline
must cover the bounded daemon request. Cleanup removes only the selected inherited
worker incarnation; it does not clear foreign states, producer bindings or current
faults, set a healthy lease, change daemon settings, or claim physical rendering.
Normal LED reconciliation may subsequently publish healthy output while current
freshness and admission continue to hold.

Each attempt retains an independent chained file/SQL history. If the command is
interrupted or its response is lost, retry the same fault/hash. A pending attempt
becomes historical `uncertain`; it is never automatically resent. A durable
`acknowledged` or freshly authorized `observed_absent` result accounts for removal
of that exact inherited ledger row. An uncertain result keeps the fault held.
Run a new review before another attempt. Missing or contradictory history blocks
health/cleanup; retained later files prevent an older head from hiding a later
attempt. A new daemon nonce requires a new review.

These `led-recovery-*` commands operate on a running, admitted recovered gateway.
The older `recovery-led-*` commands operate during offline quarantine; their
producer-specific cleanup and publication contracts are unchanged. Runtime and
helper binaries should be upgraded together. An older helper cannot interpret a
new completed aggregate cleanup as permission to ignore missing inherited proof.

Local tests use the pinned LED daemon with mock hardware, synthetic producer
status and local Unix sockets. Physical LED timing and actual user-manager
qualification remain operator checks.
