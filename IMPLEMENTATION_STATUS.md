# Consolidated implementation and acceptance status

Updated 2026-11-06. This is the current work list for the requested 90% target.
Work is paused at the user's request. See the pause checkpoint in `progress.md`
for the untested N8 draft and exact resumption steps. The broad race suite was
interrupted for the pause; no complete race-suite pass is claimed.
The normative contracts remain in [PLAN.md](PLAN.md),
[BACKUP_WORKER_PLAN.md](BACKUP_WORKER_PLAN.md),
[NOTIFICATION_WORKER_PLAN.md](NOTIFICATION_WORKER_PLAN.md), and
[INTERACTIVE_WORKER_PLAN.md](INTERACTIVE_WORKER_PLAN.md).
[progress.md](progress.md) retains chronological implementation evidence; old
remaining-work lists do not override newer checkpoints.
The stable acceptance denominator and per-criterion evidence map are in
[ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md); it records 84 existing criteria and
keeps incomplete subcases and environment requirements visible.

## Measurement

Do not derive completion from document length, checkpoint count, or test count.
Track implementation and qualification separately. `Recorded` means code and
prior test evidence exist, not that this review reran the acceptance gate.
`Partial` means the complete planned operator workflow is not available.
`Open` means implementation or acceptance evidence still needs work.
`Deferred` remains unqualified and is never counted as passed.

Before claiming 90%, map the normative acceptance scenarios to evidence, close
the critical activation/recovery/ownership workflows below, and rerun the
applicable automated gates against the final tree. This initial inventory is
not a measured percentage. Splitting a completed item into more rows must not
increase the score. No production-ready claim follows from local completion.

## Current inventory

