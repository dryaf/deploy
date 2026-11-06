# Implementation plan: server inventory, managed Quadlets, and pull backups

Reconciliation/rollback update (2026-11-06): the consolidated current work list is
[IMPLEMENTATION_STATUS.md](IMPLEMENTATION_STATUS.md); it is not a measured 90%
completion claim. `backup-worker notification-resolve` now records exact,
audited unsent/retry decisions for held legacy sources. Duplicate-risk
acknowledgement is required for uncertain retries; original rows, attempts and
dispositions remain retained. Independent file/SQL evidence, stale-review checks
and canonical acceptance protect release and coverage.
`notification-rollback` can restore initialized Telegram delivery after shared
revocation and outcome reconciliation, using a new canonical `legacy-active`
generation and separate native sender units. Shared dispatch cannot use that
generation; retained canonical leases protect legacy I/O. Explicit revocation
tombstones it. Capable pinned binaries are required before cutover; uncertain
shared outcomes, missing/compacted required evidence and later generation/helper
migrations remain explicit limitations. The managed startup guard now applies
inactive-producer enforcement to verification mode, allowing admitted delivery
mode to restart with active producer generations. Native restoration pins the
reviewed Telegram credentials and fails closed on canonical identity/recovery
changes. The per-criterion evidence map is [ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md).
See [the operational guide](examples/notification-worker/BACKUP_CUTOVER.md).
Recovered Telegram/Signal, command/LED production admission, later recovery epochs
and external qualification remain open. Earlier dated checkpoints below describe
the state before this update.

Managed backup cutover update (2026-11-06): the initialized outbound-only
Telegram profile now has an executable per-producer handoff and report-service
migration. `backup-worker notification-cutover --local --review` prints pinned
legacy evidence; `--apply --spec ABS` applies/resumes the exact reviewed request.
Compatible helper/image probes, exact unit checks and retained setup, notifier,
attempt, gateway operation, installation and worker leases protect the transition.
File/SQL intent anchors retain the legacy fence. Accepted history is preserved;
explicitly reviewed unsent work is exported; ambiguous/incomplete work stays held
and degrades delivery health without starving fresh reports. Credential-free
report service/timer/path installation follows exact generation staging/activation.
`--revoke --spec ABS` drains reports, tombstones the generation and imports receipts,
retaining the legacy fence. This implements fenced rollback; releasing held rows
and restoring a new legacy generation remain separate unfinished reconciliation
work. Signal/recovered gateways and command/LED admission remain unsupported.
See [the operational guide](examples/notification-worker/BACKUP_CUTOVER.md).
Earlier dated checkpoints below describe the state before this update.

Legacy delivery evidence update (2026-11-05): the outbox notifier now durably
reserves each provider attempt and atomically commits its outcome and acceptance.
Interrupted reservations become ambiguous under an inode-pinned exclusive lease;
the existing legacy retry/fallback policy is retained. A bounded hash-linked
journal preserves exact envelopes beyond the outbox's 32-attempt summary.
`backup-worker delivery-review --local --state-dir` inspects this evidence without
settling it or granting migration authority. Attempted sources cannot silently
become fresh shared exports or batch members. Older helper evidence remains
incomplete; manual test sends are outside this journal. Per-producer generation
activation, legacy outcome reconciliation, helper capability enforcement and
report-service cutover/rollback remain unfinished. This supplies a prerequisite
for the next managed backup cutover transaction, not a completed migration.

Delivery lifecycle update (2026-11-04): `activate-delivery --local --root
--manifest-hash --qualification-hash` now journals dispatcher activation for
initialized outbound-only fixed-recipient Telegram workers. Exact fresh
qualification, runtime/helper capabilities, file/SQL ownership, loaded manager
and process/socket pins guard activation. Admission is checked at startup,
periodically and immediately before sending. `deactivate-delivery` restores
verification without requiring live credentials or unexpired qualification;
explicit cancellation can supersede an interrupted activation. Ownership survives
updates, compatible rollback and uninstall. Producer generations stay inactive;
per-producer admission, recovered/Signal runtime handoff, command/LED activation
and legacy backup cutover remain unfinished. Native executable and SIGKILL
fixtures use simulated managers and synthetic credentials, not live qualification.
This supersedes older statements that every production lifecycle transition is
unimplemented. See the [delivery guide](examples/notification-worker/DELIVERY_ACTIVATION.md).

