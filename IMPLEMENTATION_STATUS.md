# Consolidated implementation and acceptance status

Updated 2026-11-06. The active request is 100% software implementation coverage,
followed by passing regular `make test`, then race tests. The completed count is
**84/84 (100%) aggregate software coverage**. No original software criterion
remains partial. Final regular tests and vet passed; the authorized `make test-all` race gate
is now running. Manual provider, hardware, cross-UID host and VM observations
remain unqualified. Signal migration, repeated migration/recovery, shared backup
lifecycle, command enrollment and explicit retirement/replacement all have regular
lifecycle, native CLI and process-death evidence. See progress.md for exact logs.
Use `make test` for iteration and `make test-all` for the final regular/race gate.
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
Track implementation and qualification separately. The user explicitly selected
software implementation coverage for the 90% target on 2026-11-06. `Recorded` means code and
prior test evidence exist, not that this review reran the acceptance gate.
`Partial` means the complete planned operator workflow is not available.
`Open` means implementation or acceptance evidence still needs work.
`Deferred` remains unqualified and is never counted as passed.

For the user-confirmed software target, use the original 84 criteria and the
implementation-only audit in SOFTWARE_COVERAGE.md. Partial software criteria
contribute zero; manual observations remain separate and unqualified. The inventory
below groups work across criteria and is not another scoring denominator.

The earlier proposed full-acceptance 90% checkpoint still requires the per-plan
floors in ACCEPTANCE_MATRIX.md, complete critical activation/recovery/ownership
workflows and applicable external gates. Software coverage does not meet that
stronger checkpoint or imply production readiness. Splitting completed work into
more rows must not increase either score.

## Current inventory

