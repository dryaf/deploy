# Original acceptance criteria

These 84 criteria preserve the acceptance contracts for the original implementation.
They are not the completion measure for the newer YAML usability milestone.
See the [roadmap](../roadmap.md) for current work and
[validation](validation.md) for recorded software checks.

`Local` identifies implementation and fixtures, not successful live qualification.
`External` requires a real environment or observed provider/hardware behavior.
The tables retain qualification gaps; no deferred observation counts as passed.
The original software baseline was recorded as 84/84; full acceptance remains open.

## Image deployment

Source: [image deployment acceptance criteria](../design/image-deployment.md#abnahmekriterien).
Files below are under `internal/deployment/` unless otherwise stated.

| ID | Acceptance | Evidence to review | Status / remaining gate |
| --- | --- | --- | --- |
| D-A01 | Image-only project | `deploy_image_test.go`, `release_plan_test.go` | Local; disposable rootless rollout |
| D-A02 | Default binary compatibility | `deploy_binary_test.go`, `configuration_test.go` | Local |
| D-A03 | Exact immutable running image | `deploy_image_test.go`, `release_safety_test.go` | Local; registry/tag drift in integration |
| D-A04 | Internal service without router/host port/required env | `render_test.go`, `deploy_image_integration_test.go` | Local; Gotenberg DNS/PDF integration |
| D-A05 | Bounded health context | `deploy_image_test.go`, `transport_test.go` | Local; actual service health |
| D-A06 | Exact rollback including initial/inactive service | `deploy_image_test.go`, `release_safety_test.go` | Local; real manager failure cases |
| D-A07 | Interrupted unconfirmed release and recovery | `release_safety_test.go`, `deploy_vm_integration_test.go` | Partial; VM power-cut evidence |
| D-A08 | Dry-run/preactivation has no live effect | `release_plan_test.go`, `deploy_image_test.go` | Local |
| D-A09 | Current and prior image survive pruning | `retention_safety_test.go`, `ops_test.go` | Local; rootless store integration |
| D-A10 | Binary, failure and rootless Gotenberg gates pass | `Makefile`, `scripts/test-image-deploy-isolated.sh` | External; final integration result required |

Also retain all 15 transaction scenarios preceding the acceptance section. The
external application's PDF endpoint and its production rollout are separate
repositories/deployments, as the plan states.

## Backup worker

Source: [backup acceptance checklist](../design/backup-worker.md#17-acceptance-checklist).
Files below are under `internal/backup/` unless otherwise stated.

| ID | Acceptance | Evidence to review | Status / remaining gate |
| --- | --- | --- | --- |
| B-A01 | Existing configuration compatibility | `internal/config`, `worker_lifecycle_test.go` | Local |
| B-A02 | Prod/test identity and path separation | `internal/inventory`, `generation_test.go` | Local; shared VPS integration |
| B-A03 | Versioned markers and old-byte preservation | `internal/quadlet`, `internal/deployment/metadata_test.go` | Local |
| B-A04 | Read-only marked/unmarked inventory | `internal/inventory` | Local; actual stopped/absent resources |
| B-A05 | ARM64 rootless setup/reboot/idempotence | `worker_lifecycle_test.go`, `worker_integration_test.go` | External; native ARM64 deferred |
| B-A06 | Restricted credentials, no admin key in runtime | `source/admin_test.go`, `worker_trust_test.go`, `worker_units_test.go` | Local; actual SSH negative controls |
| B-A07 | WAL snapshot without production lifecycle actions | `source_safety_test.go`, `capture_locks_test.go` | Local; source integration |
| B-A08 | Empty source rejection, standalone read-only snapshot | `source_safety_test.go`, `archive_test.go` | Local |
| B-A09 | Validator compatibility and measured write impact | `source_safety_test.go`, source qualification | Partial; target load/latency evidence |
| B-A10 | Interrupted transfer/publication never succeeds | `protocol_test.go`, `archive_test.go`, `archive_reconcile_test.go` | Local; power loss remains external |
| B-A11 | Deployment locks released before slow stream | `capture_locks_test.go`, `source_safety_test.go` | Local |
| B-A12 | Missing storage/identity/discovery cannot be green | `availability_test.go`, `notification_evaluation_test.go` | Local |
| B-A13 | Retention preserves valid/foreign/leased data | `retention_test.go`, `automatic_retention_test.go` | Local |
| B-A14 | Complete filtered/paginated history | `latest_test.go`, `pull_test.go`, `retirement_test.go` | Local |
| B-A15 | Exact/latest pull while source offline | `pull_test.go`, `availability_test.go` | Local; disconnected source integration |
| B-A16 | Verified no-overwrite atomic local bundle | `pull_test.go`, `archive_test.go` | Local |
| B-A17 | Export/retention/interrupted pull races | `pull_test.go`, `retention_test.go` | Local |
| B-A18 | Native Signal and relink/update | `signal_link_test.go`, `signal_service_test.go` | Partial; verifier replacement/rebind implemented, native/live account relink qualification remains |
| B-A19 | Prompt bounded/deduplicated alerts and recovery | `notifications_test.go`, `notification_attempts_test.go` | Local; actual destination/outage observation |
| B-A20 | Vienna daily boundary and duplicate limits | `notifications_test.go`, `notification_periods_test.go` | Local |
| B-A21 | Independent Telegram/fallback preserves snapshots | `notifications_test.go`, `notification_monitor_test.go` | Local; actual fallback receipt |
| B-A22 | Independent powered-off/delivery alerts | `monitoring_test.go`, `notification_monitor_test.go` | External; armed monitor outage test |
| B-A23 | Isolated application restore or explicit absent coverage | `restore_test.go`, `verification_test.go` | Local; selected application drill |
| B-A24 | Distinct snapshot/delivery/restore/recovery status | `health_test.go`, `notification_report_test.go`, runbooks | Local |
| B-A25 | No side-effect provisioning/sends in read/dry/test | CLI dry-run tests, fake transports | Local; integration opt-ins audited separately |
| B-A26 | Request/capture-bound freshness | `notification_evaluation_test.go`, `notification_reconcile_test.go` | Local |
| B-A27 | Bounded host probes without control sockets | `health_test.go`, `notification_report_units_test.go` | Local; actual timer/mount failures |
| B-A28 | Live export lease and last verified snapshot pins | `retention_test.go`, `verification_test.go` | Local |
| B-A29 | Restricted rotation and all shared-state migrations | `worker_trust_test.go`, `shared_setup_test.go`, `notification_cutover_continuation_test.go` | Recorded; shared-owner update/recovery, rotation, activation and retained cutover continuation |
| B-A30 | Armed external checks and measured alert delay | monitoring qualification/runbook | External; configured destination required |
| B-A31 | Setup releases locks and survives disconnect | `setup_recovery_test.go`, `worker_lifecycle_test.go` | Local; real SSH/systemd run |
| B-A32 | Journal precedes preparation mutations | `setup_recovery_test.go`, `worker_lifecycle_test.go` | Local |
| B-A33 | Durable retirement and honest catch-up coverage | `retirement_test.go`, `notification_periods_test.go` | Local |
| B-A34 | Stale writers cannot erase policy/pin evidence | `state_test.go`, `generation_test.go`, `archive_reconcile_test.go` | Local |
| B-A35 | Single bounded report and exact batch members | `notification_pipeline_test.go`, provider formatting tests | Local; recipient formatting qualification |
| B-A36 | Lost-state/offline-source/read-only recovery | `archive_reconcile_test.go`, `availability_test.go` | Local; attached-disk integration |
| B-A37 | Cross-path single writer and pruning evidence | `takeover_test.go`, `retention_test.go`, `worker_integration_test.go` | Local; rootless mounts |
| B-A38 | Exact checker/snapshot contract and negative controls | `restore_test.go`, `verification_test.go` | Local |
| B-A39 | Independently copied/hashed drill input | `restore_test.go`, `verification_test.go` | Local; disposable sandbox qualification |
| B-A40 | Stable coverage and measured resource limits | `notification_evaluation_test.go`, health/maintenance tests | Partial; target workload/latency evidence |
| B-A41 | No-write export and disk-local list/pull | `availability_test.go`, `pull_test.go` | Local; actual read-only mounted disk |
| B-A42 | Safe capacity cleanup and durable pause expiry | `automatic_retention_test.go`, `pause_test.go` | Local |
| B-A43 | Separate monitor identities and stale retry suppression | `monitoring_test.go`, `notification_monitor_test.go` | Local; remote ambiguity remains visible |

## Shared notification worker

Source: [notification completion criteria](../design/notification-worker.md#17-completion-criteria).
Paths below are under `internal/notification/` unless prefixed `backup/`.

| ID | Completion criterion | Evidence to review | Status / remaining gate |
| --- | --- | --- | --- |
| N-A01 | Independently tested Signal/Telegram/LED adapters | `provider`, `led`, relay/native integration | Local managed Telegram/Signal/LED activation; live outputs remain unqualified |
| N-A02 | Producers lack provider/hardware credentials | `install`, backup report unit tests | Local |
| N-A03 | Durable queue versus acceptance/ambiguity | `worker/worker_test.go`, `worker/receipt_compaction_test.go` | Local |
| N-A04 | Output outage does not block capture | backup report/monitor tests, runtime loop tests | Local; end-to-end outage window |
| N-A05 | Freshness/retirement/subject ownership | `worker/producer_retirement_test.go`, `worker/led_clear_guard_test.go` | Local; explicit aggregate recovery now tested on published state and pinned mock daemon; regular checks passed; external and deferred race qualification remain separate |
| N-A06 | Revised/expired reports and late historical acceptance | worker tests, backup pipeline/export tests | Local |
| N-A07 | Future-basis waiting and idempotent freshness | worker tests | Local |
| N-A08 | Delayed coverage versus new outage | backup period/monitor/pipeline tests | Local |
| N-A09 | Healthy LED evidence/liveness expiry | `worker/led_runtime_test.go`, pinned daemon fixtures | Local; physical timing deferred |
| N-A10 | Recovery epoch, reconciliation and explicit ambiguity | `worker/recovery_*`, repeated managed Signal/Telegram/LED, backup repeated recovery and command ledger ancestor tests | Software implemented; external recovery qualification remains open |
| N-A11 | One Signal account owner including children | provider Signal tests, install Signal native fixtures | Implemented software; native handoff/replacement and legacy backup account adoption with CLI/child-lease/SIGKILL checks; actual providers remain unqualified |
| N-A12 | Migration semantics, interrupted handoff, tombstones | backup cutover/resolution/rollback tests, `worker/legacy_ownership_test.go` | Implemented software; Signal migration, repeated cycles/recovery and SIGKILL checks pass; live qualification separate |
| N-A13 | Exact batch ordering/coverage | backup pipeline tests, worker incident guards | Local |
| N-A14 | Durable obligations and catch-up replacement | backup export/period/administration tests | Local; unsupported legacy assertion/waiver remains explicit |
| N-A15 | Final local/integration/crash/live evidence | `Makefile release-check`, qualification records | External; no blanket qualification |
| N-A16 | Complete operator lifecycle runbooks | `examples/notification-worker`, `installation_retirement*_test.go` | Implemented software; explicit UID/socket/configuration/history retirement and replacement, retained audit and repeated recovery; external qualification separate |

The nine mandatory review scenarios in section 15 remain subcases of these
criteria. In particular, scenario 1 includes **another** migration after legacy
rollback; initialized G+1 lifecycle, scoped resolution, native ownership and SIGKILL tests now cover that sequence, including repeated cycles and Signal profiles. Batch coverage and
cross-provider isolation retain their executable scenarios. Scenario 3
requires production activation after restored ambiguity reconciliation; offline
publication/verification alone does not complete it.

## Interactive worker

Source: [interactive acceptance scenarios](../design/interactive-commands.md#11-required-tests-and-acceptance-scenarios).
These are the 15 mandatory end-to-end scenarios. Existing contract fixtures are
valuable evidence. Initialized Telegram now has managed read/all admission and
disable/drain, with immutable history and interruption tests. The current implementation-only audit is in docs/development/validation.md;
manual qualification gaps and the supported lifecycle boundaries stay explicit. Deferred external
and broad race qualification does not become a passing full acceptance result.
Synthetic risk fixtures remain explicitly
synthetic; no real vulnerability scanner is implied.

| ID | Mandatory scenario | Existing fixture families / remaining integration |
| --- | --- | --- |
| I-A01 | Two workers, exact addressed finding, forged context | `worker/context_routing_test.go`, `interaction_integration_test.go` |
| I-A02 | Changed assumptions before confirmation, replay once | `interaction/ledger_test.go`, synthetic risk fixture |
| I-A03 | Rescan admission crash, same job, no false resolution | synthetic worker fixture, command dispatch/ledger tests |
| I-A04 | Independent finding/LED, snooze and acceptance expiry | `worker/policy_health_test.go`, LED guard tests |
| I-A05 | Old gateway restore cannot repeat committed effect | `worker/recovery_commands_test.go`, command recovery commit/install tests; first managed activation and command-history adoption implemented; committed-effect restore through managed reactivation now tested; full qualification remains separate |
| I-A06 | Authorization revoked queued/in-flight, no private leak | managed policy narrowing/recovery tests, command dispatch/rejection and context routing tests |
| I-A07 | Atomic conversion identity through replay/restore/alias change | `worker/command_intake_test.go`, recovery request/command tests; managed restore-before-conversion and alias migration test |
| I-A08 | Stopped producer at acceptance expiry, no false green | policy health/LED tests; actual daemon timing qualification |
| I-A09 | Private/group/thread isolation and credential rotation | context routing/provider conversation/Signal tests; managed enrollment removal/generation migration, dual-provider credential rotation and recovered-provider transitions implemented; actual providers unqualified |
| I-A10 | Context survives receipt compaction, no forged authority | `worker/contexts_test.go`, context routing/compaction tests |
| I-A11 | Ordered results, non-admission fence and compacted floor | interaction ledger/recovery and worker dispatch tests |
| I-A12 | Subject-scoped filtering and narrowed rights | managed permission-policy lifecycle, `worker/gateway_queries_test.go`, context routing and synthetic risk tests |
| I-A13 | Late preparation closes without effect, conflicts retained | `worker/command_rejections_test.go`, interaction ledger tests |
| I-A14 | Independent supports/transitions and stale-clear refusal | policy health, LED guard and recovery LED tests |
| I-A15 | Full support snapshot plus bounded transition backlog | policy health/worker transition tests, backup reconciliation |

The area tables and design-review corrections in all plans remain required
subcases. Before converting any row to a completed acceptance result, attach the
exact test names, final revision/tree identity, environment and observed result;
record gaps within a row rather than averaging them away.
