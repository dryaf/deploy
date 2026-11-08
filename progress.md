# Progress checkpoint

Ordinary YAML steps 1–54 and shared-configuration SC1–SC5 are implemented.
SC5 verification completed on **2026-11-09, 00:32 Europe/Vienna**.
No implementation or test process is left running by this task.

The [roadmap](docs/roadmap.md) lists the remaining scope.

## Latest work: shared configuration SC5 (step 54)

Both short worker commands now coordinate `notification_worker.enabled`,
`backup_worker.enabled` and `backup_worker.notifications.enabled` for the retained
local shared installation. Any disable fences and stops capture, health,
maintenance and report production before gateway delivery is paused. The gateway
service is removed only for its own disable. Queues, archive, source keys,
producer generations, original owner/cutover, receiver/command history, credential
exclusions and native Signal lifetime remain retained. LED cleanup affects only
owned output and keeps original permission, bindings, incidents and foreign clients.

A typed restoration anchor forces a new observation cycle, including a previously
qualified verification-only gateway. Existing delivery resumes after fresh
notification qualification; enabled command intake or an unadmitted enable request
must be disabled/applied separately before activity. Restoring reporting cannot
re-enable commands. Backup qualification binds the activity receipt, and capture
activation needs a new manual backup begun after restoration. Explicit uninstall,
missing activity history, changed full declarations and recovery do not supply
restoration authority. Recovered variants remain SC6/SC7.

Durable coordinator, setup-file and SQL phases retain exact stop/start/commit
retries. The full declaration stays pinned even when a disabled dependency drops
out of the notification fingerprint. Both workers inspect the canonical completed
receipt; no source enrollment, replacement handoff or legacy fallback occurs.

SC5 validation/evidence is under
`dist/workspaces/shared-configuration-sc5-evidence/`:

- Four compiled isolated CLI scenarios passed (271.021s): staged/delivering,
  owned Signal-only, Telegram read and mutation command histories. They exercise
  all three activity controls and both workers' apply/resume/status/plan, fresh
  terminal qualification, unchanged receiver/cutover and explicit intake restore.
- Final staged terminal qualification scenario passed (98.876s); this includes
  restoring a gateway before its first delivery admission.
- Complete final coordinator suite excluding `^TestWorkerCLI` passed (49.449s),
  including stop prerequisites without lingering/source tools/image, unbound
  delegation, complete-declaration retry pins and missing history refusal.
- Complete backup suite passed (206.657s). Affected installer/delivery/command/
  report-profile regression tests passed (19.791s); focused restoration-anchor,
  queued command, LED and qualification checks passed separately.
- Complete config/provider/relay suites passed (0.024s / 0.008s / 10.522s).
  Vet, generated field reference and whitespace checks passed.
- The initial sandbox-only broad run was stopped because its socket fixtures
  were blocked. It is retained as diagnostic evidence, not a passing regression.
  Replacement checks used the required local test capabilities and an isolated
  writable cache. SC5 did not rerun the entire installer suite; affected installer
  workflows and compiled service scenarios are the SC5 validation boundary.

Managed `ActivityRestoration` anchors and delivery-admission version 4 bind the
original pause through suspension/restoration. Backup activity version 2 retains
its file/SQL head, dependency inventory and restoration time; future recovery and
composition work must preserve these histories. No staging, commits, race run or
live/manual qualification were performed.

## Previous work: shared configuration SC4 (step 53)

Unrecovered Telegram-only command gateways support same-bot private token/chat
reference rotation and fixed alert destination changes after a separate
command-disable/apply. Same-destination rotations retain queued delivery;
destination changes require settled delivery messages. Receipts 35/40 and both
executable capability checks preserve the original receiver, reduced permissions,
cursor, queued commands, saved results/replies, reporting handoff and LED/foreign
client state. Fresh qualification resumes delivery; intake needs explicit re-enable.
Admitted profile removal, account/name changes, new bots and command audiences
retain their separate identity/admission/disposition requirements. The existing
command-free unused Telegram profile workflow beside owned Signal remains bounded.

Owned direct Signal gateways with the existing local backup support same-account
private references and recipient changes before activation and on unrecovered
command-free production owners. Production changes pause for fresh qualification;
changed destinations require settled delivery messages. The original account
bytes, link, native account/inbox, service/socket/unit, helper and native CLI remain.
Immutable recipient approvals select each send's exact destination. Gateway,
helper and native executable capability checks precede production effects.