Outgoing Signal profile update (2026-11-04): `migrate-signal-profiles --local
--spec` now journals a proven outgoing transport transition for an already
supervised recovery verification gateway. Canonical configuration and recovery
hashes remain unchanged; a separately frozen effective runtime and profile proof
bind the new socket/inbox pins and affected route revisions. Existing queued
routes and Telegram evidence are retained. Runtime health exposes the profile
hash and periodic audits stop the gateway on proof loss. Capability checks,
interruption/SIGKILL fixtures and native-process tests cover this verification-only
transition. Production activation, Signal runtime handoff, legacy fixed-recipient
conversation enrollment and backup cutover remain unfinished. This supersedes
older checkpoint statements that all outgoing profile migration is unavailable.
See [the profile migration guide](examples/notification-worker/SIGNAL_VERIFICATION.md#outgoing-profile-migration).

Activation admission update (2026-11-04): `activation-review --local --root`
now composes the pinned verification revision, latest qualification, local provider
identity, recovered request seals/cursors and current Telegram handoffs into a
read-only prerequisite inventory. Worker inspection requires an inactive gateway
and exclusive installation/worker leases; it performs no shutdown, polling, repair
or fence changes. Managed Signal verifier health is checked separately from
outbound profile pins; stale/legacy pins require an explicit migration. A saved
review grants no authority. Production activation, command admission, Signal runtime
handoff/profile migration and backup cutover remain unfinished. SQLite inspection
refuses nonempty WAL/journals without checkpointing or ignoring their contents.
See [the admission review guide](examples/notification-worker/QUALIFICATION.md#activation-admission-review).

Managed Signal verifier update (2026-11-04): native service setup, resume,
inspection and uninstall now preserve an immutable endpoint/service binding.
The gateway can adopt a supervised endpoint directly or upgrade its existing
foreground binding with `supervise-signal`; the unit gains explicit dependency
and ordering. Capability, loaded command, process/Unix-peer identity and health
pins constrain startup. Account/intake/command fences remain closed. Local
lifecycle, crash and real-process fixtures cover the implementation, with a
simulated manager; actual user-systemd/ARM64 qualification remains open. Service
binary upgrade/re-enrollment and later recovery epochs still need explicit
transitions. Legacy backup-relay/backup cutover and production admission are not
completed by this step. This supersedes older checkpoint statements below that
endpoint supervision is unimplemented. See the
[service guide](examples/notification-worker/SIGNAL_VERIFICATION.md#managed-endpoint-service).

Managed Signal configuration update (2026-11-03): the shared notification
installer now adopts the exact verification endpoint configuration into the
recovered gateway with crash resume, health pins and retained Telegram evidence.
The separate Signal verifier retains account/inbox ownership. Gateway readiness
ends if that verifier fails. This grants no backup delivery/source/command
authority; endpoint supervision, legacy relay/backup cutover and production
activation remain unfinished.

Signal endpoint update (2026-11-03): the shared notification implementation can
now publish and run a separate health-only verifier for an exactly completed
account/inbox recovery. It retains the account and both inbox leases and stages a
pinned gateway configuration without applying it. No backup source, delivery or
command authority is granted. Managed service/configuration cutover, legacy relay
adoption and production activation are still unfinished.

Signal account fence update (2026-11-03): recovery now has an internal service
coordinator that persistently blocks ordinary account consumers, including the
shared legacy relay entry point and linking maintenance. It checks service and
account identity, surviving CLI leases and independently retained installation
results. This is composed with the published gateway/inbox recovery under the
gateway lease. A native notification-relay user-systemd adapter now connects
this to the `recovery-signal-handoff` CLI with loaded-command, executable and live
account/inbox-lease checks. It does not support the backup Podman relay or switch backup
ownership/start a receiver. The new rootless Podman adapter supports the shared
interactive notification Quadlet, with real container/mount/flock checks and
private-store tests. Adopting the legacy backup relay, managed backup cutover and
production activation remain open.

Managed Telegram integration update (2026-11-03): `renew-telegram` now stops the
adopted gateway verification service, freezes and journals its drain request and
restarts verification with an independently audited completion. Exact crash resume
and a same-schema `upgrade-helper` path are implemented; health identifies the
renewal. This grants no backup command/delivery authority. Production admission,
backup cutover and Signal service handoff remain unfinished.

Telegram renewal integration update (2026-11-03): the gateway now retains new
provider-drain observations in a separate journal tied to the original publication
and exact predecessor. Recovery retries and provider ACK ordering are tested;
the CLI can inspect the latest completed renewal. Canonical command mappings,
backup delivery and execution fences remain unchanged. Managed local Telegram
shutdown/restart is implemented above; production cutoff installation and
backup/notification activation remain open.

Telegram gateway handoff inspection update (2026-11-03): the notification CLI
can verify current freshness of an original published Telegram handoff without
polling or changing retained evidence. This grants no backup command/delivery
authority; renewed handoff evidence is implemented, while managed production
cutover remains unfinished.

Signal gateway recovery integration update (2026-11-03): the notification CLI
can now install a relay runtime baseline tied to the exact published gateway
cursor, retaining source history and keeping both ordinary relay starts fenced.
This is verification-only infrastructure; it grants no backup delivery or command
execution and does not complete managed backup cutover or provider activation.

Managed gateway recovery verification update (2026-11-03): the notification
installer can now adopt a completed publication into its existing verification
service, binding the new identity and exact publication commit. Runtime restart,
update and uninstall preserve the proof and quarantine. It grants no backup
source, delivery or command authority; managed backup cutover and production
activation remain unfinished.

Canonical gateway publication update (2026-11-03): `PublishRecovery` and
`recovery-publish` now install the reviewed gateway database with a journaled,
atomic exchange and exact crash resume. Backup composition tests publish completed
notification handoffs while preserving source delivery fences; incomplete
interactive intake is refused before freezing. Managed service cutover,
production activation and subsequent recovery epochs remain unfinished.

Historical gateway image verification update (2026-11-03): backup handoff tests
now compose the public `VerifyRecoveryPublication` API for notification-only and
interactive workers. It checks the completed image and retained historical proof
without current canonical state, preserving source receipts and execution/delivery
fences. Final publication and managed service cutover remain unfinished.

Gateway publication image update (2026-11-01): the public
`PrepareRecoveryPublication` API and local `recovery-publication-prepare` command
now seal a reviewed gateway database image with its historical proof anchor.
Backup handoff composition covers notification-only and interactive workers while
preserving source receipts, review hashes and delivery/execution fences. The
canonical database is unchanged. Final gateway publication, ancestor-chain
handling, activation and managed backup service cutover remain unfinished.

Historical gateway verification update (2026-11-01):
`notification-worker recovery-checkpoint-verify` now checks retained recovery
inputs and returns a deterministic proof for an exact checkpoint. Backup handoff
tests compose the public verifier for both notification-only and interactive
workers. Verification preserves source fences and has no provider/domain side
effects. Final gateway publication and managed service cutover remain unfinished.

Shared recovery evidence update (2026-11-01): gateway checkpoints now retain
independent ownership and administrative proof files alongside the recovered SQL
projection. Backup handoff tests compose the public checkpoint API for both
notification-only and interactive workers and verify that source evidence and
fences remain unchanged. Current-epoch proof capture is implemented; final gateway
publication, ancestor-chain integration and managed service cutover remain pending.

Shared recovery checkpoint update (2026-11-01): `notification-worker
recovery-checkpoint` now captures the reviewed gateway SQL projection while
retaining delivery/command fences and exact byte/logical digests. It includes
committed WAL contents without changing source receipts or ownership. This is a
publication prerequisite; final gateway publication, ancestor evidence handling
and managed backup service cutover remain unfinished.

Shared recovery review update (2026-11-01): `notification-worker recovery-status`
now recognizes the backup producer ownership/generation handoff, retained delivery
receipts and installed command outcomes under a read-only offline review. It keeps
pending stages and inherited fences visible and does not acknowledge receipts,
execute commands or activate reporting. Native backup handoff tests exercise the
review. Final publication and managed backup service cutover remain unfinished.

Shared recovery preservation update (2026-11-01): notification preparation now
seals the original gateway database, including committed WAL contents, into a
private archive with independent file and SQL proofs. Backup handoff and LED
cleanup tests use this preparation path. Archived evidence keeps its original
identity; source/receipt/command fences remain intact. This closes the source
preservation prerequisite, while final gateway publication, safe repeated recovery
and managed backup service cutover remain unfinished.

Local recovery handoff update (2026-11-01): `backup-worker recovery-handoff` now
joins completed notification reconciliation to canonical ownership/generation
handoff and the source delivery identity/cursor commit. Interactive workers also
reconcile their retained command ledger, commit its new identity and install the
proved outcomes without replaying a domain effect or granting execution. One
gateway lease and the stable source command lease span all stages; archive,
operation and reviewed-summary pins prevent cross-installation handoff. Both
notification-only and interactive paths support crash retry. Old history and
delivery/command fences remain; managed runtime/service handoff, final recovered
publication and shared production cutover are still unfinished.

Local producer recovery update (2026-11-01): `backup-worker recovery-notifications`
now reconciles retained messages/receipts and records the immutable summary using
an exact notification recovery operation pin. It holds the offline worker lease,
fences producer reporting and supports crash resume and historical replay without
renewing evidence. `notification-worker recovery-prepare` can prepare/resume the
verified snapshot quarantine and returns that operation ID. Native executable and
SIGKILL tests cover these paths. Neither command performs command reconciliation,
active cursor/ownership publication, provider delivery or service activation.
Final publication, managed orchestration and shared cutover remain unfinished.

Local notification LED recovery update (2026-11-01): explicit
`backup-worker recovery-led-evidence` and `recovery-led-clear` commands now connect
the backup evaluator and exact guarded cleanup to the notification recovery
workflow. They require completed producer reconciliation and exact review hashes.
Cleanup imports only completed receipts, preserves uncertain outcomes and retains
source leases during the Clear. Exact crash retry does not resend a clear or renew
evidence. Native CLI/pinned mock-daemon and process-kill tests cover the workflow.
No service, source host, delivery generation or recovery publication is activated.
Remote orchestration, worker aggregate recovery and managed shared cutover remain
unfinished.

Shared LED cleanup import update (2026-11-01): the notification worker can now
retire an exact inherited named-binding fault through its committed, independently
verified domain-authorized cleanup receipt. It preserves the original incarnation
in the epoch archive, other fault rows and the complete cleanup/import audit.
This does not re-evaluate or waive backup incidents, issue another clear, grant
healthy output or activate reporting. Managed backup cleanup orchestration,
worker-wide reconciliation, final publication and shared cutover remain unfinished.

Shared LED epoch update (2026-11-01): `recovery-led-handoff` now archives original
notification LED operation/mapping history and starts new-epoch journals inside
quarantine. Adopted backup faults retain their exact IDs and ownership evidence;
the handoff grants no cleanup, backup resolution or activation authority.
Worker-wide reconciliation, final publication and managed backup cutover remain
unfinished.

Shared inherited-LED update (2026-11-01): notification runtime primitives now
verify old fault ownership through immutable adoption/preservation/inventory
proofs. A fresh ordinary backup status cannot discharge an inherited fault; normal
cleanup skips it and output remains critical/unknown. Canonical and database
identity, UID, layout and history are checked. This does not publish recovery or
complete managed backup cutover; worker-wide reconciliation remains pending.

Shared producer retirement update (2026-11-01): the notification worker now has
an explicit local permanent producer-epoch retirement operation. It requires
retired source subjects, settled deliveries/commands, acknowledged receipts and
resolved LED cleanup, plus revoked generations. It preserves backup receipt and
message history and cannot waive backup incidents or count cancellation as
acceptance. Source-host decommissioning, configuration removal and managed shared
backup cutover remain unfinished. See the notification administration guide.

Shared LED configuration update (2026-11-01): the notification worker's managed
verification lifecycle now supports LED-binding-only migration with inactive
producer ownership, pinned configuration and crash-resumable mapping publication.
Existing backup fault/cleanup records on changed bindings must be resolved first;
configuration changes cannot waive backup incidents. This does not implement
shared backup cutover, report service installation or recovered-state publication.

Shared recovery fencing update (2026-11-01): an absent backup producer can now be
fenced in the notification worker's new recovery epoch without fabricating backup
evidence. The normal backup summary/ownership/generation handoff can subsequently
complete that pending transition. Old generations and their history remain revoked;
independent producers can reconcile separately. Final publication and managed
shared-backup cutover remain outstanding.

Shared-notification schema update (2026-11-01): the managed verification worker now
supports an explicit schema 9 → 10 migration with retained original DB and recovery
helpers. Producer identity/generations, queued reports and receipt history remain
unchanged. This does not activate shared reporting, migrate the backup database or
complete the remaining legacy/shared cutover and rollback coordinator.

Shared recovery adoption update (2026-11-01): `recovery-led-adopt` now merges the
preserved original/snapshot LED fault union into the quarantined reconciliation
ledger, retaining old IDs/epochs and historical cleanup evidence. Executable cleanup
flags are reset, and a separate domain-authorized cleanup remains valid across the
merge without recreating its cleared state. Exact retries are crash-resumable.
This does not waive backup incidents, publish recovery, complete normal-runtime
LED acceptance or implement the remaining managed shared-backup cutover.

Shared recovery preservation update (2026-11-01): the notification quarantine can
now freeze both source LED fault ledgers into one independently journaled record.
Post-snapshot backup faults and old cleanup flags retain their original identities.
This neither waives backup incidents nor grants LED cleanup authority; fresh backup
domain evidence is still required. The record is preparation for publication,
not managed backup migration or activation. See `recovery-led-preserve` in README.

Signal compatibility update (2026-11-01): `backup-worker signal-link` now invokes
an account-fenced deploy entry point in the pinned relay image instead of bypassing
the lease with a direct CLI launch. Both backup and shared relays refuse startup
while its bounded link journal is incomplete/uncertain. Explicit status/review
commands, child inheritance and crash tests are implemented. Backup linking now
publishes a durable service gate, stops its enrolled relay and restores its captured
activity through a bounded, crash-resumable journal. Exact configuration/credential/
image/service binding and notifier ownership are preserved. Default retries retain
the last operation even after completion; `--new-link` or a fresh explicit
`--operation` requests a new link. Setup/update/recovery cannot bypass unfinished
maintenance. Old images without
maintenance protocol version 2 fail the isolated preflight probe. Shared service
installation/account handoff and live linking qualification remain unfinished.

Shared-notification qualification update (2026-11-01): the managed notification
CLI now stores dated operator evidence for each enrolled producer's generation
cutover, receipt reconciliation and report monitoring, plus route/provider/host
checks. Exact installed revisions and local inputs are pinned, incomplete/failed
observations remain visible, and no report grants activation. This evidence
workflow does not implement the remaining shared backup installation/cutover or
perform live external-monitor qualification. See the notification qualification
guide in `examples/notification-worker/QUALIFICATION.md`.

Shared-notification maintenance update (2026-11-01): bounded acknowledged
intermediate receipt compaction retains final acceptance evidence, producer cursor
floors and immutable events. Managed executable rollback preserves the notification
queue, current schema/configuration and helper, and remains verification-only.
These controls do not perform the still-unfinished shared backup cutover.

Shared-notification administration update (2026-11-01): offline reviewed retry/
cancel commands now preserve an independent durable audit under notification
schema 10. Real backup receipt-import tests show that administrative cancellation
leaves the producer obligation open, creates a replacement event and satisfies the
obligation only after replacement acceptance. Replaying the administrative decision
does not change that replacement. These controls neither waive backup incidents nor
complete the shared-mode lifecycle migration. Schema 10 also requires compatible
runtime/recovery helpers; the explicit shared-worker schema 9 → 10 upgrade is
implemented, while other managed migrations remain unfinished.

Shared-notification installation update (2026-11-01): the notification worker now
has a managed **local verification-only** setup/update/resume/uninstall lifecycle.
It preserves pre-initialized inactive state and pins configuration, binaries,
identity and the running revision. Crash recovery is tested with interrupted
processes and a temporary native worker. Queue/receipt inspection is available
without advancing producer acknowledgement floors. This does not install or
activate the backup shared exporter/report service, transfer legacy notification
ownership, complete restored LED adoption or qualify production delivery.

Shared-notification recovery update (2026-11-01): the internal backup coordinator
now supplies challenged LED domain evidence to an already quarantined notification
worker. It reuses the archive/incident/host-probe evaluator while delivery remains
fenced, atomically freezes each new status sequence and report, and records the
worker acknowledgement separately. Retries preserve the original evidence; a new
inventory round evaluates newer failures. Qualification, stale host evidence,
pause/run state and retirement retain their existing conservative semantics.
The evidence exchange does not send messages, advance delivery cursors, clear LEDs
or grant command execution. A separate cleanup adapter now retains both source
leases and validates the exact acknowledged report plus fresh backup domain state
before each journaled exact-ID clear. Its speculative evaluation always rolls back.
Changed incidents, pause/run state, qualification, stale probes and clock changes
block dispatch. Lost replies never authorize automatic replay. Worker tests cover
seven SIGKILL boundaries and the real pinned daemon with mock hardware; backup
integration covers source drift and lease contention. Managed recovery orchestration,
restored LED ownership adoption, shared
report-service installation and production activation remain pending. Tests use
real local recovery/SQLite code and a fake registry, including six SIGKILL cases;
they do not establish live provider, hardware or ARM64 runtime qualification.

Status: the six issues from the 2026-10-31 critical review are corrected;
the full plan is still not release-qualified. Later verification failures now
invalidate current health and survive catalog reconstruction; delivery catch-up
has an independent outbox kind and repairs older covered rows; interrupted updates
have an independently installed recovery supervisor and state-access gate;
monitoring compares actual check URLs; pause/resume and expiry persist catch-up
requests through failed starts and incomplete runs; interrupted first generation
enrollment resumes without changing identity or accepting a conflicting ledger.

The four findings from the follow-up review are also corrected: retention consults
both archive and local verification failures before deleting history; interrupted
JSON cleanup recognizes the actual atomic-write filenames; source SSH trust is
qualified in staging and published with durable rollback before-images; and clock
uncertainty blocks manual and scheduled retention while preserving diagnostic
checks. Regression coverage includes a database that passes `quick_check` but
fails `integrity_check`, missing verification acknowledgements, interrupted
receipt/spool cleanup, trust-file drift and legacy ownership, rollback of new and
existing trust files, and clock correction followed by normal retention.

The next four review findings are corrected as well: pull/export refuse recorded
checksum or integrity failures without substituting an older snapshot; a complete
reconciliation retains missing catalog entries as unavailable, and the independent
health probe checks the latest files on disk; completed rotation journals allow a
new uninstall transaction; and retirement closes scoped incidents explicitly as
retired-by-operator, supersedes obsolete alerts, and preserves unrelated batch
members. Notification passes resume interrupted retirement cleanup. Regression
tests cover these cases, restored bundle availability, quick checks that cannot
erase integrity failures, concurrent reader/control leases, unfinished lifecycle
exclusion, and idempotent uninstall retries.

The latest project review also corrected local/remote database retention that
validated deletion candidates but not retained copies; missing-newest fallback in
`--latest`; source capture children outliving deployment locks; and verification
leases being misclassified as missing bundles. Publication intent now preserves
per-generation capture identity in archive control before the bundle rename;
uncertain publication requires explicit ID selection. Legacy catalog migration
includes missing rows but never infers completeness from a reconstructed catalog.
Capture watchdogs and native SQLite inherit the export/service/target descriptors,
and closing a parent's copy does not unlock children. Health availability checks
use the short index lease, independently of the long verification lease.

The four findings from the subsequent module review are corrected: automatic
retention requires a fresh successful publication for that exact active instance
in the latest finished run, with matching configuration, binding, generation, and
capture identity. A per-generation claim is checkpointed before deletion and may
be used only once, including after interrupted pruning or local-state loss.
Failed sources preserve history while successful scopes can prune; explicit
scoped operator pruning remains available without a new capture. Scheduler leases
now wait through bounded notifier contention, including when committing a timed-out
run, while preserving pause/heartbeat ordering. Independent health includes the
maintenance timer and service with a bounded manager-probe budget. The release
target now requires the isolated backup-worker suite as well as the deployment
and VM suites; this repository does not contain a CI workflow enforcing that gate.

Validation for these four fixes passed: targeted regression tests with the race
detector, the full `make test` unit/race/vet suite, Linux ARM64 and macOS ARM64
cross-builds, and the expanded AMD64 rootless Podman suite. Real user systemd
confirmed that idle maintenance is healthy, a disabled maintenance timer is
unhealthy, and a failed maintenance service is unhealthy. The existing capture,
SIGKILL recovery, SSH trust, isolated validator, and lifecycle checks passed too.
The VM crash matrix was not rerun for this change; native ARM64 and live provider
qualification remain outstanding. Cross-builds do not provide runtime qualification.

The added regression tests passed with the race detector, and the full unit/race/
vet suite passed. Expanded AMD64 rootless Podman tests passed for killing either
capture supervisor, closing parent lock descriptors while SQLite remains alive,
and completing a real snapshot before releasing locks for streaming. The native
SQLite watchdog ended lingering work and all three locks became available. These
checks use the production capture/SQLite functions and do not replace full
source-to-worker-to-restore, VM power-loss, or live ARM/provider qualification.

Local evidence includes regression tests for both reviews, a real SIGKILL of an
installer child, failed-restart retry, and exclusion of live state readers during
installation. `make test-backup-worker-isolated` provides a dedicated disposable
rootless Podman/systemd recovery test using the production helper. That test passed
on AMD64 after the follow-up fixes: real SSH rejected staged candidate trust while
the live connection remained usable; automatic recovery restored configuration,
SSH trust, helper, archive control, state policy, and the previously enabled/active
backup timer. The production bubblewrap SQLite self-test also passed inside that
environment. The harness uses
its own temporary Podman store and generated credentials; it sends no provider
messages and does not operate on existing containers.

The container harness also exercises uninstall after a completed rotation journal
through the production lifecycle entry point and real user systemd, checking that
timers stop, owned files are removed, archives remain readable, and retries work.
This added lifecycle check passed on AMD64. Its fixture supplies source-revocation
acknowledgements; it does not prove a complete real rotation or source revocation.

The full unit/race/vet suite and focused backup regressions passed; these are local
checks, not acceptance proofs. Static Linux AMD64/ARM64 and Darwin ARM64 builds
passed as well; compilation is not hardware qualification. The container test
covers update interruption, SSH trust isolation, and the validator, not the
complete source-to-worker-to-restore flow. Full setup, source
capture, notifier/provider orchestration, and the complete backup pass still need
integration qualification. No backup-specific VM reboot/power-loss/storage test
has run; the existing deployment VM tests do not exercise the new worker. The user
deferred live ARM qualification. Phase-0 hardware/provider proofs and phase-7
rollout remain pending. No acceptance checkbox is marked complete from these
checks; no real source/worker was provisioned, real notification sent, or image
published. See `examples/backup-worker/README.md` for commands and handoff.

Implementation decisions:

- V1 calendar syntax is `hourly`, `daily`, or `weekly`. Hourly execution uses UTC;
  daily reports use the configured civil timezone.
- Initial setup and updates stage disabled timers; `qualify` records an explicit
  live report and `activate` requires that report plus a recent manual backup.
- Linux AMD64/ARM64 use static helpers and a credential-free bubblewrap validator.
  Existing macOS CLI commands remain buildable; local archive operations and
  isolated pulls currently require Linux.
- Source profiles require ordinary SQLite, a dedicated host bind, the standard
  rootless namespace, and a matching numeric OCI user (explicit UID:GID for
  non-root). Unknown overrides, drop-ins, or ownership evidence fail closed.
- Filesystem identity is mandatory for writes; read recovery can inspect an
  explicitly selected copied archive. Writer takeover requires recovered control
  and `setup --recover --fenced`, drains archive leases, and advances its epoch.
- Source generations are explicitly changed with `source-reconcile` and accepted
  by the worker with `accept-generation`; neither discovery nor a missing record
  silently creates a replacement identity.
- Heavy maintenance uses the archive writer lease and a 25-minute admission
  budget before the next backup. Deferred work remains pending; application
  checks require an operator-supplied pinned checker and negative controls.
- Build definitions require actual caller-selected image digests and immutable
  package snapshots. They do not claim that an unpublished image exists.

Prepared on 2026-10-31. This plan extends the existing deployment tool and preserves the image-deployment plan in `PLAN.md`.

Seven critical design reviews completed on 2026-10-31; findings and required implementation proofs are recorded in sections 19–25. Design fixes are incorporated below, but remain unverified implementation requirements until their tests pass. Later review findings are applied to the normative sections rather than left as competing alternatives.

## 1. Outcome and decisions

The intended user experience is:

```sh
deploy release prod
deploy ls vps-main
deploy backup-worker setup home-backup
deploy backup-worker status home-backup
```

Stored backups are also accessible from the operator's machine:

```sh
deploy backup-worker list home-backup
deploy backup-worker pull --backup <backup-id> --output ./restore-test home-backup
```

`server.yaml` defines machines, SSH connections, infrastructure, and worker roles. Application `deploy.yaml` files reference a server by name and define environments, services, database paths, and backup opt-in. Test and production may share a server and deployment user, provided their services and data paths are distinct.

The Raspberry Pi pulls backups over SSH. It runs a rootless Quadlet container for each scheduled backup pass. Its timer, persistent configuration, credentials, archive, and notification state survive container replacement. The laptop is not part of scheduled operation.

Decisions for the first production release:

| Concern | Decision |
| --- | --- |
| Discovery | Versioned JSON comments in generated Quadlet source files; shared inventory implementation for `deploy ls` and backup discovery. |
| Backup selection | Explicit per-application opt-in plus the worker's explicit list of source servers. |
| Database scope | One ordinary SQLite database file per application environment, reachable through a host bind mount. |
| Source access | Dedicated restricted SSH key per worker/source pair, under the deployment user's account; versioned export helper, no persistent source agent. |
| Scheduling | Hourly backup by default, implemented by a host systemd timer activating a finite-lived container. |
| Archive | Immutable snapshot bundles, SHA-256, SQLite validation, atomic publication, persistent receipts. |
| Notification preference | Signal first; optional Telegram fallback or standalone Telegram provider. |
| Successful runs | One daily summary at 09:00 Europe/Vienna by default; no message for each hourly success. |
| Failed runs | Immediate event on first observed failure, bounded retries, deduplication, six-hour reminders while unresolved. |
| Recovery | One explicit recovery event per incident by default; configurable separately from daily success. |
| Pi offline detection | External missed-run monitor; this cannot be provided by software running only on the Pi. |
| Safety | Backup operations never restart the application, replace production data, or perform deployment recovery. |
| Compatibility | Existing inline SSH application configurations, single-server files, and `deploy db backup` remain supported. |

Hourly snapshots imply potential data loss since the last completed snapshot, not point-in-time recovery. A two-hour freshness threshold is an alert threshold, not a guarantee of an hourly recovery point during failures. Restore time depends on database size, transfer speed, application dependencies, and the restore procedure; measure it in restore drills.

## 2. Existing code and integration constraints

Build on these components instead of implementing a second deployment or database system:

| Existing component | Relevant behavior and required adaptation |
| --- | --- |
| `internal/config/` | Strict YAML decoding, environment normalization, database validation. Add inventory resolution before existing validation; preserve strict unknown-field handling. |
| `internal/deployment/server.go` | Currently one server and Traefik provisioning. Separate server selection from provisioning so a backup-only host does not need a Traefik stack. |
| `internal/quadlet/` | Application renderer, release labels, infrastructure and maintenance templates. Introduce one metadata encoder used by all container renderers. |
| `internal/deployment/db.go: downloadRemoteSnapshot` | Creates a SQLite `.backup` through SSH and validates the download; currently holds deployment locks through streaming. Extract snapshot creation and bounded transfer responsibilities. |
| `internal/deployment/remote_guard.go`, `internal/deployment/transaction_common.sh` | Shared service/target locks and refusal of unfinished recovery. Reuse these contracts; the current guard permits absent ownership records, so the new exporter must add positive proof of managed ownership. |
| `internal/deployment/off_host.go` | Existing client-mediated off-host transfer and application restore check. Preserve its CLI while sharing bundle/verification primitives. |
| `internal/deployment/backup_inventory.go`, `internal/safefs/` | SHA-256 and private atomic JSON publication. Extend with separate versioned worker receipts; do not silently reinterpret existing receipts. |
| `internal/deployment/retention*.go`, `internal/deployment/retention.sh` | Existing managed-inventory deletion rules. Reuse policy concepts, not release-specific inventory assumptions. |
| `internal/transport/`, `internal/process/`, `internal/deployment/operation_limits.go` | Strict host-key checking, SSH, subprocess deadlines. Restricted worker SSH needs its own transport configuration, without administrator agent/config inheritance. |
| `internal/cli/main.go` | Per-command parsing needs extension, plus a global inventory path option. Avoid ambiguous positional server/environment selection. |
| `Makefile` | Existing Go, isolated Podman, and VM-crash checks. Extend the release gates for the new paths. No `.github` directory was present during planning; do not assume a working CI release workflow already exists. |

The current application normalizer checks direct `host`/`user` fields and compares some input fields before normalization. Inventory resolution must produce a complete `Environment` before those checks. Tests must catch accidentally validating the unresolved copy. Adding slices/maps to `DatabaseConfig` also affects existing whole-struct comparisons; replace those comparisons if the type ceases to be comparable.

No repository change in this planning task provisions hosts, installs keys, publishes images, or sends messages.

## 3. Configuration contract

### 3.1 Server inventory

The following v2 configuration is accepted by the CLI:

```yaml
version: 2

servers:
  vps-main:
    host: vps.example.com
    user: deploy
    ssh_port: 22
    ssh_key: ~/.ssh/vps-admin
    ssh_known_hosts: ~/.ssh/known_hosts
    # Existing stack settings may be configured here.

  home-backup:
    host: raspberrypi
    user: backup
    ssh_key: ~/.ssh/pi-admin

    backup_worker:
      sources: [vps-main]
      storage:
        path: /mnt/backup/deploy
        require_mount: /mnt/backup
        min_free_bytes: 1073741824
      schedule: hourly
      timezone: Europe/Vienna
      freshness: 2h
      max_run_time: 40m
      retention:
        keep_last: 48
        max_age: 720h

      notifications:
        primary: signal
        fallback: telegram
        failure: immediate
        repeat_after: 6h
        recovery: true
        daily_at: "09:00"
        signal:
          account_file: /home/backup/.config/deploy/secrets/signal-account
          recipient_file: /home/backup/.config/deploy/secrets/signal-recipient
        telegram:
          token_file: /home/backup/.config/deploy/secrets/telegram-token
          chat_id_file: /home/backup/.config/deploy/secrets/telegram-chat-id

      monitoring:
        backup_ping_url_file: /home/backup/.config/deploy/secrets/backup-heartbeat
        daily_delivery_ping_url_file: /home/backup/.config/deploy/secrets/report-heartbeat
```

Only configure Telegram when it has actually been set up. Signal-only and Telegram-only configurations are valid. `fallback` must name a different configured provider; no silent change of provider if Signal setup fails.

Path semantics must be explicit:

- Server SSH key and known-host paths are local to the operator running the CLI. Resolve relative paths against the inventory file's directory and `~` against that operator's home.
- Worker storage and notification secret paths are on the worker host. Require absolute paths; do not expand them on the laptop.
- Setup creates separate worker/source SSH identities on the Pi. Administrator key paths in the server inventory are never copied into runtime configuration.
- Setup normalizes host paths to fixed container mount paths, such as `/archive`, `/state`, `/config`, and `/secrets`. User configuration never needs to know those internal paths.
- The backup mount is enrolled by filesystem identity and an archive UUID, not merely by an existing directory name. Setup can capture these values; they belong to installation state rather than requiring manual YAML entry.

Defaults: hourly, Europe/Vienna unless explicitly set, freshness 2h, run deadline 40m, sequential sources/instances, one worker per backup host in v1. Validate supported calendar syntax, IANA timezone, positive durations, counts, and bounds. Freshness must exceed the ordinary schedule interval plus allowed execution delay. For unusual calendar schedules, require an explicit freshness policy and show computed next occurrences.

Use one typed policy shared by generated configuration, status, and enforcement. V1 exposes `max_snapshot_bytes` (default 10 GiB) and an optional `timeouts` mapping: `connect: 10s`, `inventory: 30s`, `snapshot: 5m`, `transfer: 10m`, `verify: 5m`, `shutdown_grace: 10s`. Each operation is bounded by the smaller of its budget and the remaining run budget; the source independently enforces its own capped snapshot/export budgets. Default retry offsets are measured from the failed attempt, but waiting must not block other eligible instances. Validate these fields before setup, and show when the selected limits cannot accommodate the measured database size/link throughput. No phase may reuse the existing release-wide `sshBudget`, which is much longer and includes unrelated build/pull work. Notification requests have an independent 10-second timeout and 45-second evaluation-pass budget so backup slowness cannot block alert delivery.

`keep_last: 48` and `max_age: 720h` retain all snapshots within 30 elapsed days AND at least the newest 48 valid snapshots. At hourly cadence this is approximately 720 snapshots per database, not 48. Setup estimates capacity using that interpretation. Duration-based retention uses elapsed UTC time, independent of DST.

### 3.2 Application configuration

```yaml
app_name: foodlog

environments:
  prod:
    server: vps-main
    target_dir: /home/deploy/apps/foodlog-prod
    database:
      driver: sqlite
      source: data/app.db
      backup:
        enabled: true
    quadlet:
      service_name: foodlog-prod
      # Existing image/build/routing settings remain unchanged.

  test:
    server: vps-main
    target_dir: /home/deploy/apps/foodlog-test
    database:
      driver: sqlite
      source: data/app.db
      backup:
        enabled: false
    quadlet:
      service_name: foodlog-test
```

Rules:

1. Resolve `server` to connection fields only. Keep target directory, deployment mode, database, runtime settings, timeouts, and application storage policy in `deploy.yaml`.
2. Reject `server` combined with inline connection fields, even if their values happen to agree. Presence-aware parsing is required for explicitly empty/zero fields.
3. Default inventory is `server.yaml` adjacent to the selected application file. Server-only commands use `./server.yaml`. Global `--servers <path>` overrides this explicitly; do not search parent directories or merge hidden global inventories.
4. Reject missing/unknown server references before remote operations. Referenced environments require a v2 inventory; legacy single-server files remain usable by legacy server commands until explicitly migrated.
5. Keep existing inline environments working without an inventory file. Mixed legacy and referenced environments in one application file are allowed, but the same environment cannot use both forms.
6. Require explicit user names for new v2 entries; do not inherit the legacy default of `root`. V1 backup sources and workers must be rootless. `deploy ls` may list rootful inventory when explicitly run as root, but it does not combine users.
7. Duplicate service names or target directories on the same resolved SSH account are errors where visible locally; existing remote ownership locks remain the final cross-project check. Shared volumes and duplicate database bindings must also be reported.
8. Backup opt-in without a valid SQLite source is invalid. Omitted backup config defaults to disabled and does not affect existing release-time database protection.
9. Distinct prod/test directories, service names, router domains, explicit container names, aliases where necessary, and host ports remain application responsibilities. Backups never infer production from an environment's spelling.

### 3.3 Migration and naming

Provide an explicit migration command or documented generated diff for single-server `server.yaml` to v2; never rewrite it during a release. Preserve the existing stack and connection values. Legacy `deploy server provision` remains valid for a legacy file; v2 provisioning requires a server name even if there is only one entry.

Inventory names are operator-facing aliases. The exporter enrolls a random persistent source UUID per source deployment account. Setup pins that UUID, the SSH account, and approved SSH host keys. The archive key is `(source UUID, service name, database generation)`; labels retain the inventory alias, app, and environment.

This refines the initial `server-name/service` sketch: renaming an inventory alias must not lose history, and replacing a VPS must not silently inherit the previous machine's success history. A service rename starts a new history in v1. Changes to target directory, database path, application/environment binding, or a rebuilt source require explicit reconciliation and a new database generation where appropriate. Keep old archives accessible. Do not concatenate arbitrary remote names into local filesystem paths; use validated IDs and encoded/hash-based components.

Duplicate aliases resolving to the same enrolled account are reported and rejected as duplicate worker sources. Explicit reconciliation is required to accept a new source UUID or changed SSH host key. A copied source identity can exist after a disk clone: detect conflicting enrolled endpoints and do not claim UUIDs prove machine authenticity.

Database-generation authority lives on the source in a small helper-owned record, not in the Pi's guess about a filename. On the first authorized export of a newly discovered service, the helper creates a random generation under the deployment locks, bound to the validated app/environment/service/target/database tuple; normal releases and ordinary database content changes retain it. Initial inventory can report `generation: unenrolled`; the first export includes the expected binding fingerprint and returns the newly established generation. Later exports require the known generation. A changed binding is refused until an administrator explicitly reconciles it and creates a new generation. This keeps `deploy ls` and inventory reads free of enrollment writes. An intentional replacement of the logical dataset at an identical path cannot be inferred from SQLite bytes; the operator must explicitly start a new generation if it represents a different dataset. Losing source generation records is an identity problem, not permission to quietly create a replacement for an already enrolled service.

## 4. Managed Quadlet metadata and inventory

### 4.1 Comment schema

Example application marker, encoded as one physical line:

```ini
# deploy:metadata {"schema":1,"managed_by":"deploy","role":"application","app":"foodlog","environment":"prod","service":"foodlog-prod","mode":"binary","target_dir":"/home/deploy/apps/foodlog-prod","backup":{"enabled":true,"driver":"sqlite","source":"data/app.db","container_uid":65532,"container_gid":65532}}
```

Use `encoding/json`; never interpolate unescaped fields into comments. Reject literal newlines/control characters in identity fields. Limit marker size to 16 KiB, scan files with explicit total-size limits, and detect duplicate JSON keys as well as duplicate marker lines. Accept additive unknown fields for a known schema only if they cannot alter security semantics; unsupported major schema versions remain visible but ineligible for backup.

Define roles `application`, `infrastructure`, `maintenance`, `backup-worker`, and `notification-relay`. Non-application roles use a smaller role-appropriate payload and cannot opt into SQLite backup. Infrastructure need not invent app/environment/target fields. Use the same encoder in all render paths, including maintenance and Traefik, and reserve generated `io.deploy.*` runtime labels against user collisions.

Marker data is public operational metadata: never include credentials, `.env` content, notification recipients, arbitrary executable hooks, or key paths. A marker is a claim of management, not an authorization credential.

### 4.2 Ownership and drift

For application export, require matching service and target ownership records, a confirmed release in the correct deployment mode, and agreement between installed Quadlet metadata and the committed candidate metadata. Reject missing/corrupt records, pending deployment/database/boot transactions, and conflicting source bindings. Do not relax existing adoption rules to support inventory.

Validate the relevant effective runtime configuration as well: a volume or user override must not silently invalidate the declared database binding. The supported backup profile requires a bind-mounted database directory, no undeclared replacement of that mount, and no unsupported user-namespace customization. Initially reject relevant drift/drop-ins the helper cannot safely interpret. Do not infer a host path from an arbitrary container path.

Metadata is installed inside existing release transactions, so failed releases and rollback restore the exact previous marker with the old Quadlet. `deploy ls` can report a candidate during an in-progress release as pending; the exporter must wait/retry or fail without backing it up as confirmed.

Older managed deployments without markers appear as `legacy-managed` only when existing deployment evidence positively establishes ownership; otherwise label them `unmarked`. They receive markers on the next successful release. No broad scan-and-adopt operation in v1.

### 4.3 Inventory merge

`deploy ls` combines Quadlet sources, generated unit state, and `podman ps --all` plus inspect data. Honor actual rootless search paths, precedence, symlinks, masks, and drop-ins; show overridden/shadowed files separately instead of pretending every file is active. Discovery remains read-only and never calls `daemon-reload` or starts a service. Quadlet source/search behavior is documented in the [Podman Quadlet manual](https://docs.podman.io/en/v5.4.2/markdown/podman-systemd.unit.5.html).

Join containers to units using generated unit/runtime identifiers and verified inspect information, supplemented by reserved labels for our generated units. Do not merge merely because names look similar. Explicit `ContainerName`, missing containers, stale labels, and orphan containers need coverage. If association cannot be proven, expose separate rows with an ambiguity diagnostic.

Keep this bounded: use the installed generator/systemd's read-only effective-unit information where possible, and interpret only the supported deploy-owned profile for backup authorization. Do not build a second complete Quadlet/systemd parser to authorize every external configuration. Unresolved external associations remain separate diagnostic rows; unresolved settings affecting a managed database binding make only that instance ineligible. A successful mount check proves the declared host file is reachable through the configured mount, not that arbitrary application code actually uses it; correct `database.source` remains an explicit application contract.

Suggested display:

```text
SERVICE                MANAGEMENT      APP       ENV    STATE        BACKUP
foodlog-prod           deploy          foodlog   prod   running      enabled
foodlog-test           deploy          foodlog   test   running      disabled
foodlog-prod-maint     deploy          foodlog   prod   inactive     —
traefik                deploy          —         —      running      —
deploy-backup-worker   deploy          —         —      scheduled    —
redis                 unmarked        —         —      running      —
old-worker            unmarked        —         —      stopped      —
```

Management and runtime state are independent dimensions. Include malformed marker, ownership drift, unsupported schema, configured-but-not-created, masked, failed, and inaccessible states. A stopped application can still have a valid backup; an idle scheduled worker is healthy if its timer and last results are healthy.

`deploy ls --json <server>` emits a versioned schema with stable IDs, diagnostics, and completeness. Partial Podman/systemd failure must not masquerade as an empty inventory. Provide `--managed` filtering without suppressing diagnostics. Without a server argument, list the current local user's inventory. Never silently enumerate other rootless accounts.

## 5. Worker setup and lifecycle

### 5.1 Public CLI

Commands below are proposed. Flags follow existing Go parsing conventions: global options precede the command; subcommand flags precede positional arguments.

```sh
deploy --servers ./server.yaml validate --server
deploy --dry-run backup-worker setup home-backup
deploy backup-worker setup home-backup
deploy backup-worker status home-backup
deploy backup-worker status --json home-backup
deploy backup-worker run home-backup
deploy backup-worker logs home-backup
deploy backup-worker list home-backup
deploy backup-worker list --source vps-main --instance foodlog-prod home-backup
deploy backup-worker list --json home-backup
deploy backup-worker pull --backup <backup-id> --output ./restore-test home-backup
deploy backup-worker pull --latest --source vps-main --instance foodlog-prod --output ./restore-test home-backup
deploy backup-worker notify-test --channel signal home-backup
deploy backup-worker recover --scan --archive /mnt/backup/deploy home-backup
deploy backup-worker recover --apply --archive /mnt/backup/deploy home-backup
deploy backup-worker verify home-backup
deploy backup-worker restore-check --instance foodlog-prod home-backup
deploy backup-worker prune --list home-backup
deploy backup-worker prune --apply --source vps-main --instance foodlog-prod home-backup
deploy backup-worker pause --until 2026-11-01T12:00:00+01:00 home-backup
deploy backup-worker resume home-backup
```

All relevant commands accept explicit source/instance selectors where needed; require both when a service name is ambiguous across sources. Status JSON separates installation, schedule, archive, per-instance backup, restore verification, notification transport, and external monitoring health. Human output shows actionable errors and last successful backup age.

Add explicit retirement/reconciliation and credential-rotation commands during the lifecycle phase. Their exact flag spelling can be finalized together; semantics below are mandatory. Do not expose destructive restore as an accidental variant of `run` or `restore-check`.

`list` inventories stored backups on the selected worker; `pull` downloads one existing backup to the operator's machine for inspection or testing. They do not contact the original VPS or create a new snapshot. The exact selection, transfer, and local-publication contract is in section 12.1. This is distinct from existing `deploy db pull <env>`, which retrieves current source data into an application's configured local database.

### 5.2 Setup transaction

1. Parse and validate the complete selected configuration locally. Produce an action plan with hosts, paths, source accounts, timer schedule, retention interpretation, notification providers, and image versions. Pure `--dry-run` performs no SSH, pulls, key creation, or writes; remote values remain explicitly unknown.
2. Connect using existing administrator access, verify host keys, detect real architecture and OS, and check Podman, Quadlet generator, cgroup v2, user systemd, linger, subordinate UID/GID mappings, filesystem capabilities, free bytes/inodes, and required source tools. A reachable SSH shell alone is not proof a user systemd manager works.
3. Do not silently install OS packages or rewrite SSH/systemd policy. Provide exact prerequisites or a separately explicit bootstrap option. Reuse the repository's supported deployment floor where practical; test the actual generated units against the worker's installed versions. 64-bit ARM Linux is the initial Raspberry Pi target; 32-bit Pi OS is unsupported in v1 and receives a clear explanation.
4. Before the first archive-enrollment, identity/key, or source-helper mutation, acquire the worker host's setup-coordinator lock and durably create the setup journal with operation ID, requested configuration hash, and observed installed generation. Hold that coordinator through finalization; a competing lifecycle operation returns busy without making persistent changes. Then enroll/verify the archive UUID and mounted filesystem. On first setup require an explicitly chosen empty/new archive location or validate an existing managed archive; never `chown -R` arbitrary storage. Check actual read/write/rename/fsync behavior as the container's effective user.
5. Generate worker identity and per-source Ed25519 keys on the Pi. Preserve them on repeated setup. Read each public key through the authenticated Pi connection, validate its type/encoding/fingerprint, and use the operator's existing source connection to install it under the source deployment user. Private keys stay on the Pi. Keep unrelated `authorized_keys` entries and permissions intact, serialize updates, and write atomically with rollback data. Test the installed key as described in section 6 before marking enrollment complete.
6. Install a versioned source helper and restricted key entry; negotiate protocol capabilities. Source provisioning is tracked independently so a partly completed setup can resume. Helper upgrades are atomic and preserve clients of other workers; one worker's uninstall must not remove a shared helper still in use.
7. Stage and verify the pinned candidate worker image, then verify DNS, routes, source SSH keys, and inventory from an ephemeral instance using its actual container network context. This initial test does not require the eventual timer/service to exist. A hostname reachable from the laptop may not resolve on the Pi or inside rootless networking. Support explicit worker-source connection overrides only when needed and display them; do not silently reuse administrator SSH config or agent forwarding.
8. Produce normalized runtime config containing only selected sources, worker-owned keys, pinned source identities, paths, and policies. Include a schema version and content hash. No application repository or administrator secrets are required at runtime.
9. Configure messaging. Signal linking requires the user's primary device; Telegram requires a valid bot token/chat. Secret input must be hidden or supplied from files, never CLI arguments or logs. Interactive setup can guide these steps; unattended setup fails with precise missing prerequisites. Provider setup never disables the other configured provider.
10. Stage images, config, Quadlets, and timers. Validate candidates with the real generator. Advance the existing setup journal outside the archive and preserve the prior configuration. Pull the pinned images before touching active units. Image updates are explicit; no Watchtower or floating `latest` behavior.
11. With the setup-coordinator lock already held, publish maintenance intent, stop future managed activations, and drain current jobs with a bounded wait or return a resumable busy result. Then acquire the exclusive schema/installation lock and archive maintenance lock in the common order, migrate/install the compatible helper/image/config set, reload, and record the installed generation. **Release the exclusive locks before** restarting health/notifier processing or waiting for the first real backup: those jobs acquire shared locks and would otherwise block on setup itself. Keep only the coordinator lock through verification/finalization. A rollback reacquires exclusive locks after draining new jobs and must preserve already committed new history. A test notification is marked as a setup test, not proof of recovery. This planning task sends nothing.
12. Enable the regular timer and notification pump, report per-source results, and verify external monitoring configuration where credentials permit. If first backup or notification testing fails, leave the installation in an explicit degraded/pending state with retry scheduling where safe. Do not call it fully protected. A setup/configuration failure restores the prior working units; an operational source outage does not unnecessarily uninstall a valid worker.

The coordinator has one stable per-account installation location, independent of the inventory alias, candidate state directory, or archive path. Setup, update, credential rotation, and uninstall use that same lock. Record each intended mutation before performing it and its observed result afterward; create identity/private-key files atomically without replacement and reconcile any interrupted operation before generating alternatives. Read back the installed generation under the coordinator rather than trusting preflight observations. Independent workers sharing a source also serialize helper/authorization installation through a source-account installation lock and compare the expected source-policy revision; do not restore an older shared helper/policy over another worker's newer enrollment. Restrict that source lock to short installation mutations, not image pulls, messaging setup, or backup transfers.

The early journal phase is `preparing`: normal backup/notifier jobs continue against the active configuration while candidates are staged. It is distinct from the later `migrating` phase that drains writers. If the laptop disconnects before activation, record/reconcile pending source operations and preserve the working installation; an incomplete preparation must not make boot reject otherwise compatible active writers. Resume inspects actual host/key state against journaled identities, including mutations whose acknowledgment was lost. No human input is awaited while holding exclusive schema/archive locks.

Interrupted setup must be resumable without replacing keys, deleting backups, or duplicating units/authorized-key lines. Rollback restores files and timer activity only; it does not roll back already published backups. Removing staged credentials from sources after a failed initial setup must be scoped to entries created by that setup, or explicitly reported as pending cleanup.

Filesystem atomicity does not make multi-host setup atomic. Maintain a journal of each host's installed helper, key fingerprint, policy/config generation, and activation result. Before restoring `authorized_keys`, compare its current hash to the file this setup installed; if somebody changed it meanwhile, remove only the owned entry or report a conflict instead of restoring an old whole-file copy. If `AuthorizedKeysCommand`, a nonstandard authorized-key path, or an external key manager controls access, stop that enrollment with exact integration instructions rather than modifying an unused/default file or changing sshd policy.

Dry-run and validation show missing secrets/providers without fetching or creating them. Real setup may configure healthy sources while another fails, but records the complete requested coverage and reports a nonzero/degraded result; it must never shrink the requested set to the sources that happened to succeed.

The worker-host installation transaction has its own bounded supervisor and journal; it must finish or safely roll back if the laptop disconnects after activation starts. On boot, the installed host helper checks that journal before admitting state writers. An incomplete migration cannot allow old notifier/helper binaries to open a newer incompatible state schema. Linking Signal or collecting secrets happens before the exclusive phase; never hold migration locks while waiting for human input. Test the full setup path through its first ordinary backup, not just unit-file installation, to detect lock/self-wait mistakes.

### 5.3 Rootless containers and timers

Install `deploy-backup-worker.container` with `Type=oneshot`, `RemainAfterExit=no`, and a separate ordinary `deploy-backup-worker.timer`. Enable the timer, not the generated service with `systemctl enable`. Quadlet documents both oneshot containers and the timer/RemainAfterExit interaction in its [service-type section](https://docs.podman.io/en/v5.4.2/markdown/podman-systemd.unit.5.html#service-type).

Use `Persistent=true` and a small randomized delay for the backup timer. A missed run produces a current backup after boot, not fictional historical snapshots. Systemd does not start a second instance of the same active service when its timer elapses; manual runs must also honor a host-visible worker lock. [systemd timer behavior](https://www.freedesktop.org/software/systemd/man/latest/systemd.timer.html)

Enforce the 40-minute default deadline inside the worker, plus an explicit host `TimeoutStartSec=42min` for the generated oneshot service and a bounded `TimeoutStopSec`. Derive the host limit as configured run deadline plus two minutes of launch/cleanup allowance; it is not extra backup work. Ensure total launch/run/termination allowance and timer jitter fit both the configured monitor grace and ordinary schedule interval. Do not use `RuntimeMaxSec` as the oneshot deadline: systemd documents that it has no effect for this service type. Verify effective generated properties and kill an intentionally hung worker on every supported systemd/Quadlet floor, including all container children. [systemd service timeout semantics](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html#RuntimeMaxSec=)

Container profile: pinned multi-architecture image, read-only root filesystem, writable bounded temporary directory, no host Podman socket, no privileged mode, no host root filesystem mount, limited capabilities/resources, fixed mounts for archive/state/config/secrets. Test rootless UID mapping, SELinux labels, and access to any shared Signal socket. Do not apply private relabeling that makes the socket inaccessible to the other container.

Mount existing directories with creation disabled: neither Podman nor a setup convenience function may create a replacement archive path when its disk is absent. Bind the verified mounted directory itself and hold a descriptor for archive operations so a path swap cannot redirect a partially completed run to the underlying filesystem. The notifier receives state and notification credentials but no source SSH keys or writable archive mount; the backup container receives source SSH keys but no Signal account store. A failed archive precondition must still be recorded by the notifier/host failure path or eventually detected externally.

Run a separate lightweight notification/evaluation oneshot container every minute, using the same deploy image and a separate unit name. It handles delivery retries, daily reports, hung-run detection, and staleness even while a backup is waiting on SSH. It does not require the archive mount to be healthy and does not hold the backup execution lock. Its own execution is serialized.

The notifier cannot inspect host systemd or the archive through its current mounts. Add a bounded host-side `health-probe` operation to the installed worker helper, scheduled before each notification pass through a small host orchestration unit. It records worker/notifier timer state, last unit result, active run/boot identity, and an archive mount/UUID/access probe with observation time and config generation. It must report pre-start errors, OOM/timeout exits, and manually disabled timers even when the backup process never wrote state. Probe failure is data for the notifier, not a failed prerequisite that prevents it from sending. A probe subprocess timeout records `unknown`; it does not hang the notification pass or assume the disk is healthy. Ordinary health observations older than two minutes are insufficient for a green daily report. The orchestrator has no messenger or source credentials, and no host-control socket enters a container. If state is unwritable, the notifier's bounded emergency mode uses its own credentials; the backup container does not gain them. External monitoring covers failure of the orchestrator/notifier itself.

If Signal is selected, install a separate persistent `deploy-signal-relay.container` for `signal-cli`. Worker, notifier, and relay are all marked as deploy-managed with distinct roles. The relay's failure must not be a `Requires=` dependency that prevents backups from running. Distinguish background relay activity from finite backup runs in `deploy ls`.

Store worker state/outbox on persistent Pi-local storage separate from the external backup disk. This allows reporting a missing backup disk. If that local storage or the Pi itself fails, the external monitor remains the independent detection path.

Enforce host-wide admission limits, not just per-container limits: one snapshot-producing run, at most one maintenance/restore drill, and a small bounded number of archive exports (default two). Prioritize scheduled backups and notification work over scrubbing/drills; do not start a long maintenance task that cannot fit before the next backup window. Cancel/defer disposable maintenance work when necessary, without labeling cancellation as database corruption; overdue verification is its own incident. Reserve disk space for all admitted jobs plus the next snapshot, and size combined memory/CPU budgets for worker, validators, Signal, and host overhead. A maintenance job or a large local pull must not make the Pi miss backups through OOM or uncontrolled resource contention.

## 6. Source helper, SSH boundary, and protocol

The source helper is a versioned installation of deploy's export functionality, invoked only through a restricted SSH entrypoint. Public administrative commands may wrap it, but runtime requests use a narrowly defined protocol rather than arbitrary CLI command strings.

Allowed operations: protocol/capability handshake, authorized inventory, and export of a known service/database generation. Requests name stable identifiers; clients cannot supply a filesystem path, shell command, restore operation, or arbitrary SQL. Parse bounded structured input with an allowlist; never evaluate `SSH_ORIGINAL_COMMAND`. Unknown operations and protocol versions fail closed.

Use OpenSSH forced-command and restriction options to disable interactive shell, PTY, agent forwarding, port forwarding, and X11 forwarding. Account-wide SSH configuration may impose additional constraints, so test the installed entry rather than trusting its text. [OpenSSH authorized-key restrictions](https://man.openbsd.org/sshd.8#AUTHORIZED_KEYS_FILE_FORMAT)

The installed entry uses `restrict,command="<absolute-helper-path> source-serve --worker-id <validated-id>"` with a fixed dispatcher; worker identity comes from that trusted entry, never from a self-asserted request field. `restrict` also disables user SSH RC execution. The dispatcher accepts only framed protocol input, rejects nonempty unexpected original commands, clears code-affecting inherited environment, and uses fixed executable paths/a controlled PATH. Interactive shell, SCP/SFTP, forwarding, and arbitrary command tests must fail using the actual Pi key. Only the initial/rotation public-key line crosses the laptop; normalized runtime configuration contains the Pi's private-key path, never the operator's key.

Environment sanitization inside the helper is too late to protect the login shell that sshd starts first. Qualify the account's effective SSH environment policy as part of the restricted-source profile: do not accept client-controlled shell/interpreter/dynamic-loader startup variables, arbitrary `AcceptEnv` patterns, or other pre-dispatch execution paths. Run negative environment-injection tests; if effective policy cannot be established, require the source administrator to fix/verify it rather than claiming the key is restricted. Derive necessary HOME/user/runtime paths from the authenticated account and trusted host state, not caller-controlled values. Do not silently rewrite sshd configuration.

Read-only `ssh-keyscan` output is not a trust decision. Derive worker host-key pins from the already verified operator-to-source connection/trusted known-host configuration, mapping the approved key to the endpoint the Pi will use. A changed endpoint must still present an approved key. Treat unknown/changed keys as an explicit enrollment error.

Look for the same public-key material in other visible authorized-key entries: an unrestricted duplicate could defeat the intended restriction. Reject that conflict; do not silently broaden or remove unrelated access. State the limit when another key backend cannot be inspected. Rotate by installing a new restricted key, proving new-key operation from the worker, switching the worker configuration, draining/revoking old helper sessions, and only then removing the owned old entry. The helper checks its server-side worker authorization on each operation and before streaming; key revocation does not retroactively erase snapshots or terminate already authenticated sessions by itself. Report any bounded in-flight transfer that may still finish. Never log key material beyond public fingerprints.

The restricted key logs into the existing deployment user's namespace so that rootless Podman ownership/locks remain consistent. It is not a separate Unix security domain from that deployment account: a fully compromised source account can alter its helper or database. Document this boundary. Restriction limits the worker key's authorized actions; it does not prove data from a compromised source is trustworthy.

Worker SSH uses `BatchMode=yes`, strict pinned host-key checking, explicit identity files, `IdentitiesOnly=yes`, no agent forwarding, and no inherited operator SSH config. Any multiplexing socket is private to that worker identity; include credential generation in its cache key or disable multiplexing for exports to avoid carrying stale authorization after rotation. Do not disable host-key checks to repair onboarding.

Each enrolled key is associated with a worker ID and source policy. Its inventory includes only backup-relevant metadata, not environment files. The source must re-check opt-in and ownership on every export. Rate-limit/concurrency-limit source exports and cap spool usage so multiple legitimate workers cannot exhaust the source disk.

Use a length-delimited protocol, not a tar extraction surface: magic/version, bounded JSON header, exactly the declared number of database bytes, and a bounded completion footer. Header includes source UUID, service, database generation/binding, snapshot ID, committed release ID, timestamps, size, and checksum. Footer and successful SSH exit are required. Standard output carries only protocol data; diagnostics go to stderr with redaction. Unexpected bytes, truncation, duplicate fields, oversized lengths, mismatched request IDs, or incompatible capabilities fail the transfer.

Each new snapshot attempt gets a worker-generated random request nonce, run ID, config generation, and durable per-instance capture sequence before SSH starts. The helper creates a fresh snapshot for that request and echoes its nonce/identities in the authenticated response; the worker checks every field and stores them in the receipt. A reconnect/retry uses a new nonce for a new snapshot, while archive `pull` intentionally exports an existing immutable ID through its different protocol role. Replayed receipts/bytes from another attempt cannot count as current coverage. These checks prevent accidental stale responses and protocol mixups, not a fully compromised source forging current-looking data. Stream bodies directly to bounded files and cap stderr, JSON, and inventory records; do not reuse `boundedOutput`'s unbounded `bytes.Buffer` for remote-controlled data.

Use unpredictable request/snapshot IDs and explicit maximum snapshot size, negotiated before allocation. Default limit should be documented and configurable (initial proposal: 10 GiB per snapshot); larger databases require deliberate capacity/deadline settings. Do not resume partial snapshots in v1: retry a newly created consistent snapshot and discard only identified incomplete local data.

## 7. Snapshot transaction

### 7.1 Source side

1. Resolve the service from validated installed metadata and positive ownership evidence. Acquire the same service and target locks as releases/database replacement, in the existing order.
2. Re-read metadata, database binding, generation, pending markers, and committed release under lock. A discovery/export race cannot select a different database unnoticed.
3. Check that the source exists as a regular file with a complete SQLite header; explicitly reject zero-byte/uninitialized files before starting the backup API. Open read-only and never let a missing path create an empty SQLite database. Reject unsafe path components, traversal, symlinks outside the approved binding, and database paths within deployment state/backup directories. Perform checks in the same rootless namespace used for access when needed.
4. Verify source capacity/inodes for an independent temporary snapshot and reserve; account for WAL growth and concurrent application writes. Do not rely on the main `.db` size alone. Enforce a bounded disk quota/size budget on the spool.
5. Create a consistent SQLite backup into a private export directory outside the application's writable data mount, using existing `.backup`/`podman unshare` machinery with bounded SQLite busy handling. Never copy only the live `.db` or manually remove `-wal`/`-shm` files. SQLite's online backup API supports copying a live database while other clients continue operating. [SQLite backup documentation](https://sqlite.org/backup.html)
6. Finalize and normalize only the private snapshot into the standalone archive format described below, close its writers, then validate it, compute SHA-256 and actual size, write its export manifest, and flush it. Record snapshot-start/finish times and the associated release before releasing locks.
7. Release deployment locks after the independent snapshot is complete; stream this immutable snapshot over SSH. This deliberately improves on the current lock-through-transfer path, so a slow home connection does not block releases for the entire download.
8. Remove the export spool on normal success/failure. For killed processes/reboots, a bounded later cleanup removes only expired helper-owned exports whose live OS lock can be acquired exclusively. Source pruning must never traverse other deployment history. Hold a shared export lock while streaming; a lease timestamp cannot authorize cleanup of an active snapshot.

The new source exporter must not infer initialization from `quick_check`: a zero-byte read-only source can produce an initialized empty destination whose check returns `ok`. Check the source itself under the deployment locks, before backup generation; an initialized database with zero application rows remains valid unless the configured application check says otherwise. File/header checks cannot identify logically missing records or prove the correct business dataset. Test absent, zero-byte, truncated-header, initialized-empty, and live WAL inputs separately; do not reject a valid WAL database just because its main file is much smaller than its logical content. [SQLite file/header format](https://sqlite.org/fileformat.html)

The first archive format stores a closed, standalone database in rollback-journal `DELETE` mode. A backup from a WAL source can retain WAL format; opening it with `mode=ro` may still create `-wal`/`-shm` files or fail on read-only media. After successful online backup, use SQLite to transition only the helper-owned private destination to `DELETE`, verify the returned mode, close all destination handles, and require no pending sidecars before hashing/publication. Never strip sidecars manually to make a bundle look complete, and never change the live source's journal mode. Record the archive database format in the receipt/capabilities; normalization precedes every reported checksum. Test validators with the file and directory read-only and assert no directory entries or bytes change. Do not repair already published bundles in place. Existing foreign/legacy WAL files require a separately qualified reader; `immutable=1` must never be used on a live source or as a workaround for unknown WAL state. [SQLite read-only WAL behavior](https://sqlite.org/wal.html#read_only_databases)

Holding deployment locks serializes deploy-controlled DB replacements, not arbitrary application writes or external administrators. Ordinary live writes are handled by SQLite. Applications that rename/replace their database outside these locks are outside the initial supported profile and must be adapted or explicitly quiesced by a future feature. High write pressure or an exclusive lock can make an online backup fail; report/retry, never stop the app automatically.

Qualify source write impact as well as backup completion. Phase 0 must exercise the selected SQLite backup implementation under representative WAL and rollback-journal writers, slow storage, and near-limit databases; record transaction latency, busy failures, WAL growth, backup restart behavior, and deployment-lock duration. A five-minute snapshot timeout is not an acceptable proxy for application latency. Prefer bounded incremental backup steps without a surrounding long-lived source read transaction; allow bounded retries/restarts to fail rather than holding a read transaction indefinitely to force completion. Declare the tested size/write-load envelope and application latency acceptance threshold before production enrollment; if it cannot be met, narrow the supported profile or change the snapshot implementation. Do not silently change source journal mode or stop writers. [SQLite incremental backup locking](https://sqlite.org/c3ref/backup_finish.html) and [journal-mode concurrency](https://sqlite.org/isolation.html)

The helper owns a source-side watchdog for its complete process tree, independent of the SSH client's lifetime; killing local `ssh` is not proof that remote SQLite stopped. Source snapshot/export deadlines and output-size limits are locally capped policy, not trusted request values. Release the existing service/target lock descriptors explicitly after snapshot creation and prevent the streamer/watchdog children from accidentally retaining them: the current shell implementation uses inherited FDs 9 and 8. A timing test must demonstrate a new deployment can acquire both locks while an intentionally slow stream is still active.

Deadlines are application/process controls, not a guarantee against a kernel task stuck in uninterruptible storage I/O. If an overdue task remains alive/locked after termination attempts, record a host/source-stuck incident and stop launching more work against that blocked scope. Never unlink its locks or declare recovery solely because a deadline elapsed. Healthy sources/notifier work continue where possible; a completely stuck host remains the external monitor's responsibility.

An old-generation snapshot that finishes transferring after a deployment remains a valid historical backup with its recorded binding/release. Before claiming the latest inventory is fully protected, reconcile any newly observed generation; never rewrite the receipt to pretend the snapshot belongs to the new release.

### 7.2 Worker side and publication

1. Verify the enrolled archive UUID, mount identity, permissions, writability, bytes/inodes, and clock sanity before receiving data. Re-check the mount around publication. An absent disk must not cause writes to the underlying SD-card directory.
2. Create a private unique `.incoming/<snapshot-id>` on the archive filesystem, with exclusive file creation and no symlink following. Enforce transfer byte/time limits before and during streaming.
3. Verify protocol identity, byte count, digest, completion footer, and process exit. Run SQLite `quick_check` with a bounded timeout in read-only mode. It is a structural check; it is not proof of application correctness or all relational/index properties. [SQLite PRAGMA documentation](https://sqlite.org/pragma.html#pragma_quick_check)
4. Write `receipt.json` containing source and worker IDs, app/environment/service, database generation and logical path, release ID/image identity where available, snapshot times, receipt time, size/hash, tool/protocol versions, and validation results. No secrets or data rows.
5. Fsync database and receipt, sync the temporary directory, rename the entire bundle into the final archive directory on the same filesystem, then sync its parent directory. Never overwrite an existing final bundle.
6. Record a durable worker-state success and notification event after publication. A crash between archive publication and state commit is reconciled by scanning final valid receipts; an incomplete directory is never promoted merely because it exists.
7. Release per-instance run state and then evaluate retention. A retention error does not erase a successful backup: report backup success plus maintenance failure as separate health dimensions.

Example layout:

```text
/mnt/backup/deploy/
  archive.json
  .locks/                    # Stable maintenance/index/bundle/control-commit locks
  control/
    recovery.json            # Versioned non-secret recovery checkpoint
    verification/            # Immutable check results bound to backup hashes
  sources/<source-uuid>/<service>/<database-generation>/
    <snapshot-id>/
      database.db
      receipt.json
  .incoming/
  .trash/
```

Snapshots are mode 0600; private directories are 0700. No automatic compression, deduplication, incremental replication, or encrypted-archive format in v1. SSH protects transit; ordinary filesystem permissions do not encrypt data at rest. Support an already encrypted mounted disk and document unlock-after-reboot implications. An additional independent/immutable copy can be added later; a writable Pi archive alone does not resist compromise of the Pi account.

Record the planned bundle ID/path in state before receiving data, then advance through `receiving`, `published-unindexed`, and `committed`. A successful fsync/rename followed by a failed state commit is an uncertain bookkeeping result, not permission to report success or delete the bundle. Recovery checks the final receipt/hash and imports its original capture time/sequence exactly once; import time never refreshes backup age. Listing may identify such bundles as pending reconciliation, but cannot claim complete history from a stale catalog. Use the same atomic no-replace destination rule as local pulls, including for intermediate parent directories; sync newly created ancestor directories as well as the final parent.

Keep enough non-secret control data on the archive to recover conservatively after loss of Pi-local state: source/instance bindings and aliases, expected/retired generations, active writer/state identity, capture high-water marks, verification pins, and the applied policy/config hash. Write a versioned, checksummed `control/recovery.json` atomically. Store verification results separately as immutable records naming backup ID/hash and check contract; never modify a snapshot receipt to append later results. Credentials, messenger account data, and notification-delivery guarantees do not belong in this checkpoint.

Checkpoint and state-store writes are another cross-filesystem boundary. Retention is disabled whenever their relevant policy/pin revisions disagree or recovery metadata is incomplete. Persist a new verification result/pin to archive control data before releasing its live bundle lock; a crash or disk failure retains the previous pin or blocks cleanup, never loses protection. A policy relaxation/retirement cannot authorize deletion until its checkpoint is durable. Full snapshots can still be preserved when checkpointing fails, but control-metadata health becomes degraded and no pruning occurs. Checksums detect accidental damage; they do not establish authenticity against a compromised worker.

Serialize recovery-control commits with a stable archive-local control-commit lock; one enrolled writer installation still contains several concurrent processes. Under that lock, validate the current writer epoch and read a consistent authoritative control revision from state before building the checkpoint. All changes to recovery policy, retirement exclusions, identity, or pins use this commit path; incidental outbox/health writes do not advance the control revision. Never replace a higher published revision with an older candidate, and reject different payloads claiming the same epoch/revision. A precomputed stale candidate must be discarded/rebuilt, not renamed over newer state. A writer-epoch change uses the existing fenced takeover path, not comparison of arbitrary UUIDs.

Keep state transactions short and closed during checkpoint file I/O. Durably publish referenced immutable records, checkpoint, and directory entries before acknowledging a policy/pin/retirement change as complete, then record the checkpoint acknowledgment in local state. If any boundary fails, report pending/uncertain control persistence and block retention; do not claim that a local state commit alone proves the change will survive loss of that state disk. Recovery reconciles a newer durable checkpoint instead of overwriting it from stale local state. Verify that an acknowledged retirement or protection pin cannot disappear after a delayed competing write followed by Pi-state loss.

Control-commit lock order: after shared schema/archive maintenance admission and any required bundle locks, acquire control-commit with no index lock or state transaction held. Release the short index lock after selecting/acquiring bundle locks, as before. Never acquire/wait for a bundle lock while holding control-commit, and never hold control-commit through snapshotting, validation, network calls, or messenger delivery. Explicit pin operations first acquire their target bundle's shared lock; a pruner retains its exclusive bundle lock while rechecking current control evidence and committing deletion intent. Use bounded acquisition and preserve the existing cleanup state machine. Read-only archive exports require no control-commit mutation or checkpoint acknowledgment.

Treat received SQLite files as untrusted parser input even when SSH is authenticated: use a current supported SQLite build, fixed SQL, no extension loading or user startup scripts, bounded validation memory/CPU/time, and no credentials in the validator's environment or inherited file descriptors. Run generic validation in a bounded child, explicitly pass only the selected file/private temporary directory, and qualify filesystem/network sandboxing on the supported Linux target. If relying on a sandbox for an isolation claim, validate its prerequisites during setup; a clean environment alone is not filesystem isolation. Apply the same input-handling constraints to local pull verification. Do not assume a valid SHA-256 makes a database safe to execute application hooks against. [SQLite guidance for untrusted databases](https://www.sqlite.org/security.html)

SQLite compatibility is distinct from transport-protocol compatibility. Qualify and record the SQLite runtime/build and required capabilities for the source exporter, worker validator, and shipped local reader. V1 supports the qualified built-in SQLite schema/features; application-defined collations/functions or unavailable virtual-table modules needed for validation are an explicit unsupported profile unless packaged in a reviewed compatible validator. Never load source-supplied extensions or substitute a dummy collation/function to make a check pass. Test actual schema fixtures; version strings alone cannot establish compatibility. Revalidate capabilities on tool upgrades and report schema changes that make a previously supported source uncheckable. [Connection-specific SQLite collations](https://sqlite.org/c3ref/create_collation.html)

For all generic SQLite checks, distinguish a demonstrated integrity finding from `unsupported-validator` and execution/resource failure. Require the expected result set (`ok` for quick/full integrity checks), not merely process exit zero or no output. Unsupported validation cannot publish a new verified backup, advance a recovery pin, or count as healthy coverage; it also cannot mark previously validated archive bytes corrupt or authorize deleting them. Preserve historical evidence and report the exact incompatible feature/tool with redacted diagnostics. Local pull fails with an actionable compatibility error and no final destination under its existing publication rules; the plain-file rescue path remains available. Application checks additionally retain their separate semantic outcomes from section 12.2.

## 8. Persistent state, scheduling, and recovery

Use a small transactional local state store for configuration generation, enrolled source identities, expected instances, run records, per-instance last results, incidents, delivery outbox, daily-report keys, restore-check results, and retirement state. Prefer a SQLite store accessed through a maintained Go driver; phase 0 must select/pin the driver and verify ARM64 builds. Keep archive receipts independently readable so backup data is recoverable if the state store is lost.

Transactions must be short; never hold a state-database transaction during SSH, snapshot generation, checksum work, or notification HTTP calls. Use explicit process locks for run exclusivity and delivery leases for the outbox. Crash recovery resets expired leases but does not assume an unacknowledged message was never delivered. All state writers use the same pinned database library/connection policy with foreign-key enforcement, bounded busy waits, and durable transactions (`synchronous=FULL` or an equally tested setting); a `Commit` returning before a durable write must not advance success/notification state. Prefer a local WAL-mode store and test host/helper/container concurrency on the actual filesystem. Shared `/state` must resolve to the same underlying files, ownership, and lock objects for every participant.

Version the state schema and normalized config independently. Back up the state database consistently before migration, quiescing every writer under the migration lock; worker image rollback must account for helper/notifier compatibility too. Never restore a pre-upgrade state copy after new backups, retirements, or deliveries have been committed: use a compatible forward repair or reconstruct/reconcile without discarding that history. Receipts recover historical snapshots; archive checkpoints/check records additionally recover known policy and pins where complete. They cannot prove missing policies or notification delivery. Require explicit reconciliation and keep health degraded until those uncertainties are resolved. Never reset a corrupt store to an empty green installation. Keep bounded logs and a small free-space reserve on the state filesystem. If state persistence fails, emit a redacted host-journal error and let the notifier attempt one bounded emergency send without pretending it is durably deduplicated; the external monitor remains the fallback when local persistence is unusable.

Maintain separate timestamps for last attempt, last successful snapshot, last completed run, last inventory observation, last full integrity check, last application restore check, and last notification acceptance. A fresh attempt is not a fresh backup. Measure the conservative recovery-point age from the worker's time immediately before its fresh snapshot request, not from a timestamp supplied by the VPS or the later download/import time. Store source-reported start/finish times separately for diagnostics. Use monotonic elapsed time within a run and a synchronized worker clock for persistence; source clock skew alone cannot make an old backup appear fresh. Latest selection/order within an instance uses its durable capture sequence, not the source clock. UTC storage and operator-selected timezone presentation apply throughout.

Freeze the expected-instance set and config generation for each run. Record complete inventories with an observation/revision fingerprint. Recheck source membership/bindings once before aggregate success; additions, removals, or binding changes leave coverage incomplete until reconciled/backed up, with no unbounded rescan loop. A concurrent worker config edit or retirement also invalidates that run's aggregate success unless explicitly reconciled under the state lock. Success is an observation as of those revisions, not a claim that no deployment can appear after the final check. Do not infer success from a completed run containing only the instances that were reachable.

Define that coverage fingerprint narrowly: enrolled identity, membership, backup authorization, database binding/generation, and required policy capabilities. Exclude observation time, uptime, changing container IDs/health, release ID when the database binding is unchanged, and other volatile display fields. Otherwise a normal redeploy or health observation would invalidate every run. Preserve the snapshot's actual release provenance separately; a changed binding or explicit backup disable still invalidates coverage.

Lock contract: keep the setup-coordinator, installation/schema, and run locks on Pi-local state storage. Normal jobs take the shared schema lock; migrations take it exclusively only after announcing maintenance/draining jobs. Archive operations next take the archive's shared maintenance lock; recovery/takeover/schema-related archive changes require it exclusively. Archive maintenance, index, per-bundle, and control-commit locks live in the archive's own stable `.locks/` namespace, so a recovery helper or another state directory cannot accidentally use unrelated locks for the same files. Under the short index coordination lock, acquire a nonblocking shared bundle lock for export/check or a nonblocking exclusive one for prune, then release the index lock before I/O. Pruners skip busy bundles. Preserve existing source service-then-target lock order independently; do not hold archive index/state transactions while contacting a source. Never unlink or replace live lock files, and never wait on network/workers inside a state transaction. Lease timestamps are diagnostic metadata, not permission to override a live lock. Release in reverse order. Linux locks can survive inherited descriptors; control those explicitly. [Linux flock semantics](https://man7.org/linux/man-pages/man2/flock.2.html)

Create and durably sync the stable archive coordination locks during enrollment and each bundle's lock before publishing that bundle. Read access opens existing locks without write/create/truncate flags; native Linux `flock` permits locks on read-only-open descriptors. Lease/audit writes are optional diagnostics for archive reads: failure to persist them must not prevent a safe export protected by the live OS lock. Read/recovery helpers must not open the missing/corrupt main state store in write mode merely to acquire admission or lease metadata. Use bounded host process admission independently of catalog health; temporary recovery state belongs on a separately writable destination. Missing lock files still require the fenced offline path from section 12.3. This does not relax durable bookkeeping for backup, verification-pin, or deletion mutations. [Linux lock access modes](https://man7.org/linux/man-pages/man2/flock.2.html)

Bind writable archive use to one enrolled worker/state UUID and writer epoch in archive control data. A different local state store must use explicit recovery/takeover, not initialize an independent catalog over the same archive. Takeover requires fencing/stopping the old writer and the exclusive archive maintenance lock; normal writers recheck the epoch before mutation. Keep archive-independent notifier health working when these archive locks cannot be accessed. Identical copies of an archive on different disks do not share file locks: replacing a Pi requires disabling old source credentials and rotating/reassigning monitoring credentials so a still-running clone cannot report misleading health. Cross-host active/active use remains unsupported.

Expected-instance rules:

- First successful setup shows the complete discovered set and enrolls opt-in instances. Zero eligible instances is `no coverage`, never green success.
- Newly discovered opt-in instances are enrolled and scheduled immediately only when no explicit retirement exclusion applies. Include additions in the daily report.
- Previously protected instances that disappear, lose their marker, disable backup, change binding, or become unreadable remain expected until explicitly retired/reconciled. Respect disabled opt-in by not exporting, but alert that protection has stopped.
- A failed/partial source scan cannot replace the last complete inventory with an empty set. Continue other known/available sources; do not claim all sources are healthy.
- Changing worker YAML to remove a source does not silently delete its expected inventory or archives. Require explicit retirement with a reason. Configuration application reports pending removals.
- Retiring an instance stops new backups and freshness incidents after explicit acknowledgment, preserves history, and suspends automatic deletion for its retired generations by default. Archive deletion is a separate explicit operation.
- Pause is bounded, visible, and recorded. Resume performs an immediate pass. A worker pause does not secretly pause the external monitor; show how to coordinate the external maintenance window, with expiry where supported.

Retirement records are persistent discovery exclusions, not deleting an expected-instance row. Instance retirement is scoped to the worker plus enrolled source UUID/service, with the observed binding/generations retained as evidence; source retirement excludes every service on that source. An unchanged enabled Quadlet, disappearance/reappearance, alias rename, a new generation of the same retired service, or re-adding the source to YAML must not silently reactivate it. Show discovered-but-retired resources and require an explicit reactivation/reconciliation action, which validates current identity/binding before requesting a fresh backup. An unrelated new service remains subject to ordinary opt-in discovery. Persist exclusions in local state and the archive recovery checkpoint; missing/inconsistent recovery evidence keeps uncertain scopes unenrolled until reconciliation. Retirement closes coverage incidents as `retired by operator`, not `recovered by successful backup`; reports show the reduced scope, and zero active instances remains no coverage. Worker retirement does not rewrite shared application markers or disable another worker's policy.

Pause is a durable admission policy with a UTC expiry, not disabling the timer. Keep backup timer, host probe, and notifier active; scheduled activations skip snapshot creation while paused. Reject new manual backup runs until explicit resume; listing/pulling and alerts remain available. An already admitted run may finish, with its outcome recorded but no coverage-success heartbeat while pause is active. The notifier evaluates expiry and records a catch-up request; the existing host orchestration helper consumes it and starts only the fixed managed backup unit through the ordinary run lock. No container gains a host-control socket or an arbitrary unit-start operation. Resume/expiry persist the request before activation and retry it if activation fails. Reboot evaluates the same deadline. Clock uncertainty keeps scheduling visibly paused/uncertain pending reconciliation rather than guessing expiry. Daily reports show the pause; existing incidents remain visible, and resume immediately reevaluates freshness without resetting backup ages. External maintenance expiry remains independently configured.

Retry policy: after an ordinary failed attempt, enqueue its failure immediately and make up to three retries due at approximately 30 seconds, 2 minutes, and 10 minutes after the first failure, with jitter inside the run deadline. A due-work queue advances other sources while an instance waits; do not sleep the entire worker for one retry. Authentication/host-key/schema/path/corruption failures are not retried rapidly. A busy deployment lock is a visible deferral with a five-minute grace persisted from the first deferral, not reset by each probe; if still busy, open an incident. If freshness is already breached, alert without that grace. These defaults are bounded policy values, not scattered sleeps.

V1 processes one source/instance at a time for predictable Pi memory/disk usage; failures do not abort remaining eligible instances. A fairness cursor prevents a slow first source from starving the rest when the global deadline is reached. Record every deferred/unattempted instance, signal incomplete coverage, and continue from the cursor next run. Status estimates whether configured coverage can fit the schedule.

If wall time moves materially backward/forward, refuse age-based deletion and mark freshness/report scheduling uncertain. Use monotonic time for in-process deadlines; persisted timestamps cannot reconstruct monotonic time across reboot. Require sane synchronized time before new success claims; snapshot IDs use random uniqueness in addition to timestamps. Daily scheduling includes DST tests.

## 9. Notifications: semantics before providers

### 9.1 User-visible policy

| Event | Default behavior |
| --- | --- |
| Hourly run succeeds | Record silently; include in daily summary. |
| First source/instance/worker failure | Queue immediately when observed; attempt delivery promptly, without waiting for all sources. |
| Deployment lock temporarily busy | Defer up to five minutes; alert sooner if freshness is already breached. |
| Same incident fails again | Update counts/details; no duplicate immediate alert. |
| Incident unresolved | Remind once every six hours. |
| State becomes worse, e.g. backup becomes stale | Escalation update even within reminder interval. |
| Recovery after failure | One recovery event referencing the incident and actual new verified snapshot. |
| Daily 09:00 | One summary per worker/calendar day: healthy, degraded, failed, paused, or no coverage. |
| Notification delivery fails | Persist/retry; use configured fallback; show transport health separately. |
| Worker stops sending successful-run pings | Independent external monitor alerts. |

"Immediate" means after a failure is detected, not at the unknown instant a machine loses power. A connection failure is observed within its timeout; failure delivery still depends on a working network/provider. Target first local send within 60 seconds of a recorded event (normally immediately); fallback after at most two minutes without provider acceptance.

Make the target achievable in the normal small-installation case: after committing a newly urgent event, update a dedicated wakeup marker in shared state that a host systemd path unit uses to trigger the existing notifier orchestration unit. The one-minute timer remains the crash/lost-wakeup fallback, with explicit timer accuracy and no notification jitter. Do not watch every state/WAL write or rewrite the marker on every delivery attempt, which could cause a self-trigger loop. Concurrent activations still serialize through the notifier lock. For outage bursts, batch affected-instance summaries within provider limits, retain individual incident IDs, and prioritize the oldest unsent urgent events; a bounded pass cannot promise delivery of arbitrarily many separate messages in 60 seconds. Show oldest pending-event age and missed delivery targets as transport degradation. This is an operational target with measurable queue latency, not a hard real-time guarantee.

A healthy daily summary requires current complete coverage, no overdue protected instances, readable committed backup records, working archive, active schedule, and no unresolved critical incidents. Include prior failures that recovered during the reporting period, skipped runs, expected/discovered counts, newest/oldest last-backup ages, free storage, and restore-check age/status. Never say "all backups successful" merely because the last attempted instance succeeded. If an application restore check is not configured, say so rather than implying restore verification.

Use `Europe/Vienna` explicitly for 09:00; persist the local calendar date and schedule timezone as the daily event key. Repeated 09:00 evaluations, restarts, and fall-back DST must not create another event for that day. Spring-forward/nonexistent configured times produce one report at the first valid evaluation after the target time. On multi-day downtime, send one current catch-up report describing the gap, not a burst of stale daily successes. Associate it with the missed reporting periods; those periods retain `missed`/`superseded` history and become `covered by catch-up <event-id>` only after that current report is accepted. Do not fabricate historical delivery timestamps. A failed catch-up leaves the gap unresolved. Send the catch-up at or after the configured report boundary on its delivery date: a restart before today's boundary waits until that boundary for the routine report, while urgent incidents and backup recovery proceed independently. This avoids sending the only daily report before the external calendar check expects it. Its acceptance consumes the ordinary report entitlement for its local calendar day, preventing another routine success report that day; urgent incidents remain independent. Recompute a queued report's health before delivery so an old green summary cannot be sent after a newer failure.

A timezone or report-time edit does not itself authorize a second ordinary success report immediately after one was sent. Carry the last-delivery boundary through configuration migration and choose the next strictly future report boundary; report a material scheduling change in status. Daily reports do not suppress urgent failure or recovery events on the same day.

Example messages:

```text
Backup failed · home-backup · incident B42
vps-main / foodlog-prod: SSH connection timed out.
Last verified snapshot: 2026-10-31 08:02 CET (58 minutes ago).
Retry scheduled. Other sources continue.
```

```text
Daily backup report · home-backup · 2026-10-31
2/2 protected instances current; 46 snapshots completed in this reporting window.
One connection incident recovered; no open backup incidents.
Oldest latest snapshot: 54 minutes. Free archive space: 182 GiB.
Application restore drill: passed 3 days ago.
```

The summary window is since the previous report boundary, not an assumed 24 hourly runs: DST, enrollment, and downtime can change the count.

### 9.2 Durable outbox and incident model

Write event creation in the same local transaction as the corresponding result/incident change. Incident keys include worker, source, instance/generation when relevant, and normalized error class. Do not use arbitrary stderr text as a fingerprint. Source-wide connectivity failures produce one source incident with affected-instance counts; avoid one alert per database for the same unreachable host. Archive failures similarly produce one worker incident.

Events have stable IDs, creation time, current severity, redacted payload, primary/fallback delivery records, retry schedule, and provider acceptance IDs/timestamps where available. Deliver outside state transactions using an expiring lease. Backoff with jitter and honor provider rate-limit hints; cap retry burst size and queue growth. Coalesce repetitive reminders and superseded daily reports, but retain an audit trail of failures and recoveries. If an incident opens and resolves before delivery, send a combined "failed, now recovered" event rather than only a misleading success.

For v1, render each logical daily report or alert batch as one self-contained message per attempted provider; do not fragment one report across several messages. Fit the payload to the smaller supported provider budget before sending so fallback preserves its essential content. Always retain event identity, current overall severity, affected/expected counts, relevant backup age or unknown status, and an actionable `status` command. Bound labels, summarize detail rows, and explicitly show omitted-detail counts; never truncate away a failure indicator or turn a degraded report into a healthy-looking one. Keep full details in local status/history. If even the minimum envelope cannot be rendered, record a notification-format failure; do not credit a partial or empty message as acceptance. Unicode boundary and long-label tests belong to provider qualification.

A batch has an explicit bounded membership list of logical incident events in durable state. Its message identifies the batch and shows the number of events covered separately from overall outage totals; local status resolves the member IDs. Provider acceptance acknowledges only those members; omitted incidents remain pending for another batch even when the message mentions aggregate outage counts. A daily summary can accurately report totals with omitted detail while its own single daily event is delivered. This avoids inventing multipart acknowledgment/reassembly semantics, preserves the one-daily-report UX, and leaves the documented ambiguous-send duplicate limitation unchanged.

Define incident resolution per class: a backup incident needs a new valid snapshot of the affected generation; a disk/timer incident needs a new host probe and, when coverage was affected, a successful backup; a restore incident needs a passing restore check; a delivery incident needs an actually accepted message. An unrelated successful instance cannot close any of these. Delivery failures update one transport incident rather than recursively creating a new notification for every failed attempt to notify about notification failure. Prioritize first failures/recoveries over reminders and routine reports; deduplicate reminder/recovery event creation across notifier restarts. Persist open incidents until evidence of recovery or explicit retirement, never expire them merely to keep the queue small.

Exactly-once messenger delivery is not guaranteed: a provider can accept a message and the acknowledgment can be lost before local commit. Use durable deduplication to create one daily event and normally send it once, but document possible duplicate deliveries after ambiguous failures. Include stable event IDs for critical alerts. Prefer a rare duplicate to silently dropping a failure. When fallback accepts an event, stop ordinary primary retries for that same event; a separately pending primary delivery may still have been accepted, so cross-channel duplicates are possible.

Backup success and notification success are separate. A valid published snapshot stays successful if Signal is down; overall worker health is degraded until notification delivery recovers. A provider HTTP/RPC acceptance is not proof the human read a message. Recheck recipient/channel correctness during setup and expose last accepted delivery, not a fictitious "read" state.

Store secret values outside YAML and worker state payloads. Do not log bot-token URLs, Signal account IDs/phone numbers, group IDs, raw provider responses, database rows, full SSH commands with credentials, or unbounded source stderr. Send compact labels, sanitized error class, timestamps, incident ID, and a suggested local status command. Encode plain text safely; external service/app names may contain hostile formatting.

### 9.3 Signal as preferred provider

Use the third-party `signal-cli` project as the adapter, not a claimed official Signal bot API. It provides sending, linking, and a daemon with JSON-RPC; it needs ongoing updates as Signal changes. Upstream warns older releases may stop working. [signal-cli upstream](https://github.com/AsamK/signal-cli)

Deploy a dedicated relay Quadlet with pinned tested image/version, persistent private account data, and a shared Unix socket available only to the notifier and relay. No public TCP/HTTP notification endpoint. The notifier calls typed JSON-RPC operations with bounded payloads and maps JSON-RPC errors even when transport succeeds. The documented daemon supports Unix sockets and continuous receiving. [JSON-RPC manual](https://github.com/AsamK/signal-cli/blob/master/man/signal-cli-jsonrpc.5.adoc)

Onboarding links a named device to an existing account using a temporary QR/link URI. Do not re-register the user's existing phone number, which can disrupt their primary client. QR/link material must not be persisted in logs. If using the user's own account, test Note-to-Self/notification behavior explicitly; a dedicated automation account or selected group may provide clearer notifications. Incoming messages must not become deploy commands, and incoming content must not be logged or retained unnecessarily. [signal-cli command manual](https://github.com/AsamK/signal-cli/blob/master/man/signal-cli.1.adoc)

Account data grants meaningful access to the linked account; isolate it from backup archives and source credentials. Linked devices can become unlinked and require primary-device interaction to relink. Treat authorization loss as a persistent actionable incident, not an infinite rapid retry. [Signal linked-device guidance](https://support.signal.org/hc/en-us/articles/360007320551-Linked-Devices)

Critical implementation gate: prove native ARM64 compatibility and resource use on the actual Pi class. The inspected upstream README lists Java/native-library requirements and does not promise the same bundled native libraries on all architectures. Build or select a reviewed ARM64-capable image and test linking, receiving, sending, reboot, and upgrade; do not assume an x86 image will work through emulation. The image digest is an implementation-time choice, not an invented value in this plan. [signal-cli installation requirements](https://raw.githubusercontent.com/AsamK/signal-cli/master/README.md)

Maintain a separate relay update path with a tested release cadence. Pinning protects reproducibility but does not remove the need to update. Track support age/version in status and include actionable update notices in the daily report. Back up relay account state before migration; a data-format migration may make image-only rollback unsafe, so validate the combined image/state restore path and never run two relay processes against the same account directory.

Signal failure must leave backups operational and trigger Telegram fallback if configured. Without fallback, queue locally and expose failure through the independent report-delivery monitor. Do not label Signal unsupported until the ARM64 spike has been attempted; it is the preferred requested path and a first-release deliverable.

### 9.4 Telegram adapter and fallback

Use the official HTTPS Bot API `sendMessage`, with a token file and explicit chat ID. Setup verifies bot identity and destination with a clearly marked test. The user must establish access to the intended chat; permission errors, blocked bot, removed group membership, and changed/migrated chat identifiers are actionable. Do not create a webhook or long-running update consumer for outbound-only alerts. If onboarding uses `getUpdates`, make it explicit and avoid consuming updates owned by an existing bot integration.

Check both HTTP status and the JSON `ok` result. Handle 429 with `retry_after`, transient 5xx/network errors with bounded retries, and invalid token/chat/permission responses as persistent failures. Render the complete bounded message below the documented message length limit using section 9.2; summarize optional details instead of splitting the logical report. Use plain text, no untrusted Markdown/HTML interpolation. Do not enable paid broadcasts. [Telegram Bot API](https://core.telegram.org/bots/api#sendmessage)

Telegram is a separate delivery option, not a private Signal transport. Messages contain operational summaries only; no database contents or credentials. A Telegram fallback on the same Pi helps with Signal/provider problems, but it cannot notify during a Pi power or internet outage.

## 10. Independent missed-run detection

An independent monitor is required for the promise "notify me when backups stop." Local Signal plus local Telegram cannot cover a dead Pi, broken SD card, hung kernel, total power loss, or home internet outage.

Implement a small external heartbeat adapter compatible with Healthchecks-style HTTPS ping URLs, usable with hosted or independently hosted monitoring. Do not build a monitoring platform into deploy. Healthchecks supports start/success/failure pings and schedule/grace-based missed-run detection. [Pinging API](https://healthchecks.io/docs/http_api/) and [check scheduling](https://healthchecks.io/docs/configuring_checks/)

Two independently configured checks:

1. **Backup coverage:** success only after a scheduled pass has produced new verified snapshots for all expected active instances at the evaluated revisions, complete source inventory was obtained, and publication is durable. Partial failure sends failure where reachable; no success ping on discovery-only, zero coverage, paused, or merely container-started runs. Prefer a calendar check matching the actual worker schedule/timezone, with 45-minute grace for the default hourly run. A simple 60-minute completion interval plus grace has a longer worst-case recovery-point age when the previous run finished late; do not call those equivalent. Derive/display the maximum detection delay from schedule, jitter, run deadline, and grace; reject a claimed freshness guarantee that the selected monitor cannot meet.
2. **Notification/report delivery:** normally renew only when the current daily summary is accepted by at least one messenger, using the matching daily calendar schedule/timezone and grace. Additionally send an immediate external failure signal when both configured delivery paths fail (or the sole path fails), so the monitor need not wait until tomorrow to reveal broken alerts. Recovery requires a newly accepted real notification and every due reporting period either actually delivered or explicitly covered by an accepted current catch-up report; do not clear an overdue-summary incident merely because an unrelated alert was accepted. This permits a genuine recovery before the next daily boundary without manufacturing an extra routine success report. A daily degraded report still proves delivery while the backup coverage check remains failed. Old queued acknowledgments cannot refresh a current monitoring period.

Use receipt-only success/failure heartbeats initially; if start pings are added, configure run-time grace correctly and associate them with a run ID. Never let "start" be interpreted as backup success. Retry heartbeat requests briefly, but never replay yesterday's success as a current heartbeat. After reconnection, perform a current evaluation/run before restoring healthy status.

The coverage and delivery checks must have different effective check identities, and each worker needs its own pair. Validate resolved secret contents, not just different file paths: a daily delivery ping to the coverage check could falsely clear a backup failure. For the initial Healthchecks adapter, accept canonical UUID-based base ping URLs and reject action-suffixed or ambiguous alias forms; require distinct identities within the installation. Setup shows redacted check fingerprints and requires the operator to verify exclusive use where other installations cannot be inspected. A ping URL cannot prove global exclusivity. [Healthchecks check identities and endpoints](https://healthchecks.io/docs/http_api/)

One serialized publisher in the existing notifier owns heartbeat requests for each check; backup jobs commit evidence/intent instead of sending concurrently. Store a versioned current desired result tied to configuration, evaluated run/report, and monitoring period; suppress obsolete failures as well as obsolete successes before sending or retrying. A delayed failure from a recovered run must not overwrite a newer success through local replay. Do not hold a state transaction over HTTP. After an ambiguous timeout, reevaluate current evidence before a bounded retry; never replay an append-only queue of old heartbeat outcomes. Ordinary ping endpoints are not a transactional compare-and-set service: show local evidence and last acknowledged external update separately, and do not promise strict remote ordering after ambiguous transport failures. Test delayed acknowledgments and simultaneous backup/notifier updates.

The monitor's own notification route must work independently of the Pi. Recommended practical combination: local Signal for normal reports and failures, external monitor delivering through Telegram or another independent channel. If Signal is required for Pi-offline alerts too, the external monitor needs its own Signal-capable relay outside the home failure domain; the Pi-hosted relay cannot serve that purpose.

Setup accepts existing secret ping URLs and prints required external schedule/routing settings; do not assume access to create a monitoring account. Require an end-to-end missed-heartbeat test before marking offline detection verified. Without monitoring configuration, the worker can run, but status/setup must explicitly say `offline detection: not configured`, never fully protected. Heartbeat URLs are credentials: redact them, restrict permissions, and rotate after exposure.

A valid ping URL is not proof that monitoring is armed or routes alerts correctly. Verify the check is active/unpaused and capable of failing before any first successful backup, using an explicit controlled failure/missed-ping test; never send a fake successful backup to bootstrap it. Schedule/source-policy changes invalidate monitoring verification until the external expectation is updated or reconfirmed. Keep backup data health and monitoring connectivity separate when the monitor endpoint itself is unavailable. HTTPS requests have bounded retries, no insecure TLS fallback, and no redirects to a different host that could expose a secret URL; missing provider acceptance cannot be hidden by printing a request as sent.

## 11. Retention, capacity, and archive integrity

Retention is per source/service/database generation. A snapshot is eligible for deletion only if it is older than `max_age` AND outside `keep_last`, with explicit semantics when a field is omitted. Require `keep_last >= 1` for automatic worker retention, and protect the newest verified snapshot, the newest snapshot with a passing full integrity check, the latest application-restore-verified snapshot when available, actively locked restore/export snapshots, and explicit pins regardless of age. A new quick-check-valid snapshot does not supersede a last application-verified recovery point. Advance a verification pin only after a newer snapshot passes the same check contract; changing/disabling that check must not silently release the old pin. Releasing a recovery pin requires an explicit named action and reason. Invalid policy is rejected before deletion.

For an explicit retention block, omitted `keep_last` defaults to 48; omitted `max_age` means count-only retention. An absent retention block disables automatic deletion until setup writes an explicit selected policy. Setup's suggested policy is the combined 48/720h example, and it must persist that choice. Reject zero/negative `keep_last`, zero/negative specified age, and an empty explicit retention block rather than interpreting them as permission to delete everything. These worker rules do not change legacy retention semantics.

Run automatic deletion only after a new successful publication for that same instance and a complete valid inventory. A failed source must not cause its last good snapshots to age out while other sources succeed. Missing, disabled, retired, or drifted instances do not get silently pruned. Never prune the source's release backups as part of the Pi's retention policy.

Preview lists exact keep/delete reasons. Only owned regular bundles with valid receipts and matching identity may be removed. Corrupt inventory stops pruning for the affected scope and raises an incident. Foreign files, symlinks, incomplete directories, or unknown schema versions are preserved/quarantined for explicit inspection.

For deletion, atomically rename an eligible bundle into managed `.trash`, sync the affected directories, then remove its contents without following symlinks. Crash recovery resumes only known deletion records. Locks coordinate restore leases, publication, and deletion. A lost acknowledgment or missing state-store row must never widen a deletion scope.

Implement deletion as a recoverable state machine: under the short index lock obtain the exclusive bundle lock, then release the index lock. Acquire control-commit, re-evaluate current policy/pins and checkpoint agreement, durably record deletion intent, rename to a unique trash path, fsync source/trash parents, and record removal from the catalog. Release control-commit before unlinking/fsyncing the trash contents; retain the exclusive bundle lock through cleanup. This orders the deletion decision against policy/pin changes without holding the index/control locks during large-file cleanup. Recovery reconciles each possible final/trash location against that intent. If state vanished or locations conflict, preserve the files and require reconciliation. Incoming cleanup similarly requires ownership plus a nonblocking exclusive lock and an expired operation record; age alone never authorizes deleting a slow active transfer. Catalog rows and directory rename are not one transaction merely because each is individually atomic.

Capacity preflight reserves space for one new uncompressed snapshot, incoming data, SQLite temporary work, restore drill copies where scheduled, and configured free-space headroom. Monitor inodes too. A forecast from first-snapshot sizes is approximate; present it as an estimate and update it as databases grow. Default behavior when insufficient space is to alert and stop that operation, not emergency deletion beyond policy. An explicit prune can reclaim already eligible history after review.

Treat insufficient capacity as an explicit operator-recovery state: automatic post-publication retention cannot free space if no new snapshot fits. `prune --list` reports exact candidates, protection reasons, reclaimable bytes/inodes, and estimated remaining deficit. Explicit `prune --apply --source <source> --instance <service>` may run without a new snapshot or reachable source, using complete reconciled local policy/control evidence; it rechecks eligibility and live locks before each deletion. It never overrides retention, pins, uncertain generations, or missing metadata. If no safe candidates exist, require added capacity or a separately explicit policy/pin change. ENOSPC may also prevent a durable deletion intent/rename: fail safely rather than claim prune is guaranteed to work on a completely full filesystem. Keep control/journal headroom and test its exhaustion; adding storage remains the fallback. Do not introduce automatic emergency deletion to resolve this condition.

Run periodic checksums and full `integrity_check` on retained snapshots, with bounded I/O and a rotating schedule; validate at least the newest snapshot for every protected instance weekly. Discovering corruption never silently promotes an older snapshot as current: report the newest known good recovery point and the incident. USB disconnect, read-only remount, fsync failure, and a filesystem that cannot provide the required atomic/durability semantics are failures, not warnings that still yield success.

The first supported archive filesystem should be a local Linux filesystem with tested POSIX permissions, rename, fsync, and locking behavior (e.g. ext4). NFS/SMB/FAT-like storage requires explicit later qualification. Preserve archive receipts so a replacement Pi can reconstruct history, but restore notification/enrollment state separately to avoid assuming missing configuration means no expected sources.

## 12. Restore verification and recovery procedure

Separate three claims: transferred/checksummed snapshot, SQLite-valid snapshot, and application-restorable backup. `quick_check` alone proves only part of the second claim.

### 12.1 Browse backups and pull one locally

This is a first-release user workflow, independent of automated restore drills:

```sh
# Browse actual history on the Pi, including retired sources/generations.
deploy backup-worker list home-backup

# Narrow history to a particular application instance.
deploy backup-worker list --source vps-main --instance foodlog-prod home-backup

# Download the exact ID shown by list into a new local directory.
deploy backup-worker pull --backup <backup-id> --output ./restore-test home-backup

# Explicitly select the latest snapshot of the active database generation.
deploy backup-worker pull --latest --source vps-main --instance foodlog-prod --output ./restore-test home-backup
```

Suggested list output:

```text
BACKUP ID    SOURCE     INSTANCE       ENV   GENERATION  SNAPSHOT TIME             SIZE    VALIDATION
b-7f31...    vps-main   foodlog-prod   prod  db-21...    2026-10-31 10:00:05 CET    24 MiB  quick-check OK
b-18ab...    vps-main   foodlog-prod   prod  db-21...    2026-10-31 09:00:04 CET    24 MiB  quick-check OK
b-a294...    vps-main   foodlog-test   test  db-08...    2026-10-30 17:00:03 CET     3 MiB  quick-check OK
```

Actual IDs must be complete and copyable, unlike the abbreviated illustration. Assign each committed receipt an opaque archive-wide unique backup ID; preserve its original source snapshot ID separately if needed. Resolve IDs inside the selected archive, never as user-supplied paths. Reject ambiguous prefixes rather than guessing.

Listing contract:

- Newest-first stable ordering using recorded worker request time/capture sequence, source identity/alias, app/environment/service, database generation, snapshot time, size, release identity, and validation status. Show source-reported time separately and flag clock uncertainty; a source clock jump must not change which capture is latest within an instance.
- Support `--source`, `--instance`, `--generation`, bounded `--limit` (default 50), and an opaque continuation cursor. JSON includes schema version, archive UUID, completeness, and next cursor. Human output shows pagination/truncation. Bind cursors to archive, filters, and a listing watermark so new arrivals do not duplicate pages; concurrent retention may still remove entries.
- Include retired sources and old generations by default. Resolve historical aliases from archive enrollment records, not only current YAML. Accept source UUIDs for renamed/retired sources; ambiguous aliases are errors.
- Read receipts/catalog and check bundle presence without re-hashing every database. Distinguish validation recorded at creation, later integrity checks, known corruption, missing files, and unknown status. `quick-check OK` is a recorded result with a timestamp, not proof of revalidation during listing. Read availability is independent of backup-writer health: an archive that is full, read-only, or protected by paused scheduling can still be listed/exported when its files and existing shared locks are readable. Never require free archive space, a successful new backup, or working notifications merely to retrieve an existing snapshot.
- Exclude incoming/trash bundles from successful history and show their diagnostic counts separately. Missing disk or unreadable/corrupt catalog returns unavailable/partial diagnostics, never a successful empty list. Verified empty history is distinct.
- No source access, backup creation, deployment changes, pruning, or silent catalog repair. Reconstruction requires the separate reconciliation operation.

Selection and download contract:

1. Require exactly one of `--backup <id>` and `--latest`. Latest requires `--source` and `--instance`, and selects that instance's active enrolled generation unless `--generation` explicitly requests historical data. If no generation is unambiguous, show choices and stop. A specific ID remains usable for retired sources/generations.
2. Resolve latest once on the worker and acquire its shared bundle lock in the same coordinated selection step; write a diagnostic retention lease only if state is writable. Return the exact ID, source, environment, generation, time, size, and expected hash before transfer. Retries within an invocation keep that ID. A new invocation may select a newer snapshot and must show its identity.
3. Never silently skip a corrupt/missing newest snapshot and return an older one as latest. Fail and offer explicit older IDs. Old but valid snapshots remain downloadable; display age/staleness and known restore-check failures without presenting them as current data.
4. Connect directly from the operator machine to the worker using its configured SSH account and strict host-key checks. Setup installs a versioned worker archive helper from the same deploy codebase. It can query/export committed archives while the scheduled container is stopped and the original VPS is offline. No inbound web endpoint, original source credentials on the laptop, or container Podman socket is required.
5. Reuse bounded framing, hashes, completion checks, cancellation, and deadlines with a distinct archive-export protocol role. Revalidate archive UUID, receipt identity, mount, and selected bundle. Source export credentials must not authorize worker archive access. Initially use the configured operator-to-worker SSH access; a future read-only archive key can use the forced-command pattern.
6. Without `--output`, use `./deploy-backups/<backup-id>/`. Explicit output is a **new local directory**, resolved on the operator machine, not a database filename. Print its absolute path. Never derive a replacement destination from `deploy.yaml` or overwrite an existing file, directory, symlink, or SQLite database/sidecars. No `--force` in v1; select another directory.
7. Preflight local capacity/inodes and SQLite validation prerequisites. Receive into a private temporary sibling directory on the destination filesystem, with exclusive mode-0600 files and a mode-0700 directory. Reject unsafe/symlink destination components. Final publication must atomically refuse an existing destination, including one created during download; use a supported no-replace primitive or fail safely. Linux is the initially tested local platform; qualify others before claiming support.
8. Verify byte count, checksum, identity, completion, and successful remote exit; run local read-only SQLite `quick_check`. Flush `database.db`, the unchanged original `receipt.json`, and a separate `download.json` recording archive/backup IDs, verification, and download time. Atomically publish the complete directory and sync its parent before reporting success.
9. Keep the shared per-backup OS lock for the bounded export and release on completion/cancellation. The lease record is diagnostic/recovery metadata; expiration never overrides a live shared lock. Terminate overdue export processes before releasing their locks, and clean stale lease records only after checking actual lock availability. If pruning won before acquisition, return `backup unavailable`, never a substitute. Archive exports take no deployment locks on the source VPS.
10. On interruption, disk full, corruption, or cancellation, publish no final directory and clean only this operation's temporary files. Crashes leave identifiable partial directories for scoped cleanup. No partial-download resume in v1. A pull does not refresh backup freshness, count as a worker restore drill, or generate a scheduled-backup success notification.

Successful output makes the next action concrete:

```text
Downloaded and verified: <backup-id>
Source: vps-main / foodlog-prod / prod
Snapshot: 2026-10-31 10:00:05 CET
Local database: /home/user/project/restore-test/database.db
Receipt: /home/user/project/restore-test/receipt.json

Point your local test application at this database to try the snapshot.
Application startup has not been run by this command.
```

The downloaded database is a disposable copy the user may open, migrate, or change locally. The archive remains unchanged. Later local edits naturally invalidate the original snapshot hash; `download.json` records verification at download time. Do not automatically execute migrations/application commands, use production `.env`, replace the project's normal local database, or run hooks from receipts. A future convenience command for replacing a configured local database must be a separate deliberate operation using existing SQLite-safe backup/replacement rules.

### 12.2 Automated drills and production recovery

`restore-check` selects a committed immutable bundle, acquires its shared bundle lock and retention lease record, copies it into a new private disposable workspace, verifies the completed copy against the selected receipt checksum, runs full SQLite integrity validation, and optionally executes a worker-configured application read check. Keep the bundle protected until the result and any resulting verification pin are durably committed. Hooks are trusted local worker configuration with explicit argv/image, not code taken from Quadlet comments or source responses. Restrict time, memory, network, output, and credentials. Never mount production paths or use production `.env` automatically.

Bind the test to its actual input bytes. Resolve the selected backup once and carry its archive/backup ID, expected database digest from the receipt, and unique drill ID through the host orchestration request, workspace, and result. Create the destination file exclusively inside a fresh private directory; use an independent byte copy, never a hard link, symlink, or reused previous drill file. After completing/closing the copy, hash that destination before launching any checker or migration, and require equality with the selected receipt. Checking the archive file before copying is not a substitute. The destination must be a distinct regular file with no hard-link aliases; verify the host job mounts/passes this validated workspace. A checksum mismatch here is a drill-input/copy failure with no passing verdict or new recovery pin; it does not by itself establish corruption of the archived source.

Record `tested_input_sha256` in the immutable verification result and require it to equal the selected backup's recorded SHA-256 before that result can advance a pin. Application startup/migrations may subsequently change the disposable copy; that is allowed and must not replace its recorded input digest with a post-migration hash or trigger a false archive-corruption finding. Preserve the existing lock until result/pin publication completes. Test substitution with a different backup of the same schema: missing-database/incompatible-schema negative controls alone cannot detect the wrong capture. Also test a writable checker against a deliberately hard-linked candidate and require rejection before execution, then prove a normal isolated drill leaves archive bytes unchanged.

Start with generic SQLite verification for all instances plus optional per-source/service application checks configured on the worker. Support an architecture-compatible pinned checker image or worker-local executable. An x86-only application cannot automatically be executed on an ARM64 Pi; report application verification unsupported until an appropriate checker exists. Do not silently claim a successful application check based on generic SQL.

Container-based application checks are separate, explicitly configured rootless host Quadlet jobs, launched by a narrowly scoped host orchestration command. Do not mount the Podman socket into the backup/notifier container to implement them. Each check sees only a disposable snapshot copy and required non-production inputs. Built-in SQLite checks run in the worker image. Resolve the exact local-check configuration during the restore phase, including source/service selectors and pinned check image/argv, and reject commands supplied by remote metadata.

Weekly drills should include an application-level read where configured and a measured restore duration. Drill failure is a separate immediate incident even when hourly backups succeed. Missing or overdue drill coverage appears in daily summaries. A restore check that writes to its disposable copy must never alter the archive or clear the checksum record of the original.

Make verification results reproducible: record backup ID/SHA-256, tested-input SHA-256 and drill identity, capture identity, application release/schema information, checker image digest or executable hash, argv/config contract hash, relevant tool versions, start/end times, and structural/application outcomes. Different check contracts are not interchangeable; a successful weaker or changed check cannot silently replace the previous contract's recovery pin. Late completion of an older capture's check cannot move a pin backward. Distinguish `passed`, `validation-failed`, and `inconclusive/execution-failed`: a missing checker image, timeout, or maintenance cancellation is not proof the database is corrupt. A definitive failing recheck updates current trust for that bundle and opens an incident while preserving historical results; an inconclusive recheck shows the last known pass with its age and the new execution problem. Neither outcome automatically removes retention protection or discards the file. Do not present a definitively corrupt file as currently proven-restorable.

`integrity_check` does not test foreign-key constraints. Include a separately reported `foreign_key_check` in the drill where supported, treating violations as an application/data-validation finding rather than deleting the snapshot or claiming transfer corruption. Maintain a separate structural result so the backup remains available for diagnosis/recovery. [SQLite integrity-check scope](https://sqlite.org/pragma.html#pragma_integrity_check)

Application check acceptance must include negative controls: the configured check must fail when the selected database is absent and when given an intentionally incompatible application database. This catches a script that exits zero without using the disposable snapshot or accidentally reads a different configured database. Trusted checker code still defines application correctness; these controls do not prove arbitrary business invariants. Default check execution has no network access, production credentials, or writable archive mount, so local restore validation cannot inadvertently send production email/webhooks or run external scheduled jobs.

Document a real restore runbook: identify correct source/environment/generation and pre-migration release, verify the bundle, stop/quiesce the correct destination application, preserve its current database using existing protection, restore through SQLite-aware replacement, preserve ownership/mode, handle WAL/journal through SQLite, start the matching application version, verify application reads, and keep rollback material. Do not automatically restore production in this feature.

SQLite-only backups do not include uploaded files, application secrets, external services, or a consistent transaction spanning multiple SQLite files/ATTACH databases. Configuration and required application image/release must remain obtainable separately. The daily report should state the scope (`SQLite data`) rather than call it a full-server backup.

### 12.3 Recovery with lost worker state or an unavailable source

The archive must remain useful when the Pi's system disk is lost, its catalog is corrupt, and the VPS is simultaneously offline. Ordinary setup's source/Signal/first-backup checks must not be prerequisites for this path.

```sh
# Preview archive contents and recovery problems without changing the archive.
deploy backup-worker recover --scan --archive /mnt/backup/deploy home-backup

# Explicitly construct a new recovery catalog; preserve the damaged old state.
deploy backup-worker recover --apply --archive /mnt/backup/deploy home-backup

# Existing read commands then work against that recovery catalog.
deploy backup-worker list home-backup
deploy backup-worker pull --backup <backup-id> --output ./restore-test home-backup
```

The operator needs access to the archive host and a compatible trusted reader helper, not access to any original source. Recovery can bootstrap only that native reader on a replacement host using the existing operator SSH connection; it does not install source keys, require a registry image, link Signal, enable timers, or send healthy heartbeats. Also support `recover --local --scan|--apply --archive <path>` without a worker argument when the disk is attached to the machine running deploy. Here `--archive` names an explicit operator-selected directory, never a path accepted from a source export request. Dry-run never installs the helper.

Complete the local-disk workflow with `list --local --archive <path>` and `pull --local --archive <path> --backup <id> --output <new-directory>`, both under `deploy backup-worker` and without a worker positional argument, `server.yaml`, or SSH. They reuse the same reader, identity/locking checks, filters, and no-overwrite publication rules. Exact-ID reads may scan bounded receipt metadata without a persistent catalog; report scan limits/incomplete scopes instead of silently omitting them. When `recover --local --apply` needs a catalog, accept `--state-dir <new-private-directory>` on writable local storage, print it, and accept the same selector on subsequent local reads. Never put required recovery state onto the read-only archive. Reject `--local` combined with a remote worker argument. The local reader must ship in the CLI artifact, so an attached disk does not require a container registry or original Pi to retrieve bytes.

Scan validates archive identity/format, enumerates receipts/control/check records, and reports readable, missing, corrupt, duplicate, unknown-version, pending-publication, and trash entries. It is read-only with respect to archive contents; helper staging/reporting uses a separate private directory. Do not promote incoming files, finish deletions, repair receipts, or silently ignore unreadable entries. For read-only media use already-existing archive lock files. If trustworthy common locking cannot be established, require the archive to be taken offline/read-only with all other writers fenced before recovery/export; never create a private replacement lock namespace and assume coordination.

Apply creates a fresh catalog in a new state directory, preserves the original damaged store, records observed evidence and incomplete scopes, and enables **recovery/read mode only**. Validate database bytes before an entry can claim current verification; metadata-only scanned entries remain explicitly unverified until checked or pulled. Capture ordering is scoped to recorded writer epochs/sequences; reconstruct high-water marks from complete evidence before allowing future writes. Duplicate IDs with divergent receipts/hashes are conflicts, not overwrite candidates. `.trash` contents remain excluded from ordinary latest selection and preserved for explicit inspection.

Recovery/read mode allows listing and exact-ID pulls of individually verified bundles even if unrelated archive entries are damaged. It emits no backup success, performs no pruning, and does not require a healthy writable archive. If active generation/order is uncertain, refuse `--latest` with choices while still permitting exact-ID retrieval. Missing/stale control checkpoints preserve all uncertain scopes; missing pins never imply permission to delete. Show restored expected coverage and verification evidence as historical, not evidence the sources are currently protected.

Resuming backup writes is separate: fence the previous writer, take exclusive archive maintenance ownership, enroll a new writer epoch, reconcile state/policy/pins and source identities, rotate or re-establish credentials/monitor routing, and complete a current successful pass. Re-enable retention only after all protection metadata is consistent. A cloned/replacement Pi must not share success-report credentials with a still-active old Pi. No source availability is needed to export an old backup during this reconciliation.

Publish a plain-file rescue runbook as well: each completed directory contains a standalone SQLite database and receipt, so a compatible SQLite/checksum tool can verify a selected copy without a running deploy service. Rescue never needs to execute a source-provided script. Future releases must retain readers for archive formats still present in retained history or ship an explicit tested reader; a state-schema upgrade must not make old backups unreadable.

## 13. Failure and edge-case matrix

| Scenario | Required result |
| --- | --- |
| Prod and test share host/user | Separate service/target/database identities, archives, results, and notifications. |
| Another project uses the same service or target | Existing ownership checks reject mutation/export ambiguity; no automatic adoption. |
| Same database accidentally bound to two services | Detect identical resolved binding; refuse ambiguous export ownership until corrected. |
| Same host has several rootless users | Separate inventory entries/source identities; no assumption that one login sees all containers. |
| Rootful source configured for backup | Clear unsupported-profile error in v1; no implicit sudo escalation. |
| Marker missing on old deployment | Show unmarked/positively proven legacy-managed; next successful release adds metadata. |
| Marker malformed/duplicate/new schema | Visible diagnostic, no export, known instance remains expected. |
| Runtime label claims management without matching source | Show unmatched/drifted container; do not grant backup authorization. |
| Override changes mount/user | Detect relevant drift or unsupported effective config; do not back up a plausible but wrong file. |
| Failed release restores older Quadlet | Old metadata restored byte-for-byte; pending state blocks export until normal recovery. |
| Inventory changes between scan and export | Revalidate under deployment locks and reject changed generation. |
| Manual DB replacement outside deploy | Unsupported concurrent replacement reported where detectable; no false locking guarantee. |
| App writes to SQLite WAL continuously | Consistent API snapshot, bounded busy handling; no raw file copy. |
| Source DB missing, zero-byte, or truncated due path typo | Explicit pre-backup source/header check fails; a quick-check-valid empty destination cannot bypass it. Initialized databases with zero rows are a distinct case. |
| Source DB locked/corrupt/SQLCipher/custom VFS | Specific unsupported/busy/corruption failure; no silent fallback to copying bytes. |
| WAL snapshot would create sidecars during read-only validation | Normalize the private snapshot through SQLite before hashing; archive reads never mutate committed bundles. |
| Validator lacks a required collation/function/schema feature | Unsupported validation, not established corruption; no false success, dynamic source extension loading, or automatic deletion. |
| Backup completes but delays production writes excessively | Fail profile qualification or adjust the snapshot implementation; a successful backup deadline alone is insufficient. |
| Container UID 65532 or restrictive directories | Test namespace access and snapshot ownership; never recursively change production ownership. |
| Source disk full during snapshot | Abort, retain old Pi copies, clean owned spool, immediate source incident. |
| Source helper killed or SSH disconnects | Source watchdog terminates work; cleanup requires an available OS lock, and no successful transfer is recorded. A storage-stuck process remains an explicit host fault. |
| Deploy starts during slow transfer | Transfer immutable completed snapshot after locks are released; preserve original release metadata. |
| Source reboot, changed host key or UUID | Reject identity change until explicit reconciliation; preserve old history. |
| Source DNS works only on laptop | Setup fails container-context connectivity test with actionable host override guidance. |
| Outbound SSH blocked/home internet down | Retry within budget; queue local alerts; external missed-run monitor reports the outage. |
| One of several sources fails | Continue others, record partial coverage, no green aggregate heartbeat. |
| One source consumes the run deadline | Record skipped work and resume fair scheduling next pass; alert incomplete coverage. |
| Pi sleeps/reboots during a run | Recover expired local run state; perform one current catch-up; no invented historical backups. |
| Timer disabled or user session disappears | Linger/timer health visible; external monitor catches silence. |
| Manual run overlaps timer | One lock owner; second invocation reports existing run, not a second success. |
| Notifier overlaps backup | Short independent state transactions and delivery leases; no backup-wide notification blocking. |
| External disk missing or wrong disk mounted | UUID/mount checks fail before writing; alert using separate local state. |
| Disk unplugged mid-transfer/fsync | No published success; keep old bundles; incomplete data remains identifiable. |
| Disk runs out of inodes or becomes read-only | Explicit storage incident; no deletion outside policy. |
| Power loss after rename, before state commit | Reconcile final durable bundle; no duplicate promotion or deletion. |
| Partial protocol, wrong checksum, oversized stream | Abort/quarantine incomplete bundle, no success/retention. |
| Backup timestamp in the future or clock jumps | Mark time uncertainty, suspend age-based deletion and misleading freshness. |
| Corrupt receipt/state store | Fail closed for retention; preserve archive, report degraded health, rebuild from validated receipts explicitly. |
| Pi-local state disk full while archive has space | No false state commit; bounded emergency alert/journal and independent monitoring; no silent state reset. |
| Archived snapshot develops bit rot | Integrity incident and explicit newest-known-good timestamp; do not overwrite history. |
| Backup opt-in removed or service disappears | Stop exports if no longer authorized, keep expected record and alert until retirement. |
| Worker config removes a source | Explicit retirement/reconcile step; no silent successful shrink of coverage. |
| Service renamed or target path changed | New history/generation with explicit mapping; old archive stays accessible. |
| Signal account unlinked/session expires | Persistent transport incident, fallback if configured, guided relinking. |
| Signal native library fails on ARM64 | Fail provider qualification/setup; preserve backup operation; no silent provider switch. |
| Signal relay upgraded with incompatible state | Controlled image/state migration and verified rollback; no concurrent access to state. |
| Provider accepts message but response is lost | Retry may duplicate; stable event ID and documented at-least-once behavior. |
| Report exceeds provider limits | Send one complete essential summary with explicit omitted-detail counts; no multipart partial-success accounting. |
| Outage batch covers only some incident events | Acknowledge only durable batch members; remaining events stay pending and visible. |
| Telegram token invalid/bot blocked/group permission lost | Actionable delivery failure; no endless tight retry. |
| Telegram HTTP 200 with JSON error/429 | Inspect API result and backoff hint; not a false sent status. |
| Both providers fail | Durable outbox, degraded delivery health, external daily-delivery check alerts independently. |
| Failure recovers before a queued notification sends | Send combined failed/recovered event with timestamps; no stale failure-only or green-only message. |
| Daily report queued before newer failure | Refresh state before sending; never deliver an obsolete green summary. |
| DST duplicate/missing report time | One logical daily event per configured local day; explicit catch-up behavior. |
| Retention fails after successful backup | Backup remains successful; separate cleanup incident and capacity risk shown. |
| Listing while source VPS is down or scheduled container is stopped | Read committed worker archives directly; no source access or new snapshot required. |
| Listing while archive disk/catalog is unavailable | Return unavailable/partial diagnostics, not a successful empty list. |
| Pull races retention | Atomically select and lease the exact bundle; export that ID or return unavailable, never substitute another. |
| Latest spans prod/test or multiple generations | Require source/instance and an unambiguous generation; show exact selected identity. |
| Selected newest snapshot is corrupt | Fail verification, without silently substituting an older snapshot. |
| Local output exists or appears during publication | Refuse replacement atomically; preserve existing local data and SQLite sidecars. |
| Laptop disk fills, SSH breaks, or pull is cancelled | Publish no final bundle; clean only owned partial files; archive remains unchanged. |
| User modifies downloaded database for testing | Only the local copy changes; original receipt remains snapshot provenance. |
| Restore drill fails while backups succeed | Immediate restore incident; daily health is degraded. |
| Drill workspace contains another snapshot with the same schema | Destination hash/identity check rejects it before checker execution; no false verification result or pin for the selected backup. |
| Disposable drill file is hard-linked to archive data | Reject before running a writable checker; only an independent new copy is admissible. |
| Pi disappears entirely | External monitor alerts; no claim that a local fallback can handle power loss. |
| Pi/source account compromised | Explain trust boundary: checksums detect transfer corruption, not maliciously fabricated data; archives on a compromised writable Pi can be lost. |
| Old snapshot/receipt returned for a new run | Reject mismatched request nonce/run identity; importing a historical bundle never refreshes freshness. |
| VPS clock jumps forward | Use worker request time and capture sequence; show source skew without sorting stale data as newest. |
| Lease timestamp expires while export is still running | Live shared OS lock still blocks pruning; terminate overdue exporter before cleanup. |
| New snapshots pass quick-check but fail application restore | Preserve the last application-verified recovery pin and maintain the restore incident. |
| Worker fails before starting or timer is manually disabled | Bounded host health probe records the failure; notifier cannot infer healthy state from an old run. |
| Concurrent key-manager update during setup rollback | Compare expected installed state and remove only owned entries; never replace a newer whole authorized-key file. |
| Same Pi public key also has an unrestricted authorization | Reject enrollment conflict and prove effective restrictions with the actual credential. |
| Worker image is rolled back after newer state writes | Preserve new history; use schema-compatible forward repair/reconciliation instead of restoring stale state. |
| Inventory/config changes while backup pass is running | Mark aggregate coverage incomplete until revisions are reconciled; do not publish green for the old set. |
| Slow previous run shifts a completion-interval heartbeat | Report true worst-case detection delay; prefer a matching calendar schedule/grace. |
| A storage-hung process survives timeout/SIGKILL temporarily | Do not steal its locks or pile up more work; mark the scope stuck and rely on independent monitoring if the host is unresponsive. |
| Setup starts its first backup while holding exclusive schema lock | Release mutation locks before launching/waiting for normal jobs; retain only the separate coordinator lock. |
| Pi-local state and source VPS are both unavailable | Recover a catalog from archive evidence and allow exact-ID pulls without first restoring scheduling, source access, or messaging. |
| Two helper/state directories access the same archive | Use the archive's common lock namespace and reject a second writer identity without exclusive takeover. |
| State loss also loses retention pins/check results | Reconstruct from archive checkpoint/verification records; incomplete evidence disables deletion. |
| Delayed checkpoint writer follows a newer retirement/pin commit | Serialize control commits, reject stale revisions, and preserve acknowledged evidence through subsequent state loss. |
| Archive is full or read-only but its backups are readable | Permit listing/verified export; writer-health failure is not an access precondition for recovery. |
| Normal redeploy changes release ID while DB binding remains stable | Preserve provenance but do not invalidate the coverage fingerprint on volatile fields. |
| Restore script returns success without reading the supplied DB | Negative controls reject the check contract; do not mark application-restorable. |
| Scrub/restore/pull jobs compete with the scheduled backup | Apply combined resource/admission limits, defer maintenance, and keep notifications responsive. |
| Large outage creates more notifications than one pass can send | Batch urgent summaries, persist individual incidents, expose queue delay, and do not claim a hard per-message deadline. |
| Read-only export cannot write a diagnostic lease | Existing shared OS lock is sufficient; no writable main catalog or archive is required. |
| Disk attached locally after Pi loss | Local list/pull use the shipped reader without server inventory, SSH, or registry access. |
| Capacity failure prevents the next publication and automatic prune | Show safe manual prune candidates/deficit; preserve pins and fail safely if even deletion bookkeeping cannot persist. |
| Pause expires during reboot or activation fails | Durable expiry/catch-up state retries through the ordinary run lock; timers and notifier remain enabled. |
| Concurrent setup or interruption before unit staging | Coordinator and journal already cover enrollment/key/helper mutations; preserve active configuration and reconcile observed outcomes before retrying. |
| Retired service still has an enabled marker or reappears | Persistent identity-scoped exclusion wins over discovery; only explicit reactivation starts coverage again. |
| Several daily reports missed during downtime | One accepted current catch-up accounts for missed periods without claiming they were delivered on time or keeping monitoring permanently failed. |
| Coverage and delivery use the same monitor check | Reject enrollment; a message acceptance must not clear a backup-coverage failure. |
| Old heartbeat retry arrives after a newer evaluated result | Suppress obsolete local sends, serialize per check, and expose uncertainty after ambiguous remote acceptance. |

## 14. Implementation phases and reviewable gates

### Phase 0 — prove risky dependencies and freeze contracts

- Verify ARM64 rootless Quadlet worker behavior on a disposable Pi-class host, including effective UID permissions, timer reactivation, power-cycle recovery, and external disk absence.
- Qualify Signal: pinned ARM64 image, link flow, self/group notifications, native dependencies, sustained daemon receiving, memory use, and upgrade/state compatibility. Telegram adapter spike with a test bot is separate; real sends require an explicitly configured test destination during implementation.
- Choose the local state-store Go dependency after checking ARM64 support, licensing, build reproducibility, and transaction behavior. Pin it; do not fetch dependencies at runtime.
- Define JSON/YAML/protocol schemas, exit-code/error classes, backup states, report/incident rules, and time/capacity limits. Record any change from this plan as a deliberate decision.
- Prove effective oneshot timeouts and child cleanup; test helper-owned source watchdogs, inherited deployment lock descriptors, shared host/container file locks, and durable state-store settings before depending on them.
- Race setup from two clients before enrollment/key creation and interrupt each early mutation; prove one coordinator/journal covers the entire lifecycle and incomplete preparation preserves the active installation.
- Run a minimal full setup state machine through activation and a normal shared-lock job; prove it releases exclusive schema/archive locks before waiting for that job. Qualify archive-local lock sharing across host/container/recovery helpers.
- Qualify source initialization checks, private WAL-to-DELETE snapshot normalization, validator capability/error classification, and measured write impact for WAL and rollback-journal sources. Record the source size/load/latency envelope.
- Run the existing tests as a baseline before code changes. None are required merely to add this plan document.

Gate: small reproducible prototypes and recorded results prove the Pi/Signal path; unresolved platform issues have a concrete solution before building the complete onboarding UX.

### Phase 1 — server inventory and application references

- Add v2 inventory types, strict loader, selection, explicit path resolution, connection resolution, and compatibility adapter for legacy server files.
- Update all environment-consuming commands, not only releases: DB operations, logs/status, maintenance, rights, prune, service actions, and dry runs.
- Update `server init/provision/validate` selection semantics and migration docs. Infrastructure-free worker entries validate without requiring Traefik.
- Add complete examples for prod/test sharing `vps-main`, with independent data directories.

Gate: existing legacy fixtures and new reference fixtures pass; ambiguous connection sources and cross-environment collisions fail before remote actions.

### Phase 2 — metadata and `deploy ls`

- Implement common metadata encoding/strict parsing, roles, reserved labels, source inventory representation, and JSON/human renderers.
- Integrate metadata into both release modes and all managed container templates, preserving exact rollback semantics.
- Build read-only inventory across actual Quadlet search paths and Podman/systemd state, with explicit completeness/drift diagnostics.

Gate: real rootless generator tests prove markers do not alter service behavior; stopped, absent, orphaned, external, shadowed, malformed, and pending units display correctly.

### Phase 3 — restricted export and atomic archive

- Implement source enrollment, version negotiation, forced-command dispatcher, restricted-key provisioning, and bounded protocol.
- Bind fresh attempts to nonce/run/config/capture identities and separate source snapshot creation from archive retrieval. Verify effective key restrictions, source watchdog behavior, and bounded remote output.
- Extract reusable snapshot logic while keeping old `db pull` and `db backup` behavior compatible.
- Implement independent source snapshot spools, early deployment-lock release, transfer validation, durable bundles, and crash reconciliation.
- Add expected-instance state and explicit source/database generation handling before enabling automatic discovery.
- Define archive-wide backup IDs, bounded listing/query/export protocol, and atomic export leases; share verified transfer primitives with the later local pull command.
- Define the archive recovery checkpoint and immutable verification-record formats before storing pins only in the local catalog; serialize control commits, reject stale revisions, and test conservative recovery from cross-store revision mismatches and delayed writers.

Gate: exports cannot execute arbitrary commands/read arbitrary paths; WAL consistency and race tests pass; injected crashes never produce a false successful backup.

### Phase 4 — worker setup and scheduling

- Build/publish reproducible deploy worker images for AMD64 and ARM64 and matching source helper artifacts through an explicit release process. No imaginary registry reference in generated examples.
- Implement setup journal, atomic configuration/Quadlet installation, worker identity/secrets provisioning, archive enrollment, mount checks, timer installation, status/run/logs, and idempotent reruns.
- Install the worker archive helper and implement `backup-worker list` and `pull`, including historical selection, pagination/JSON, local no-replace publication, checksum/SQLite validation, and independence from source availability.
- Provide archive-only recovery scan/apply and exact-ID retrieval with destroyed local state, offline sources, and read-only/full archive storage. Keep recovered installs in read mode until explicit ownership/policy reconciliation.
- Add worker deadline, retries, fairness, run recovery, short state-store transactions, and platform resource limits.
- Implement the host health probe/orchestrator so notifier decisions use fresh timer/unit/mount evidence, including failures before container startup. Use one cross-process lock/state policy across worker, host helper, notifier, archive access, and migration.
- Implement update/uninstall/key rotation; removal retains archives by default and revokes only owned source key entries. Define shared-helper reference accounting.

Gate: setup, interrupted setup, upgrade, failed upgrade, repeated setup, reboot, and disconnected storage pass on both architectures; offline laptop is irrelevant to scheduled backups. Listing and downloading work with the source VPS offline and the scheduled backup container stopped; concurrent pruning and existing local destinations cannot cause wrong-snapshot downloads or overwritten local data.

### Phase 5 — notifications and external monitoring

- Implement provider-neutral incidents/outbox, separate notifier unit, immediate failure path, six-hour reminders, recovery, and daily reports with timezone/DST handling.
- Ship Signal relay setup/linking and Telegram adapter/fallback. Add notification test and delivery-health status without conflating delivery with backup validity.
- Add external backup/report heartbeat adapters and setup diagnostics; no green status for absent monitoring qualification.
- Verify monitor arming, calendar/grace agreement, immediate external delivery-failure signals, and incident-specific recovery without recursive notification storms.
- Add a dedicated urgent-event wakeup path plus timer fallback, with bounded outage batching and visible queue-latency health.
- Redact sensitive transport errors and configure persistent bounded logs.

Gate: provider failures, ambiguous delivery, channel fallback, time changes, and actual missed-worker detection behave as specified. Local-only notification is not a completed offline-alert feature.

### Phase 6 — retention, drills, and operational documentation

- Implement safe per-instance retention previews/deletion, restore leases, managed trash, checksum scrubbing, full SQLite checks, and worker-configured application restore drills.
- Preserve full-integrity/application-restore recovery pins, and implement explicit catalog/filesystem publication/deletion reconciliation with live OS-lock protection.
- Persist exact checker contracts/results and tested-input digests, reject wrong-capture or aliased drill copies, reject ineffective checks using negative controls, report foreign-key findings separately, and enforce maintenance admission/resource budgets against scheduled work.
- Implement explicit retirement/reconciliation and archive reconstruction on a replacement Pi.
- Document source/worker credential rotation, Signal relinking/upgrades, disk replacement, monitor maintenance, source rebuild, generation changes, and restoring prod versus test.
- Add capacity estimates and status messages distinguishing snapshot validity from application recovery coverage.

Gate: full restore drill on an isolated environment, retention/crash tests, and recovery on a replacement worker succeed before production rollout.

### Phase 7 — staged rollout

1. Upgrade deploy and enroll source helpers without changing existing schedules/backups.
2. Redeploy one test application to introduce metadata; verify `deploy ls` and discover/export manually.
3. Set up the Pi with a test database and notification destination. Exercise first backup and restore, disconnect disk, block source SSH, stop relay, and stop the Pi to test external alerts.
   List its stored snapshots, pull an explicit backup ID to a fresh laptop directory, and point an isolated local test application at the downloaded database. Repeat with source SSH blocked to prove archive recovery is independent of the original VPS.
4. Enable production backup opt-in and redeploy it, keeping its service/data distinct from test. Verify one actual production snapshot through a disposable read check without modifying production.
5. Observe at least one daily report, a planned failure/recovery cycle, and one scheduled restore drill. Keep previous off-host backup arrangements until this acceptance window passes.
6. Explicitly retire redundant schedules after verifying coverage. Preserve their historical backups under their original retention contracts.

## 15. Code and package ownership

The implementation uses one Go module with these package boundaries. The root
`main.go` is a compatibility entry point; `cmd/deploy/main.go` is the canonical
build target. Both delegate to `internal/cli`.

| Area | Packages/files |
| --- | --- |
| Config and resolution | `internal/config/` |
| CLI and server selection | `internal/cli/`, `internal/deployment/server.go` |
| Metadata and discovery | `internal/quadlet/`, `internal/inventory/` |
| Source export and protocol | `internal/backup/source/`, `internal/backup/protocol/` |
| Shared DB/filesystem helpers | `internal/sqlite/`, `internal/safefs/` |
| Archive, state and reconciliation | `internal/backup/archive*.go`, `internal/backup/state.go` |
| Archive browsing/local pull | `internal/backup/archive_access.go`, `pull.go` |
| Worker execution/setup | `internal/backup/worker*.go` |
| Worker retention/restore | `internal/backup/retention_safety.go`, `internal/backup/restore.go` |
| Notifications | Providers in `internal/backup/notification/`; transactional outbox and monitor in `internal/backup/` |
| Deployment and legacy database operations | `internal/deployment/`, including embedded transaction scripts |
| Transport/process control | `internal/transport/`, `internal/process/`; restricted export SSH in `internal/backup/source/` |
| Helper packaging | `internal/backup/helper/`, `packaging/backup-worker/`, `scripts/build-backup-*.sh` |
| Tests | Feature-focused tests beside their package; shared fixtures in `internal/testutil/` |

Archive control, worker state and the notification outbox remain together because
their transaction and lock ordering rules need a single owner. Source capture,
protocol and provider delivery have independent package boundaries.

Avoid a generic plugin framework or distributed scheduler. Provider interfaces and a snapshot/export protocol are justified boundaries; broad infrastructure abstraction is not required.

## 16. Verification plan

### Unit and component coverage

- Legacy/v2 server files, inline/referenced environments, unknown keys, duplicate YAML keys, multiple documents, explicitly empty conflicting fields, relative path contexts, source alias changes, and missing inventory.
- Marker round trips, JSON duplicate keys, unsupported versions, control/newline injection, long records, reserved labels, incorrect roles, and byte-for-byte rollback.
- Inventory joins for custom container names, no running container, stopped containers, unrelated Podman containers, multiple rootless search paths, masks, symlinks, shadowed definitions, and partial command failure.
- Framing fuzz tests for lengths, premature EOF, trailing garbage, request/identity mismatch, extreme sizes, path traversal, shell metacharacters, Unicode identifiers, and unknown operations.
- Mock clock/provider tests for first-failure immediacy, incident coalescing, persistent reminders, recovery, fallback, ambiguous acknowledgment, 429/backoff, Telegram JSON errors, Signal RPC errors, and queue limits.
- Single-message report envelopes with long Unicode labels and provider size differences; preserve severity/counts/age/action, disclose omitted detail, and acknowledge only explicit alert-batch members.
- State/config schema upgrade and rollback, source-generation enrollment races, missing generation records, exhausted local state storage, and explicit expected-inventory reconstruction.
- Retirement exclusions survive repeated discovery, alias changes, service reappearance, new generations, source re-addition, and checkpoint recovery; explicit reactivation validates current bindings and never marks retirement as backup recovery.
- Multi-day missed reports plus accepted/failed catch-up, restart just before/after the configured report time, same-day daily-boundary evaluation, crash around acceptance, and external delivery recovery without fabricated historical acknowledgments.
- Daily boundary, DST changes in Europe/Vienna, UTC/local conversion, time rollback, long outage catch-up, timezone changes, stale green report invalidation, and report-state migration.
- Retention union semantics, protected newest snapshot, corrupt receipts, missing instances, retired generations, leased restores, and interrupted trash cleanup.
- Archive listing filters/cursors/completeness, historical aliases, full IDs, ambiguous prod/test/generation selection, latest resolved once, and recorded versus current validation status.
- Local pull ID/receipt/hash agreement, destination no-replace races and symlinks, default/explicit path semantics, interrupted downloads, local capacity failures, and immutable original receipts.
- Replay/wrong-run responses, source-clock jumps, config/inventory changes during a pass, observed versus imported timestamps, and monotonic capture ordering for latest selection.
- Incident-specific resolution and self-referential notification failures; monitor activation before first success, calendar versus interval detection delay, and immediate external failure when all messenger paths fail.
- Coverage fingerprint excludes observation time/runtime IDs and same-binding release changes; real authorization/binding changes still invalidate a pass.
- Restore contract negative controls, foreign-key findings, changed checker hashes, late older verification results, and conservative reconstruction of missing pins.
- Same-schema wrong-snapshot substitution in the drill workspace, mismatched job/result IDs, hard-link/symlink aliases, and writable application checks; require the tested-input digest to match the selected backup before execution, and preserve the original archive hash.

### Real filesystem and process tests

- Use actual SQLite WAL writers and known committed/uncommitted rows. Prove the snapshot is a consistent committed state, source writes continue, and missing paths never create source databases.
- Exercise zero-byte/truncated sources, initialized empty tables, WAL-only committed changes, and custom-collation/feature-dependent schemas. Verify standalone snapshot normalization changes only the private destination, hashes its final bytes, and creates no sidecars during archive reads. Treat missing validator capabilities separately from corruption; test validation output as well as exit status.
- Measure source transaction latency, busy errors, WAL growth, and deployment-lock duration under the supported load/size envelope in both journal modes.
- Inject SQLite busy, slow writer, disk full, inode exhaustion where supported, permission failure, process kill, and transfer timeout. Fake command output alone is not evidence of valid snapshot recovery.
- Crash at each boundary: spool creation, snapshot completion, protocol header/body/footer, file fsync, directory rename, parent fsync, state commit, outbox claim, provider acceptance, receipt reconciliation, and retention rename/delete.
- Race release/push versus export and assert source locks match existing keys; prove a completed snapshot transfer no longer holds deployment locks.
- Corrupt local state while preserving receipts, then reconstruct conservatively. Verify secrets are absent from stdout/stderr, JSON status, dry runs, journals, and notification payloads.
- Race archive selection/export with pruning and lease expiry; kill downloads around local file sync, directory publication, and parent sync. Prove no existing local database or sidecars are replaced and no partial file appears under the completed destination.
- Keep a slow export alive past its metadata lease deadline and prove its OS lock still blocks deletion. Crash publication/deletion between each catalog/filesystem transition, and preserve the last application-verified pin when every newer drill fails.
- Upgrade while notifier/host helper/archive access are active; test migration quiescing, incompatible-schema rollback after new writes, authorized-key compare-and-swap conflicts, unrestricted duplicate keys, and original-command/environment bypass attempts.
- Exercise setup's complete prepare/activate/test/finalize sequence and SSH interruption/boot recovery, including concurrent clients before key generation, lost source-install acknowledgments, and source helper changes by another worker. Distinguish incomplete preparation from incomplete migration. Assert the normal test run can acquire its shared schema lock.
- Delete/corrupt Pi-local state while keeping the archive, remove original source connectivity, and use recovery scan/apply followed by exact-ID pull. Repeat on a full and read-only archive, with incomplete checkpoint history and unrelated damaged bundles.
- Open the same archive through two different state directories/processes; prove common bundle locks still protect exports, unauthorized second writers are refused, and takeover requires fencing plus exclusive maintenance ownership.
- Delay an old checkpoint producer until after a newer retirement/pin commit, then remove local state and reconstruct; acknowledged protections must survive. Race control commits with explicit pinning/pruning to prove the lock order and stale-revision checks. Inject crashes between checkpoint/state/pin updates, and verify incomplete metadata blocks retention. Run backup, scrub, pull, and relay together under constrained memory/disk; defer maintenance rather than lose backup/alert availability.

- Deny all catalog/lease writes and open an enrolled archive read-only; exact-ID export must still coordinate with a competing pruner through existing locks. Include a bundle that has never previously been pulled.
- Attach the archive locally with no `server.yaml`, worker host, or registry connectivity; exercise scan, optional recovery catalog, list, and verified local pull using the shipped CLI reader.
- Exhaust free bytes/inodes before the next snapshot; preview/apply only eligible cleanup without contacting sources, and fail safely when deletion intents cannot be persisted. Prove protected snapshots survive.
- Pause across expiry/reboot, race expiry with manual resume, and fail the first catch-up activation; admit one catch-up, keep notifier active, and preserve prior backup ages/incidents.
- Supply identical heartbeat identities through different secret files/URL spellings; reject conflicting checks. Delay failure/success responses and change the desired result during retries; no obsolete locally queued outcome may overwrite current evidence.

### Isolated integration environments

- Supported rootless Podman/systemd versions on AMD64 and a real ARM64 Pi-class Linux host; 65532-owned application data; SELinux coverage where supported.
- Source SSH forced-command tests: reject shell, arbitrary file read, SCP/SFTP, forwarding, PTY, environment-based bypass, unknown protocol, and a key enrolled for a different worker policy.
- Full two-environment scenario: prod/test on the same VPS/account, separate DBs; enable only prod, then both, then disable test and verify retirement warning/history preservation.
- Fresh setup, rerun setup, interrupted setup, credential rotation, helper coexistence, relay upgrade, worker update rollback, timer restore, reboot, and disabled user lingering.
- Mount absent, wrong filesystem mounted at same path, mid-run unplug, read-only remount, power interruption, archive migration, and replacement Pi recovery.
- Real Signal link/send and Telegram test send to explicitly designated test recipients. Verify the human sees the expected destination/format; provider acceptance alone cannot validate that UX.
- Stop the Pi entirely and confirm the independent monitor sends an alert without the Pi's Signal relay. Restore the Pi and verify current-state recovery, no stale success heartbeat replay, and no daily message flood.
- Restore a snapshot into an isolated matching application and measure actual recovery time.
- List and pull exact/latest snapshots from a worker with source SSH blocked and its backup container stopped; verify checksums locally, run an isolated local application against the disposable copy, and confirm the remote archive and normal local application data remain unchanged.

Run the existing project checks after implementation:

```sh
go test ./...
go test -race ./...
go vet ./...
make test-image-deploy-isolated
make test-deploy-vm-crash
```

Add dedicated opt-in backup integration/crash targets that refuse production source configuration. Do not require messenger credentials or send real messages in ordinary unit tests. Document test coverage and unavailable hardware/provider checks explicitly; never claim a tested ARM64/Signal release from mocks alone.

## 17. Acceptance checklist

- [ ] Existing application and server configurations work without forced migration.
- [ ] Prod/test can share one VPS and user with unambiguous identities, data paths, archives, and alerts.
- [ ] All generated container roles receive parseable versioned markers; failed releases preserve old bytes.
- [ ] `deploy ls` shows marked and unmarked resources, including stopped/absent containers, without mutating the host.
- [ ] Setup installs a real ARM64 rootless Quadlet worker and timer, survives reboot, and reruns idempotently.
- [ ] Worker credentials permit only authorized inventory/export; no administrator private keys reach the Pi runtime.
- [ ] Live WAL snapshots validate; backup operations never stop/recover/restore the production application.
- [ ] Zero-byte sources cannot become apparently successful empty backups; published snapshots are standalone and validation performs no archive writes.
- [ ] Validator incompatibility cannot be reported as proven corruption or healthy coverage; source write impact passes the recorded load/latency qualification.
- [ ] No incomplete transfer or interrupted publication is reported as a backup success.
- [ ] Slow transfers do not hold deployment locks after snapshot completion.
- [ ] Missing disks, missing instances, changed identities, and empty discovery never produce false green coverage.
- [ ] Retention cannot delete the last valid recovery point, foreign data, or a leased restore snapshot.
- [ ] `backup-worker list` shows committed history, including retired sources/generations, with filters, copyable IDs, pagination, and honest validation/completeness status.
- [ ] `backup-worker pull` downloads an exact or explicitly selected latest snapshot directly from the worker, even with the source VPS offline.
- [ ] Pull verifies checksum/SQLite integrity and atomically publishes a new local bundle without overwriting existing files or changing the project's local database.
- [ ] Retention/export races and interrupted pulls never substitute a different snapshot, delete an actively exported bundle, or report partial local data as successful.
- [ ] Signal works on the actual target architecture and has a documented relink/update path.
- [ ] First failures alert promptly, retries are bounded, repeated incidents are deduplicated, and recovery is explicit.
- [ ] One logical daily report is generated in Europe/Vienna, with documented ambiguous-delivery duplicate limits.
- [ ] Telegram can operate independently or as explicit fallback; provider failures do not invalidate good snapshots.
- [ ] An external monitor detects a powered-off Pi and failed daily notification delivery independently.
- [ ] Actual application restore verification succeeds in an isolated environment, or absent coverage is clearly shown.
- [ ] Status and documentation distinguish SQLite-only backups, notification acceptance, restore validation, and full-system recovery.
- [ ] No production provisioning or messenger send occurs as a side effect of validation, listing, dry-run, or ordinary tests.
- [ ] Fresh backup claims are bound to current requests and worker-observed capture time; old receipts or VPS clock skew cannot renew health.
- [ ] Host timer/mount/pre-start failures reach the notifier through fresh bounded probes without exposing host-control sockets.
- [ ] Active exports remain protected beyond lease timestamp expiry, and the last application-verified recovery point survives failing newer drills.
- [ ] Setup/key rotation proves effective restricted access, preserves concurrent unrelated key edits, and migrates every shared-state participant safely.
- [ ] External checks are actually armed and their configured worst-case alert delay matches the documented policy.
- [ ] Full setup releases exclusive locks before its test backup and recovers from laptop disconnect without admitting incompatible state writers.
- [ ] Setup coordination/journaling begins before enrollment/key/helper mutations; interrupted preparation preserves compatible active jobs.
- [ ] Explicit retirement survives discovery and recovery; accepted catch-up reports resolve missed-period delivery monitoring without invented historical sends.
- [ ] Delayed checkpoint writers cannot erase acknowledged policy/pin/retirement evidence; read-only exports remain independent of control writes.
- [ ] Reports fit one complete provider message, retain essential failure information, and acknowledge only events actually included in an alert batch.
- [ ] Archive scan/recovery and exact-ID pulls work with lost worker state and an offline source; full/read-only media do not prevent read recovery.
- [ ] Archive-global locks and one-writer ownership survive separate host/container/recovery state paths; missing control/pin evidence disables pruning.
- [ ] Restore verdicts identify the exact checker contract and snapshot hash, and checks that ignore the supplied database fail acceptance.
- [ ] The actual disposable drill input is independently copied and hashed before execution; wrong captures and archive aliases cannot earn verification pins or alter stored backups.
- [ ] Volatile runtime observations do not invalidate otherwise complete backup coverage, and maintenance/notification bursts respect tested resource/latency limits.
- [ ] Read-only exports require no lease/catalog writes, and attached-disk local list/pull work without the lost Pi or server inventory.
- [ ] Capacity failure has a concrete safe cleanup path and honest no-reclaimable-space diagnostics; paused scheduling resumes through durable expiry/catch-up state.
- [ ] Monitoring rejects shared coverage/delivery check identities and suppresses stale local heartbeat retries while reporting ambiguous remote acceptance.

## 18. Deliberate limits and remaining implementation-time choices

No point-in-time recovery, distributed transactions across multiple databases, arbitrary container-volume extraction, rootful backup sources, SQLCipher, inbound bot administration, automatic production restore, cross-worker leader election, automated archive encryption/key escrow, or immutable second-copy storage in the first version.

The implementation must choose and record actual worker/relay image digests, supported ARM64 base/Java/native-library combinations, a Go SQLite state driver, and exact protocol/error constants after phase-0 qualification. Operational enrollment must supply actual storage, sources, Signal account/destination or Telegram bot/chat, and external-monitor URLs. Those unknown values do not block writing the software plan and must not be guessed into deployable credentials or registry references.

The preferred complete installation is: hourly SQLite pull backups to the Pi, Signal for immediate incidents and one daily summary, optional local Telegram fallback, and an independent external missed-run monitor whose alerts remain deliverable while the Pi is off.

## 19. Critical design review — 2026-10-31

The architecture remains appropriate, but the earlier plan was not yet precise enough to implement safely in several places. The following findings were corrected in the design during this review. They are not claims that implementation bugs have been fixed: the feature is still a plan, and each proof below is a release gate.

| Finding | Severity and consequence | Incorporated correction | Required proof |
| --- | --- | --- | --- |
| Freshness depended on timestamps without a complete request identity contract. | High: stale/replayed results or source clock skew could create false current coverage. | Worker nonce/run/config/capture identity; worker-observed request time; import time never refreshes health. | Replay, clock skew, crash import, and latest-selection tests. |
| The notifier could not observe host timers or the backup disk through its mounts. | High: disabled schedules/pre-start failures could remain hidden or a daily report could use stale health. | Bounded host health probe, observation age, independent notifier execution, explicit unknown/degraded states. | Disable timer, remove disk, fail pre-start, hang probe, and exhaust state storage. |
| Time-based export leases were underspecified. | High: expiry could allow retention to delete a live export or restore input. | Shared/exclusive per-bundle OS locks, consistent ordering, TTL only for diagnostic/recovery records. | Slow transfer beyond TTL, kill exporter, concurrent prune, inherited-FD tests. |
| Retention protected the newest snapshot but not the last application-proven recovery point. | High: apparently healthy new snapshots could displace all proven-restorable data. | Full-integrity and application-verification pins with explicit release semantics. | New quick-check passes plus repeated application-check failures must retain the old verified bundle. |
| Shared-state migration stopped too few participants and allowed a misleading old-state rollback. | High: notifier/host/helper writes or later backups could be lost or read through incompatible schemas. | Installation/schema lock, all-writer quiescing, durable state policy, no rollback over newer committed history. | Concurrent upgrade and forward-repair tests with backups, retirements, and deliveries after migration. |
| SSH installation described intended restrictions rather than proving effective authorization. | High: duplicate unrestricted keys, another key backend, or incorrect worker identity could bypass the intended boundary. | Validated public-key transfer, server-bound worker identity, restricted dispatcher, negative connection tests, conflict-safe rotation/rollback. | Actual Pi-key shell/forward/SFTP rejection and concurrent authorized-key editing tests. |
| Local timeout and lock behavior was assumed to cover remote/child processes. | High: a disconnected worker could leave source snapshots consuming disk or block deployments through inherited locks. | Source-owned process watchdog, explicit descriptor release, typed per-phase budgets, tested oneshot activation limit. | Kill local SSH, stall remote SQLite/stream, and acquire deployment locks while streaming. |
| Individually atomic filesystem/state changes were treated too loosely as one transaction. | High: publication/deletion crash windows could hide valid bundles or remove data based on stale catalog state. | Explicit pending/publication/deletion state transitions and conservative recovery; sync ancestors and use no-replace publication. | Crash injection at every cross-store boundary. |
| External alert delay and first-run arming were not fully specified. | Medium: an unarmed monitor or completion-interval grace could miss the promised outage window. | Match calendar schedule, derive actual worst-case delay, verify active routing before first success, signal total messenger failure externally. | First-run failure, slow prior run, schedule edit, and powered-off Pi tests. |
| Recovery and retry semantics could create noisy or misleading incidents. | Medium: unrelated successes could clear failures; notification failures could recursively notify themselves. | Incident-class recovery criteria, coalesced transport incident, due-work retries, persistent grace boundaries and priority. | Fault/recovery combinations, notifier restarts, and total provider outage tests. |
| Backup coverage could change while a run was executing. | Medium: a pass could report success for a reduced or outdated expected set. | Frozen run/config revisions and bounded final inventory reconciliation. | Add/remove/rebind an instance or retire a source during a slow pass. |
| Several requirements invited excessive scope or untestable isolation claims. | Medium: duplicating Quadlet parsing or assuming clean env equals sandboxing could delay delivery without providing the claimed protection. | Use system tools and a narrow authorized profile; isolate generic validation only to the extent actually qualified; keep hardware/provider gates explicit. | Unknown external configurations remain visible, supported profiles work, and unsupported isolation prerequisites fail clearly. |

Remaining qualification risks: real ARM64 Signal dependencies/account behavior, effective rootless mount permissions and SELinux integration, end-to-end power-loss durability, and external monitor routing. These require the phase-0/integration environments; document review cannot settle them. Build the vertical path (one declared application, one restricted source, one immutable archive, list/pull, one alert) early, then complete the production gates before replacing existing backups. No additional service or third-party platform should be introduced unless it resolves a stated requirement in this plan.

## 20. Second critical design review — 2026-10-31

This pass reviewed combined failures and interactions introduced by earlier safeguards. The first review's requirement to protect state with shared/exclusive locks was sound, but setup's ordered steps could wait for a normal test job while holding the very exclusive lock it needed. An isolated local `flock` experiment confirmed that conflict; it does not validate the eventual setup implementation.

| Finding | Consequence | Incorporated correction and acceptance proof |
| --- | --- | --- |
| Setup held its mutation lock while waiting for the first ordinary backup. | High: successful installation could hang or time out before completing its own acceptance run. | Separate coordinator lock from mutation locks, drain before exclusive acquisition, release before normal jobs, test full activation and disconnect/boot recovery. |
| Cold recovery depended on a catalog/setup path that could require the unavailable source. | High: readable backups might be inaccessible during the combined Pi-state/VPS failure they exist to cover. | Explicit archive-only scan/apply/read mode; exact-ID export without source, Signal, registry, or a new backup; test destroyed state and offline sources together. |
| Bundle locks lived in one local state directory. | High: another recovery/helper state directory could coordinate on different locks for the same archive. | Archive-local shared maintenance/index/bundle locks, enrolled writer identity/epoch, fenced takeover; test separate state directories against one archive. |
| Verification pins and expected-coverage evidence could disappear with the Pi's system disk. | High: recovery could lose deletion protections or invent an empty healthy policy. | Non-secret archive checkpoint and immutable verification records; cross-store mismatch disables retention; test crash/state-loss reconstruction and missing evidence. |
| Writer-health checks could accidentally block read recovery. | High: full/read-only media or paused scheduling might prevent downloading an otherwise valid backup. | Separate read prerequisites from write admission; list/pull remain available in recovery mode; test full/read-only archive and unrelated damaged bundles. |
| A coverage fingerprint could include volatile observations. | Medium: normal redeploys, uptime changes, or observation timestamps could prevent every run from becoming healthy. | Fingerprint only membership/identity/authorization/binding policy, preserve release provenance separately; test real versus irrelevant changes. |
| Application restore success lacked a precise check contract and negative controls. | High: a no-op checker or a check against another database could grant a false recovery pin. | Bind verdict to snapshot/checker/config hashes, require failing negative controls, distinguish foreign-key findings and stale rechecks; test ineffective and changed checkers. |
| Individually bounded jobs and notification passes did not bound combined load. | Medium: maintenance could exhaust the Pi, while a large incident burst exceeded an implied immediate-send guarantee. | Shared resource/admission policy, maintenance deferral, urgent-event wakeup with timer fallback, bounded batching and visible queue age; test constrained concurrent work/outage bursts. |

This pass adds no new external platform. It makes recovery independent of the normal control plane and tightens the existing host helper, archive format, and scheduler contracts. The plan is now large enough that further unstructured additions would themselves be a maintenance risk: implementation should proceed through the stated small vertical path, with remaining questions resolved by executable fault tests and recorded contract changes. Hardware, messenger, and real power-loss qualification remain outstanding; none is implied by document checks.

## 21. Third critical design review — 2026-10-31

This pass checked whether the documented recovery commands can actually honor their prerequisites, and whether monitoring can confuse notification success with backup success. Six remaining gaps were corrected in the normative contracts. These are design changes, not implemented fixes.

| Finding | Consequence | Incorporated correction and required proof |
| --- | --- | --- |
| Pull required a lease write despite promising read-only recovery. | High: a readable archive could become inaccessible because diagnostic state cannot be written. | Precreate durable bundle locks; read helpers use existing read-only-open locks and optional lease diagnostics. Test a never-before-exported bundle with archive and catalog writes denied. |
| Local recovery had no matching local list/pull route. | Medium: after attaching the disk, the CLI still expected a remote worker to retrieve its files. | Define local reader commands and an optional separate recovery state directory; test without inventory, SSH, original Pi, or registry access. |
| Capacity recovery mentioned explicit pruning without defining its executable contract. | Medium: post-publication retention cannot start when the next snapshot does not fit, and deletion itself may need free metadata space. | Add preview/apply semantics independent of source/new-backup availability, retain all protection gates, and fail safely if durable cleanup cannot proceed. |
| A bounded pause lacked an autonomous expiry/activation mechanism. | Medium: disabling a timer could leave the worker paused indefinitely, or reset freshness incorrectly on resume. | Keep timers enabled, persist pause and catch-up intent, serialize admission, and test expiry/reboot/activation failure. |
| Separate monitor configuration fields could resolve to one check. | High: delivery success could clear a real backup failure; another worker could mask an outage. | Validate distinct canonical check identities and document exclusive ownership; test duplicate secrets and alternate URL forms. |
| Heartbeat retry ordering was defined only for stale successes. | Medium: an old failure could be replayed after recovery, while concurrent publishers disagreed about current state. | One publisher per check, versioned current intent, stale-outcome suppression, and explicit ambiguous-transport limits; test reordered completions. |

A temporary local experiment confirmed that a shared `flock` on a read-only-open file blocks a competing exclusive lock and releases on close without writing a lease. It did not test read-only mounts, rootless containers, or power-loss durability. Document structure/examples are checked separately; the implementation and hardware/provider acceptance gates remain outstanding. These changes refine existing recovery, scheduling, and monitoring components and add no new daemon or external service.

## 22. Fourth critical design review — 2026-10-31

This review exercised SQLite behavior behind the proposed abstractions. It found three reproducible edge cases and one missing performance qualification. The following changes are incorporated into source capture, archive format, validator contracts, and acceptance gates; no production implementation was changed.

| Finding | Consequence | Incorporated correction and required proof |
| --- | --- | --- |
| An uninitialized source can pass through backup into a valid empty database. | High: a zero-byte replacement/path mistake could be reported as a successful backup despite the plan's stated rejection rule. | Check source initialization before the backup API; separately test missing, zero-byte, truncated, initialized-empty, and live WAL sources. Structural checks still do not prove business-data completeness. |
| A WAL-derived snapshot need not behave as a standalone read-only file. | High: a validator can create sidecars in an archive or fail only when the recovery disk becomes read-only. | Normalize only the private destination to DELETE mode before hashing, close handles, and qualify sidecar-free reads on read-only storage; never mutate source journal mode or published bundles. |
| A compatible file format does not imply a compatible validator. | Medium: a healthy custom-collation database can produce validation errors that look like corruption, while silently bypassing the check would create false confidence. | Explicit runtime/capability profile, unsupported-validator outcome, no source-loaded extensions or dummy implementations, and fixtures across source/worker/local readers. |
| Backup completion/timeout tests did not bound production write impact. | Medium: correct snapshots could still cause unacceptable application write latency or WAL growth. | Qualify incremental capture and the supported size/load envelope against measured application latency and storage behavior in both journal modes. |

Temporary experiments using Python's SQLite 3.46.1 demonstrated: a zero-byte read-only source produced a 4096-byte snapshot with `quick_check=ok` and no tables; both generic integrity checks rejected a valid indexed database whose custom collation was unavailable; and a read-only open of a WAL-derived snapshot created WAL/SHM sidecars. A separate experiment normalized only the private snapshot through SQLite, retained the expected row, left the source in WAL mode, and observed unchanged hash/no sidecars during a later read. These isolate the design assumptions; they do not qualify the eventual Go/helper binaries, read-only mounts, production latency, or target hardware. No application code, host configuration, or external service was changed.

## 23. Fifth critical design review — 2026-10-31

This pass reviewed lifecycle ordering and policy interactions. Three contract gaps were corrected without adding a daemon, provider, or archive format. These are document-level findings, not reproduced defects in implemented backup-worker code.

| Finding | Consequence | Incorporated correction and required proof |
| --- | --- | --- |
| Setup acquired its coordinator and created its journal after early enrollment/key/helper writes. | High: concurrent initial setups or early interruption could leave conflicting identities and untracked source changes despite later activation locking. | Acquire one stable installation coordinator and journal before the first domain mutation; preserve no-replace identities, reconcile lost acknowledgments, and distinguish preparing from migrating. Race two clients and interrupt every early boundary. |
| Automatic opt-in discovery lacked an explicit exception for retirement. | Medium: an enabled marker, reappearing service, or re-added source could reactivate intentionally retired coverage. | Persist source/service-scoped retirement exclusions in state and archive control; require explicit reactivation, preserve reduced-scope history, and test discovery/recovery against those exclusions. |
| Delivery recovery required all due reports to be sent while downtime policy coalesced them into one report. | Medium: recovery could remain failed indefinitely or send the forbidden backlog to satisfy the monitor. | An accepted current catch-up explicitly covers missed periods without marking historical reports delivered; test acceptance failures, crash recovery, and the same-day report boundary. |

The changes preserve the earlier setup deadlock fix: the coordinator spans the lifecycle, while exclusive schema/archive locks cover only the drained mutation phase and are released before ordinary acceptance jobs. Early preparation must not block existing healthy jobs. Review validation checks the document's ordered steps and policy wording; real concurrent setup, retirement/recovery, and notification state-machine tests remain implementation gates.

## 24. Sixth critical design review — 2026-10-31

This pass examined cross-process recovery metadata and notification acknowledgment boundaries. Two remaining gaps were corrected; the notification correction deliberately reduces v1 complexity.

| Finding | Consequence | Incorporated correction and required proof |
| --- | --- | --- |
| Atomic checkpoint replacement lacked serialized monotonic control commits. | High: a delayed producer could replace newer retirement/pin evidence with an older valid checkpoint; loss of local state would then hide the acknowledged change. | Stable control-commit lock, authoritative revisions/epoch checks, durable completion acknowledgment, and explicit bundle/control lock order. Race stale writers, pin/prune, and state loss. |
| Reports could be split without defining whole-report acceptance. | Medium: one accepted fragment could be mistaken for delivery of the full report, while partial fallback/retry added ambiguity. | One complete bounded message per logical report/batch, essential severity/counts/age/action preserved, optional details summarized, and explicit batch membership. Test provider limits, Unicode, omissions, and fallback. |

A temporary filesystem experiment published and fsynced revision 2 with a retirement record, then atomically published revision 1 without it: the older state replaced the newer despite both writes being individually durable. A revision guard rejected the stale candidate in a separate check; this is evidence for the missing ordering requirement, not a concurrent implementation test. No production helper or messenger was exercised. The control-commit lock is part of existing archive coordination, and reports reuse the existing notifier/providers; no additional service is introduced.

## 25. Seventh critical design review — 2026-10-31

This review checked the end-to-end evidence behind application-restore claims. One additional contract gap was found; the correction tightens the existing drill path and adds no command, service, or provider.

| Finding | Consequence | Incorporated correction and required proof |
| --- | --- | --- |
| Restore verification checked the archive before copying without explicitly binding the verdict to the actual disposable input. | High: a stale/wrong same-schema copy could earn a verification pin for a different capture; a hard-linked working file could also let a writable check mutate the archive. | Hash the completed independent destination before checker launch, bind job/result/input digest to the selected backup, reject aliases/reused workspaces, and test wrong-capture substitution plus writable checks. |

Temporary SQLite experiments confirmed that a different snapshot with the same schema passed `quick_check` and a basic application read while its digest differed from the selected snapshot; hashing the actual input detected the substitution. A separate hard-link experiment showed a write through the supposed disposable file changing the original bytes. These demonstrate why source-only checksum validation and a logically valid schema are insufficient evidence for the drill's input identity. They do not demonstrate bugs in an implemented worker. Document checks and the isolated experiments passed; integration of the copy gate, host job binding, and pin publication remains an implementation requirement.