| ID | Requirement | Status | Evidence / next acceptance step |
| --- | --- | --- | --- |
| D1 | Image/binary configuration, planning, rendering and dry-run | Recorded | `internal/config`, `internal/deployment`, `internal/quadlet`; audit PLAN acceptance criteria against final suite |
| D2 | Release, rollback, retention and interrupted transaction recovery | Recorded | `internal/deployment`; isolated image and VM crash gates remain required |
| D3 | Rootless Gotenberg behavior and application integration boundary | Open evidence | `examples/gotenberg`, `scripts/test-image-deploy-isolated.sh`; confirm pinned image, DNS, health and PDF evidence; external application changes remain separate |
| B1 | Inventory, managed metadata, existing configuration compatibility | Recorded | `internal/inventory`, `internal/config`, Quadlet tests |
| B2 | Restricted source export and consistent atomic archive | Recorded | `internal/backup/source`, archive and transfer tests; full SSH/rootless qualification required |
| B3 | Backup setup, scheduling, update, key rotation and uninstall | Recorded | `internal/backup/worker_setup*`, lifecycle tests; shared-owner setup/recovery, rotation and qualified report activation |
| B4 | Offline list/pull, archive recovery, retention and restore drills | Recorded | Backup archive/recovery/restore tests; full application drill and disconnected-source evidence required |
| B5 | Incidents, reminders, daily reports, fallback and external monitors | Recorded | Backup notification/monitor tests; actual missed-worker alert qualification open |
| N1 | Typed protocol, quotas, durable queue, receipts and provider adapters | Recorded | `internal/notification/{protocol,worker,provider}` |
| N2 | Freshness, report validity, obligations, batching and catch-up coverage | Recorded | Backup pipeline/period/receipt tests and worker dispatcher tests |
| N3 | Managed verification installation, update, rollback and administration | Recorded | `internal/notification/install`; actual user-manager qualification remains open |
| N4 | Managed delivery activation/deactivation | Implemented | Initialized and recovered Telegram, supervised Signal conversation profiles, and qualified LED admission; fresh per-revision qualification required |
| N5 | Managed backup producer cutover and report service | Implemented | Telegram and explicitly adopted Signal migration, shared setup/update, key rotation and qualified reporting; retained owner/helper proofs |
| N6 | Held legacy outcome review and explicit release | Implemented, bounded actions | `notification-resolve`: audited unsent/retry decisions, duplicate-risk acknowledgement, immutable file/SQL evidence and canonical receipt tests; historical acceptance assertions/waivers remain unsupported |
| N7 | Restore legacy delivery after shared revocation | Implemented | Audited Telegram/Signal rollback, remigration and repeated fenced cycles across recovery; immutable payload/receipt/source history and SIGKILL evidence |
| N8 | Recovered Telegram production activation | Implemented | Repeated recovery with retained ownership and command/request history, renewal/cursor handoff, backup activation and fresh qualification; native/process-death fixtures |
| N9 | Managed Signal account/verifier-to-runtime handoff | Implemented | Supervised native service, inherited account lease and child lifetime, single inbox owner, retained ancestry and repeated recovery; live relink/provider qualification separate |
| N10 | Legacy Signal conversation enrollment and backup relay adoption | Implemented | Explicit conversation mapping and legacy backup account adoption, incomplete-history inbox bootstrap, exact account/child fencing and published runtime handoff; CLI and native-helper checks pass |
| N11 | Signal verifier binary upgrade/re-enrollment | Implemented, verification and native production | Fresh immutable service roots, explicit endpoint transfer and gateway rebind, retained predecessor chain, interruption/branch/lease refusal; native process replacement and focused SIGKILL/race checks passed; actual host/provider qualification remains separate |
| N12 | Later managed recovery epochs | Implemented | Repeated managed Telegram/Signal, LED, backup and command recovery with retained ancestor proofs; regular native, lifecycle and SIGKILL checks pass |
| L1 | LED client, freshness, bindings and durable ownership evidence | Recorded | `internal/notification/led`, worker/install LED tests |
| L2 | Worker-wide recovered LED reconciliation and cleanup | Implemented, bounded retained history | Qualified managed LED activation and explicit inherited worker-fault review/cleanup implemented. Fully published recovery lifecycle/race and pinned-daemon socket/SIGKILL checks passed; file-tail and queue-ambiguity follow-ups passed. Regular repository checks passed; full qualification remains separate; foreign/current faults remain protected |
| L3 | Remaining UID/socket/configuration migrations and history cleanup | Partial | Explicit supported transitions, retained evidence, bounded storage and crash recovery |
| N13 | UID/socket/configuration/history lifecycle | Implemented | Explicit settled retirement and fresh replacement with original audit retained; new producer epochs and fresh native interactive account; narrower migrations preserve continuity |
| I1 | Command grammar, identities, authorization and reply correlation | Recorded | `internal/notification/interaction`, worker interaction tests |
| I2 | Backup handlers, durable results and synthetic risk fixtures | Recorded | Backup command tests; synthetic risk fixtures are not a vulnerability scanner |
| I3 | Managed intake/execution admission, disable and drain | Implemented, supported provider profiles | Explicit read/all admission, disable/drain, monotonic permission narrowing with retained recovery policy, independent history, credential/request pinning and interrupted-operation cancellation; recovered Telegram handoff implemented, prior command-history restore implemented; Signal admission implemented with retained recovery cursor checks; native report/command/restart/disable/revocation passed |
| I4 | Interactive restore, replay and downgrade acceptance | Implemented | Managed restore before conversion, retained original outcomes, alias migration, enrollment removal/generation rotation and dual-provider reply isolation; regular lifecycle/native/SIGKILL checks pass |
| Q1 | Implementation checks and later race qualification | Regular implementation gate passed; broad race deferred | make test passed (all regular tests plus vet). Prior focused race/SIGKILL and ARM64 cross-build pass. Broad race stopped at user request; make test-all remains a later explicit gate. Cross-build is not native execution |
| Q2 | Real disposable user-systemd/rootless integration | Partial | Native executable, pinned LED mock and offline rootless Podman Signal handoff passed; registry-backed image/backup/VM base unavailable; actual user-manager qualification remains open |
| Q3 | VM reboot/power-cut and end-to-end recovery | Open | `make test-deploy-vm-crash`, full backup/report/receipt/monitor and restore scenarios |
| Q4 | Actual Telegram/Signal receipt, fallback and external outage alert | Deferred | Explicit disposable accounts/destinations and pinned Signal image required |
| Q5 | Native ARM64 and physical LED qualification | Deferred | User-deferred manual checks; retain exact installation/configuration evidence |

## Verification and external qualification

All 84 original software criteria are implemented; the supported lifecycle scope
and retained-evidence boundaries are documented in SOFTWARE_COVERAGE.md. Final
regular tests and vet passed. Q1 now tracks the running broad race gate.
Q2–Q5 retain separate environment,
provider, monitor, VM, target-load and hardware qualification. The user will run
live messenger checks manually. Record exact installation/configuration evidence
and observed outcomes before promoting those acceptance results.

Future real vulnerability and uptime workers are outside this implementation
target; their shared interface is qualified using clearly labeled fixtures.
Production deployment, real provider sends and hardware changes are not implied
by local implementation acceptance.

The user explicitly deferred live messenger testing on 2026-11-06: no live test
credentials are available, and they will perform those checks manually after
implementation. Continue software work and automated fixtures without requesting
credentials again. Report implementation and manual qualification separately;
manual checks remain unqualified and must not be represented as passed.