Both workers retain original bootstrap and reporting proofs after repeated
updates. Private exclusions include original, staged and production reference
sets; staged native provider changes also retain a manifest-anchored credential
history. Missing current/historical approvals, changed files or damaged history
fail closed. Explicit service repair uses the original setup and retained selected
references; it cannot relink or replace an account.

SC5 shared activity is integrated above. SC6/SC7 recovered variants, SC8 composition, new
command authority and Signal account/service lifetime work remain separate.
Race and live/manual qualification remain deferred. No staging or commits were made.

SC4 verification:

- Final focused provider/history checks passed (installer 23.525s):
  `/tmp/deploy-sc4-final-history-invariant.log`. They include missing current and
  historical native approvals and removed-anchor refusal before repair/status.
- Complete final config/provider/relay checks passed (0.034s / 0.010s / 9.453s):
  `/tmp/deploy-sc4-final-config-provider-relay.log`. Expanded Signal backup scope
  interruption checks passed (6.724s): `/tmp/deploy-sc4-signal-backup.log`.
- Complete coordinator suite passed (60.737s):
  `/tmp/deploy-sc4-coordinator-source-final.log`.
- Final compiled Telegram read/mutation command scenarios passed (196.926s):
  `/tmp/deploy-sc4-telegram-final-source.log`.
- Compiled native Signal-only/combined layouts passed (181.295s):
  `/tmp/deploy-sc4-native-shapes-final.log`. The final Signal-only scenario also
  verifies staged and repeated production updates, exact exclusions, stable native
  PID/start count, and unused Telegram addition/removal (300.828s):
  `/tmp/deploy-sc4-native-presence-final.log`.
- Complete installer regression suite passed (1487.687s):
  `/tmp/deploy-sc4-final-installer.log`. It began before the final missing-anchor
  invariant; the final focused installer checks and compiled CLI scenarios cover
  that invariant.
- Broad regular checks excluding `^TestWorkerCLI` passed all runtime packages,
  including worker 370.533s, backup 209.601s and installer 1524.754s. Its coordinator
  field-reference check ran before regeneration and reported stale documentation;
  the generated reference and complete final coordinator check above replace that
  result. Log: `/tmp/deploy-sc4-regular.log`.
- Final vet, generated field reference, 274 local documentation links and whitespace
  checks passed. Logs: `/tmp/deploy-sc4-vet-source-final.log` and
  `/tmp/deploy-sc4-docs-final.log`.
- Logs, documentation snapshots and a final changed-working-tree SHA-256 inventory
  are retained in `dist/workspaces/shared-configuration-sc4-evidence/`. The
  inventory includes pre-existing SC1–SC3 work; earlier evidence is unchanged.

This is scoped software validation, not the complete milestone `make test`/race
or live provider/hardware acceptance gate.

## Previous work: shared configuration SC3 (step 52)

SC3 adds disabled-command route edits, production backup report-route selection
for command-free and disabled-command gateways, and disabled-command derived
freshness. Both worker entry points converge. Route edits preserve the exact
interaction runtime and require settled delivery messages. Report selection
requires an applied route and settled frozen backup exports; explicit producers
must already allow it. Effective default permission changes pause for notification
qualification;
an explicitly allowed route retains runtime and qualification.

Backup scope 24 and a compact SQL reference retain the original handoff, owner
and pipeline binding while selecting the route for future exports. Frozen messages
and command replies keep their original audiences. Status and continued reporting
check the typed route history; missing SQL/file authority fails closed. Scope 25
publishes command-bearing derived freshness, with capture/report fences, both
backup runtime files and the archive checkpoint. Notification qualification and
backup capture qualification/activation remain separate. New route receipts 33/38
and executable capability checks retain receiver restrictions, saved results,
observations and LED/foreign-client state. Commands require explicit re-enable.

SC3 verification:

- Expanded config, worker, installer, backup and coordinator tests passed:
  `/tmp/deploy-sc3-focused4.log`.
- Complete backup/coordinator suites passed (204.537s / 63.771s):
  `/tmp/deploy-sc3-final-backup-coordinator.log`. Final history, missing SQL/file
  authority and alternating-policy checks also passed. The complete final backup
  rerun after the initialization-marker guard passed (173.508s):
  `/tmp/deploy-sc3-backup-final.log`.
- Command-free report selection passed (140.620s) in `/tmp/deploy-sc3-cli3.log`.
  The other scenarios in that diagnostic run had a test-ordering error; the
  corrected read/mutation routing scenarios passed (248.165s) in
  `/tmp/deploy-sc3-command-cli-final.log`.