| ID | Requirement | Status | Evidence / next acceptance step |
| --- | --- | --- | --- |
| D1 | Image/binary configuration, planning, rendering and dry-run | Recorded | `internal/config`, `internal/deployment`, `internal/quadlet`; audit PLAN acceptance criteria against final suite |
| D2 | Release, rollback, retention and interrupted transaction recovery | Recorded | `internal/deployment`; isolated image and VM crash gates remain required |
| D3 | Rootless Gotenberg behavior and application integration boundary | Open evidence | `examples/gotenberg`, `scripts/test-image-deploy-isolated.sh`; confirm pinned image, DNS, health and PDF evidence; external application changes remain separate |
| B1 | Inventory, managed metadata, existing configuration compatibility | Recorded | `internal/inventory`, `internal/config`, Quadlet tests |
| B2 | Restricted source export and consistent atomic archive | Recorded | `internal/backup/source`, archive and transfer tests; full SSH/rootless qualification required |
| B3 | Backup setup, scheduling, update, key rotation and uninstall | Recorded | `internal/backup/worker_setup*`, lifecycle tests; shared-mode lifecycle still restricted |
| B4 | Offline list/pull, archive recovery, retention and restore drills | Recorded | Backup archive/recovery/restore tests; full application drill and disconnected-source evidence required |
| B5 | Incidents, reminders, daily reports, fallback and external monitors | Recorded | Backup notification/monitor tests; actual missed-worker alert qualification open |
| N1 | Typed protocol, quotas, durable queue, receipts and provider adapters | Recorded | `internal/notification/{protocol,worker,provider}` |
| N2 | Freshness, report validity, obligations, batching and catch-up coverage | Recorded | Backup pipeline/period/receipt tests and worker dispatcher tests |
| N3 | Managed verification installation, update, rollback and administration | Recorded | `internal/notification/install`; actual user-manager qualification remains open |
| N4 | Initialized outbound-only Telegram activation/deactivation | Recorded | `managed_delivery*`; other profiles remain explicitly refused |
| N5 | Managed backup producer cutover and report service | Recorded, restricted profile | `notification_cutover*`; initialized outbound-only Telegram only |
| N6 | Held legacy outcome review and explicit release | Implemented, bounded actions | `notification-resolve`: audited unsent/retry decisions, duplicate-risk acknowledgement, immutable file/SQL evidence and canonical receipt tests; historical acceptance assertions/waivers remain unsupported |
| N7 | Restore legacy delivery after shared revocation | Implemented, restricted profile | `notification-rollback`: initialized Telegram, capable pinned binaries, canonical legacy generation, native sender, safe revoke and SIGKILL recovery; uncertain shared outcomes block, later generations and older-helper migration remain open |
| N8 | Recovered Telegram production activation | Open; untested draft | Worker ownership-continuity draft started in `managed_delivery_recovery.go`; not compiled/tested or integrated into the installer. Migrate retained ownership and consume current recovery/handoff/admission evidence under retained leases |
| N9 | Managed Signal account/verifier-to-runtime handoff | Partial | Verification service and profile migration exist; production account/delivery transition missing |
| N10 | Legacy Signal conversation enrollment and backup relay adoption | Open | Explicit destinations and exact stopped/account-owner proof; extend producer cutover |
| N11 | Signal verifier binary upgrade/re-enrollment | Open | Immutable revisions, crash-safe replacement and compatible rollback |
| N12 | Later managed recovery epochs | Partial | Offline recovery/publication exists; repeat complete managed recovery/activation cycles |
| L1 | LED client, freshness, bindings and durable ownership evidence | Recorded | `internal/notification/led`, worker/install LED tests |
| L2 | Worker-wide recovered LED reconciliation and cleanup | Partial | Complete aggregate reconciliation and managed activation; never clear foreign/current faults |
| L3 | Remaining UID/socket/configuration migrations and history cleanup | Partial | Explicit supported transitions, retained evidence, bounded storage and crash recovery |
| I1 | Command grammar, identities, authorization and reply correlation | Recorded | `internal/notification/interaction`, worker interaction tests |
| I2 | Backup handlers, durable results and synthetic risk fixtures | Recorded | Backup command tests; synthetic risk fixtures are not a vulnerability scanner |
| I3 | Managed intake/execution admission, disable and drain | Open | Read commands first, incident/job commands after qualification; preserve outbound availability |
| I4 | Interactive restore, replay and downgrade acceptance | Partial | Prove complete managed lifecycle with pending mutations and conversation isolation |
| Q1 | Final ordinary/race/vet and architecture cross-build | Open final gate | `make test`; cross-build is not native execution; run on final implementation |
| Q2 | Real disposable user-systemd/rootless integration | Partial | Native executable, pinned LED mock and offline rootless Podman Signal handoff passed; registry-backed image/backup/VM base unavailable; actual user-manager qualification remains open |
| Q3 | VM reboot/power-cut and end-to-end recovery | Open | `make test-deploy-vm-crash`, full backup/report/receipt/monitor and restore scenarios |
| Q4 | Actual Telegram/Signal receipt, fallback and external outage alert | Deferred | Explicit disposable accounts/destinations and pinned Signal image required |
| Q5 | Native ARM64 and physical LED qualification | Deferred | User-deferred manual checks; retain exact installation/configuration evidence |

## Execution order and completion evidence

1. N6–N7: close the current Telegram backup handoff's reconciliation and rollback
   loop. Include stale review, contradictory evidence, crash/retry, revoked
   generation, missing anchor, batch coverage and another producer's isolation.
2. N8–N12: complete recovered Telegram and Signal runtime/provider transitions,
   then repeat upgrade and recovery with retained historical evidence.
   `managed_lifecycle.go` currently refuses recovery adoption for delivery-enrolled
   state. `managed_delivery.go` refuses recovered runtime, and the worker's delivery
   owner remains bound to its original identity. Replacing these fences requires
   an audited owner-lineage transition plus fresh activation, not removing guards.
3. L2–L3 and I3–I4: complete managed LED and command admission, reconciliation,
   disable/drain and supported migrations without weakening existing fences.
4. Audit D1–D3 and B1–B5 against every normative acceptance scenario; implement
   any discovered gap rather than treating a recorded checkpoint as a pass.
5. Q1–Q3: run the final automated/local environment gates and update runbooks.
   Record Q4–Q5 separately until the designated external checks are performed.

Future real vulnerability and uptime workers are outside this implementation
target; their shared interface is qualified using clearly labeled fixtures.
Production deployment, real provider sends and hardware changes are not implied
by local implementation acceptance.

The user explicitly deferred live messenger testing on 2026-11-06: no live test
credentials are available, and they will perform those checks manually after
implementation. Continue software work and automated fixtures without requesting
credentials again. Report implementation and manual qualification separately;
manual checks remain unqualified and must not be represented as passed.