- Existing producer/read and timing/mutation compiled CLI regressions passed
  (234.410s): `/tmp/deploy-sc3-cli-regression.log`.
- Final vet, documentation links, generated field inventory and whitespace
  checks passed.
- The broad regular run excluding `^TestWorkerCLI` finished: installer 1311.207s,
  worker 387.063s and coordinator 64.264s passed. Its earlier backup nil-declaration
  failure was fixed; the complete final backup and coordinator checks above
  replace that result. Log: `/tmp/deploy-sc3-regular.log`.
- Logs, this checkpoint and a final changed-working-tree SHA-256 inventory are
  retained in `dist/workspaces/shared-configuration-sc3-evidence/`. The inventory
  includes pre-existing step-50/SC1/SC2 work; historical evidence is unchanged.

CLI orchestration passed before the final missing-table guard; final backup tests
cover that guard. This is scoped validation, not the complete milestone
`make test`/race gate. Recovered and Signal command variants, provider replacement and
shared activity remain outside SC3. Race and live/manual qualification remain
deferred. No staging or commits were made.

## Completed: shared configuration SC2 (step 51)

Existing unrecovered Telegram-only gateways with admitted local backup commands
now support quota, requiredness, allowed routes, policy health, context capabilities
and explicit/default producer declarations while intake is disabled. Effective
updates pause delivery, advance both scopes and require notification qualification;
commands are re-enabled only by a later explicit YAML apply. Equivalent producer
declarations retain runtime bytes, activation, observations and qualification.

The implementation keeps the original producer and receiver, SQL command head,
reduced actor/subject permissions, queued commands, saved results/replies, original
reporting handoff, original observations and LED/foreign-client state. Quota
reductions must fit retained work; access changes require settled delivery messages.
Context policy cannot expand receiver authority. New typed receipts 34/39 and
executable capability checks protect effective updates. Retained command-aware
scope inspection supports declaration history, repeated updates, independent
renewal and later command activity. Exact retries pin both notification and full
backup declarations; completed declarations cannot replay over later edits.

SC2 verification passed:

- Focused config, worker, installer, coordinator and backup checks passed:
  `/tmp/deploy-sc2-final-focused.log`.
- Final dispatch checks passed (3.465s); the complete coordinator suite excluding
  `^TestWorkerCLI` passed (71.619s): `/tmp/deploy-sc2-dispatch-final.log` and
  `/tmp/deploy-sc2-workerctl-final.log`.
- Both compiled isolated SSH producer scenarios passed (274.401s):
  `/tmp/deploy-sc2-cli-pass.log`.
- Final `go vet ./...`, documentation links, generated field inventory and
  whitespace checks passed.
- All four existing read/mutation timing and budget compiled CLI regressions
  passed (374.330s): `/tmp/deploy-sc2-cli-regression.log`.
- Regular `go test -timeout=35m ./... -skip '^TestWorkerCLI'` and `go vet ./...`
  passed: `/tmp/deploy-sc2-regular.log` (installer 1166.924s, worker 426.271s,
  backup 209.852s, coordinator 54.648s). This run began before the final coordinator
  dispatch fix; the complete final coordinator suite and six CLI scenarios above
  verify that source separately.
- Logs, checkpoint and final changed-working-tree SHA-256 inventory are retained
  in `dist/workspaces/shared-configuration-sc2-evidence/`. The inventory includes
  existing step-50/SC1 work; their historical evidence remains unchanged.

Other compiled CLI scenarios were excluded from the regular run; this is not the
full milestone `make test`/race gate. Race and live/manual qualification remain
deferred. No staging or commits were made.

Recovered/Signal command migrations, new identities, provider changes, route
definitions, report-route selection and
derived backup freshness remain outside SC2. See the
[shared configuration plan](docs/plans/shared-configuration.md).

## Completed: shared configuration SC1

The [shared configuration matrix and plan](docs/plans/shared-configuration.md)
defines V0/VU/A0/AU/AD/AE/Q/R0/RD/RE states, current support and missing workflows,
queue/ownership/qualification prerequisites, preservation contracts and SC2–SC8
implementation dependencies. SC1 adds read-only planner guidance; it does not
implement the pending migrations or add runtime admission authority.

Notification `plan` now distinguishes unsupported producer, routing/freshness,
provider/destination, shared activity and recovered changes. Implemented limits
and explicit timing updates with active commands explain the separate disable/apply
prerequisite. Missing migrations explain that disabling alone is insufficient.
Backup planning directs gateway-relevant edits to notification planning because
backup inspection does not establish command/recovery authority. Existing pending,
drift, scope, helper and path checks retain precedence; no-op and unbound planning
keep their previous behavior. Public JSON fields are unchanged.

SC1 verification passed:

- 130 state/change matrix cases plus conditional, read-only, redaction, no-op,
  pending/drift/helper and backup-plan checks: `/tmp/deploy-sc1-planning.log`
  (focused package run: 0.066s).
- Coordinator regular tests excluding `^TestWorkerCLI`: 42.850s,
  `/tmp/deploy-sc1-workerctl.log`; `go vet ./internal/workerctl` passed.
- Compiled isolated read-mode SSH timing workflow: 146.118s,
  `/tmp/deploy-sc1-cli.log`, including both workers' apply/resume/status/plan.
- Documentation links, generated field inventory and whitespace checks passed.
- Logs, checkpoint and SC1 file SHA-256 inventory are retained in
  `dist/workspaces/shared-configuration-sc1-evidence/`.

These are SC1 checks, not a new full `make test`/race gate. The original step-50
source/evidence inventory remains unchanged. Race and live/manual qualification
remain deferred.

## Completed: step 50 (historical verification)

An unrecovered Telegram gateway with admitted local backup commands can change
heartbeat/evidence thresholds in an existing explicit
`notification_worker.producers.backup-worker` declaration:

1. Disable command intake in YAML and apply.
2. Change only `max_silence_seconds` / `max_evidence_seconds`, plan and apply.
3. Qualify the revised notification runtime and apply to resume delivery.
4. Explicitly enable commands and apply when wanted.

The implementation retains the original receiver and permission restrictions,
reporting/delivery ownership, original observations, queued messages and commands,
saved results/replies and LED disposition. Separate typed history pins the exact
command authority. Both installed helpers must support timing migration before
review permits a delivery pause. New producer declarations, other producer policy,
provider/audience changes, recovered gateways and Signal command intake remain
outside this step. Budget changes and timing changes remain separate operations.

## Step-50 verification

Focused declaration, storage, installer and coordinator tests pass, including
read/mutation modes, repeated changes, interruption/resume, changed retry inputs,
lost receiver authority, saved replies, original observations, alternating budget
changes, qualification renewals and LED/foreign-client retention.

- Focused log: `/tmp/deploy-yaml-step50-focused.log`.
- Compiled isolated SSH timing and budget scenarios passed in read and mutation
  modes (341.860s): `/tmp/deploy-yaml-step50-cli-final.log`.
- Documentation links and the generated public-field reference passed.
- `GOCACHE=dist/test-go-cache go test -timeout=35m ./... -skip '^TestWorkerCLI'`
  and `go vet ./...` passed: `/tmp/deploy-yaml-step50-regular.log`.
  The four relevant compiled CLI scenarios ran separately. Other compiled CLI
  scenarios were excluded from this run; this is not the full `make test` gate.
- Final installer/coordinator checks passed after cleanup (7.555s / 4.136s).
- Whitespace checks passed. Logs, this checkpoint and a SHA-256 source inventory
  are retained in `dist/workspaces/yaml-step50-evidence/`.
- Race tests and live/manual qualification remain deferred.

## Next work

SC1–SC5 are complete. Continue with **SC6: typed policy continuation after recovery**,
using the row prerequisites and acceptance cases in the shared configuration plan.
SC6–SC8 remain implementation work.
Step 49 is already integrated: do not reapply its integration script or copy its old workspace over
the root. Preserve existing work and `dist/checkpoints/`; no reset, cleanup,
staging or commit is authorized by this checkpoint. No subagents were used.

Remaining work includes recovered policy continuation and composition; provider identity/account lifetime changes; retired source identity reuse; command enrollment on
activated gateways and new authority/audiences; recovered/legacy qualification;
Signal intake and adoption/relinking; production LED mapping/device replacement;
helper and worker lifecycle changes; public-field acceptance and release/onboarding
work. Application local image builds/signing/SSH transfer, explicit private
networking, and uptime/telemetry are planned separately in the roadmap.

The user will perform live provider, Raspberry Pi/ARM64/LED and other host/hardware
qualification manually after implementation. Do not request live credentials or
count fixture evidence as live qualification. No current usability completion
percentage is asserted; the historical 84/84 software score is a different milestone.
