# Shared configuration: migration matrix and implementation plan

Updated 2026-11-09 against ordinary YAML steps 1–54. **SC1–SC5 are implemented:**
the remaining transitions are classified; disabled-command producer policy,
routing, report-route selection, derived freshness, bounded provider changes
and shared activity have migrations. SC6–SC8 remain implementation work. A planned
transition below is not available
merely because its YAML fields can be parsed.

This is the detailed shared-configuration workstream of the
[worker usability plan](worker-usability.md), primarily U6/U9/U10/U11.
The [roadmap](../roadmap.md) tracks the other workstreams; the
[checkpoint](../../progress.md) records completed implementation and checks.
Current supported user workflows remain in the [worker guide](../workers/README.md).

## Scope and classification inputs

The topology is an existing local notification gateway and backup producer on the
same server and UID, with retained coordinator and domain ownership. A source
server may be remote. Cross-server gateway transport, new producer identities,
command-authority expansion, Signal account takeover/relinking, LED device
replacement, helper upgrades and directory relocation have separate admission or
lifecycle workstreams. This plan records dependencies on them.

Classify from the retained declaration, canonical installation receipt, delivery
owner, command history/admission, recovery commit and provider ownership. Desired
YAML alone does not prove whether commands were admitted or recovery completed.
A disabled command declaration can still have an admitted receiver and results.
A verification-mode installation can still have a production delivery owner.

| State | Required facts | Meaning for updates |
| --- | --- | --- |
| V0 | Enrolled verification gateway; no delivery owner, recovery commit or admitted command history | Shared policy is staged; qualification/admission still follow |
| VU | Same as V0, with an enabled or disabled unadmitted Telegram command declaration | Retain its bot, aliases and policy; the declaration grants no receiver authority |
| A0 | Unrecovered delivery owner; no retained command history or command declaration | Existing command-free production migrations can apply |
| AU | Delivering gateway with a command declaration but no retained receiver admission | Finish original command admission separately; staged editing does not authorize a production migration |
| AD | Unrecovered delivery owner and retained command authority; intake disabled in applied and desired policy | Only implemented command-preserving migrations are eligible |
| AE | Same retained command authority, with intake active or enabled in applied/desired policy | A supported AD migration first needs a separate disable/apply; missing AD migrations remain missing |
| Q | Verification mode with an existing delivery owner after a production pause | Complete the retained pause/update/qualification workflow before a different configuration change |
| R0 | Completed recovery evidence; no retained command authority | Needs the SC6 recovery continuation and applicable SC7 migration |
| RD | Completed recovery with retained command authority and disabled intake | Also retain command recovery/receiver proofs; SC6/SC7 required |
| RE | Completed recovery with retained command authority and active/requested intake | SC6/SC7 and applicable command admission checks required; fresh-install toggles are not a bypass |

Incomplete recovery, missing/corrupt authority, ownership drift, pending operations,
removed installations, legacy scope conversion and changed helper/path pins retain
their existing stronger checks. They are not extra update states that permit a
configuration migration.

## Baseline matrix

Each cell is conditional on the prerequisites in the next section:

- **V**: existing staged workflow; qualify the revised runtime before activation.
- **D**: existing production pause/apply → qualify → apply workflow.
- **D0**: record equivalent effective policy without a runtime restart or renewal;
  runtime comparison must prove equivalence.
- **H**: complete original command admission separately; production policy editing
  with an unadmitted declaration is not implemented.
- **K**: disable commands and apply separately, then use the supported AD workflow.
- **A**: shared activity stop/fence → restore verification → qualify → apply;
  capture needs separate qualification, a new manual run and explicit activation.
- **P8**: missing composition implementation owned by SC8.
  Disabling commands alone does not implement it.
- **R**: SC6 plus the corresponding SC7 recovered variant.
- **X**: separate enrollment/handoff/admission workflow; keep existing ownership.

Q is omitted from the grid because its prerequisite always wins: finish the exact
retained workflow first. R0/RD/RE all show R; their distinct retained proofs and
command-intake conditions are mandatory in SC6/SC7.

| ID / requested change | V0 | VU | A0 | AU | AD | AE | R0 / RD / RE |
| --- | --- | --- | --- | --- | --- | --- | --- |
| SC-LIM: global queue, payload, database, free-space and receipt-retention budgets | V | V | D | H | D | K | R |
| SC-TIM: timing in an existing explicit backup producer declaration | V | V | D | H | D | K | R |
| SC-QUOTA: existing producer message quota | V | V | D | H | D | K | R |
| SC-ACCESS: producer requiredness, allowed routes, policy health or context capabilities | V | V | D | H | D | K | R |
| SC-DECL: introduce/remove explicit policy for the same existing backup producer | V | V | D0 if equivalent, otherwise D | H | D0 if equivalent, otherwise D | K | R |
| SC-ROUTES: route definitions and primary/fallback among retained providers | V | V | D | H | D | K | R |
| SC-REPORT: backup selection of an already declared notification route | V | V | D0 if explicitly allowed, otherwise D | H | D0 if explicitly allowed, otherwise D | K | R |
| SC-FRESH: backup freshness supplying the default producer evidence deadline | V | V | D | H | D | K | R |
| SC-TG-REF: Telegram token/chat-file selection for retained provider metadata | V | V, same command bot | D | H | D, same bot | K, same bot | R |
| SC-TG-PRESENCE: unused Telegram profile addition/removal beside unchanged owned Signal | V | X | D | X + command admission | X | X | R |
| SC-TG-ID: explicit Telegram account/name or interactive destination declaration changes | X | X + command admission | X | X + command admission | X + command admission | X + command admission | R + applicable admission |
| SC-SIGNAL-REF: owned direct Signal private references / recipient | V | X + command admission | D | X + command admission | X + command admission | X + command admission | R |
| SC-SIGNAL-LIFE: account, service or native-artifact replacement | X | X + command admission | X | X + command admission | X + command admission | X + command admission | R + applicable lifecycle |
| SC-ACT: notification worker, shared backup or shared reporting disable/re-enable | A | A; K if enable requested | A | A; K if enable requested | A | K | R |
| SC-HANDOFF: change the selected shared notification worker | X | X | X | X | X | X | R + handoff |
| SC-NEW: additional producer identities or retirement | X | X | X | X | X | X | R + enrollment/disposition |
| SC-MIX: edits spanning migration families or command/LED/artifact policy | P8; apply supported changes separately today | P8 | P8 | P8 | P8 | P8 | R + composition |

SC-TG-REF cannot determine bot/destination identity from a changed pathname.
Existing domain review distinguishes same-destination rotation, which can retain
queued work in A0, from a changed bot/chat, which requires a settled queue.
VU retains its command bot, actors and conversations; qualification covers the
selected alert destination. SC4 makes that distinction for admitted commands while retaining their original
receiver and reply audiences.

Backup freshness with an explicit producer is backup-only policy: its deadline
does not supply the gateway producer threshold. Inspect/apply it through the
supported backup policy workflow. Introducing explicit producer settings also
removes the derived freshness field from notification scope; that scope consequence
is one SC-DECL change, not an operator-requested mixed edit.

Identical normalized declarations remain a no-op, including on recovered or
paused installations. No-op planning says the policy matches; it does not assert
current health, completed qualification or active delivery. D0 needs the existing
runtime equivalence review, not just equality of selected YAML values.

## Preconditions, retained state and qualification

All rows retain the current gateway/producer identities and generations, archive
writer, source enrollment/keys, original reporting owner, legacy-sender fence,
credential history and terminal message history. Retained commands additionally
preserve the original receiver/capability boundary, permission restrictions,
provider input fences, pending requests, saved results and original reply targets.
Unchanged LED disposition retains bindings, unresolved incidents and other clients.
Owned native Signal retains its account, service, inbox and artifact authority.

| Rows | Checks before retaining an operation | Publication and activation contract |
| --- | --- | --- |
| SC-LIM / SC-QUOTA | Count retained messages/payload and inspect database/WAL/free-space requirements; refuse reductions that do not fit; keep command queue budgets separate | Retain pending work, update derived producer limits where applicable, record both declarations; production effective changes need fresh notification qualification |
| SC-TIM | Retain existing producer identity and original heartbeat/evidence observations; command path currently requires an existing explicit declaration | Change evaluation thresholds without fabricating observations; record both scopes and qualify changed effective policy |
| SC-ACCESS | Keep the active backup report route allowed; settle affected delivery work before access changes; context capability changes cannot expand receiver rights | Publish same-identity producer policy and preserve terminal history; new command authority follows its separate admission |
| SC-DECL | Compare compiled effective policy, including derived defaults; distinguish existing identity from new enrollment | Equivalent A0/AD changes record declarations only; effective changes use their applicable policy migration |
| SC-ROUTES / SC-REPORT | Declare/apply routes first, then explicit producer permission if needed; settle affected messages; retain or settle outstanding command replies before removing their dependencies | Retain frozen destinations and subscriptions; both declarations advance; notification qualification follows an effective runtime change |
| SC-FRESH | Confirm default producer policy; fence capture/report publication and other backup readers under existing lock order | Coordinate both backup runtimes, archive checkpoint and gateway evidence deadline; notification requalification and backup capture qualification/activation remain separate |
| SC-TG-REF / SC-TG-PRESENCE | Verify old/new private references, actual provider identity and retained credential exclusions; classify queue requirements from destination identity; reject removal of referenced profiles | Preserve cursor, conversations and saved replies; requalify actual changed provider/runtime; separately admit any new command authority |
| SC-TG-ID / SC-SIGNAL | Identify account/destination/service lifetime changes; require supported account handoff, replacement or artifact migration and command admission where applicable | Preserve old outcomes and original audiences; a key/account supplied by a new declaration is not proof of ownership |
| SC-ACT | Compute dependent capture/report/intake ordering; inspect pending work and retained fences; do not infer retirement from disable | Stop dependent production before its gateway; retain queues/accounts/archive; restore verification and only explicitly requested qualified capabilities |
| SC-HANDOFF / SC-NEW | Inspect exact old/new owners and obtain explicit enrollment/retirement/disposition receipts | No silent producer replacement, legacy-sender fallback or dropped fault history |
| SC-MIX | Validate the entire edit and dependency graph before effects; detect conflicts with pending operations and separate authority changes | One pinned intent references each child receipt; lost replies resume the same graph; restoring old files does not undo external outcomes |

Every effective production update must state which service/report triggers are
fenced, which qualification becomes invalid and how to resume. Both installed
executables must prove the required capability before any pause. Unsupported
helpers, capacity failure, missing receiver/recovery proofs or changed retry inputs
must fail before new effects. A separate apply cannot replace the original
reporting handoff merely to make the current declaration match.

## Read-only planner contract implemented by SC1

The notification planner checks pending operations and canonical receipts before
applying the matrix. Existing scope/helper/path checks retain precedence. New
classification supplies fixed action text and codes through the existing
`state`/`code`/`next` result; it adds no JSON fields or migration authority.
It never emits credential values, private paths, actor values or secret digests.

| Situation | Planner outcome / required action |
| --- | --- |
| No policy change | Preserve `up_to_date`; inspect status separately for activity/qualification |
| Pending operation / ownership drift / missing snapshot | Existing exact-resume or recovery failure wins; do not classify a replacement operation |
| Recovered R0/RD/RE | `recovered_configuration_migration_required`; retain active recovered identities and proofs until the recovery-aware workflow exists |
| Legacy Signal authority | `signal_adoption_required`; retain account/inbox and use its adoption/qualification workflow |
| Q with a different policy | `delivery_qualification_pending`; finish the retained configuration before another change |
| Shared activity | `policy_changed` after dependency/command review; either worker coordinates capture/report stop and gateway pause; restoration requires fresh notification qualification and separate capture activation |
| Different shared worker / additional producer | `shared_dependency_migration_required` / `producer_enrollment_required`; explicit handoff/admission required |
| Mixed families | `separate_configuration_changes_required`; apply supported families separately |
| Signal edits / explicit Telegram metadata edits | `signal_provider_migration_required` / `provider_identity_migration_required` |
| AU | `command_admission_required`; finish original admission separately |
| AE requesting an implemented AD migration | `disable_commands_required`; disable/apply first, then the bounded update |
| AD/AE requesting an unsupported declaration shape or provider workflow | `command_producer_policy_migration_required`, `command_routing_migration_required` or `command_provider_migration_required`; disabling alone is insufficient |
| A0/AD backup report-route selection | Apply a declared route through either worker; settle frozen backup exports, review producer permission and qualify effective runtime changes |
| Unrecognized completed history / unmet declaration shape | `shared_configuration_review_required` / `shared_configuration_prerequisites_required` |
| Backup plan with changed shared gateway scope | `shared_gateway_review_required`; run the notification worker's read-only plan to inspect gateway command/recovery authority |

Pure classification is in
[shared_configuration_plan.go](../../internal/workerctl/shared_configuration_plan.go);
[planning.go](../../internal/workerctl/planning.go) invokes it only during planning.
Existing `internal/config` declaration validators and typed domain review remain
the source of apply eligibility. Provider content, capacity, services and current
qualification still need their authoritative apply/review checks. A plan is not
permission to bypass those checks. Unbound gateways and standalone backups retain
their existing planning paths. Gateway diffs include only gateway-relevant
backup fields. A backup-only difference in an old full snapshot may already be
applied; its completed backup receipts must be inspected before effects rather
than treating that historical difference as a new mixed operation.

## Implementation packages

| Package | Bounded sequence and dependencies | Required completion evidence |
| --- | --- | --- |
| SC2 — complete | AD Telegram-only producer quota, access policy and explicit/derived declarations; retains step-49/50 history. Context settings retain original receiver authority; new authority still needs admission | Read/mutation storage, installer, coordinator and compiled CLI coverage; capacity/settled-queue refusal, equivalent declarations, repeated updates/retries and narrowed permissions |
| SC3 — complete | AD Telegram-only route edits, A0/AD production report selection, and AD derived freshness; route and explicit producer permissions must be applied before selection | Both worker entry points, retained original handoff and receiver, frozen-report refusal, future route selection, scope/publication retries and separate capture qualification |
| SC4 — complete | AD same-bot Telegram credential/reference rotation and alert destination changes; existing command-free unused Telegram presence workflow retained with explicit retirement refusal for command dependencies. Owned direct Signal same-account references/recipient change without native lifetime replacement. New bots/audiences, metadata, account takeover/relinking and native intake retain their separate admission/lifecycle dependencies | Typed receipts 35/40; old/new credential pins, cursor/receiver and native ownership retained, missing approval/history refusal, fresh qualification; read/mutation and native Signal compiled CLI coverage |
| SC5 — complete | Shared activity intent and backup SQL/unit fences for enrolled and unrecovered delivery/disabled-command gateways; capture and reporting stop before gateway pause/removal | All three switches, exact full-declaration retries, fresh qualification, retained queues/commands/receiver and immutable owner, native Signal process and LED permission/foreign-client preservation; capture qualification/activation remain separate |
| SC6 | Establish a typed policy continuation after completed recovery, binding active recovered identities, recovery commit, original publication/evidence, current delivery and applicable command/provider recovery proofs | Read-only review and corruption refusal, first post-recovery no-op/update, multiple policy changes and another recovery generation; old recovery receipts remain immutable |
| SC7 | Apply SC2–SC5 operations to R0 first, then RD/RE, using SC6; include unchanged owned Signal and LED history | Each supported recovered matrix cell has executable coverage; qualification/intake and recovered cursor/receiver ownership remain separate |
| SC8 | Compose only compatible supported families in dependency order; explicit ordered steps for incompatible edits. Audit histories after source edits, backup policy and compatible helper changes | Both workers' plan/status/apply/resume, semantic no-ops, exact graph retries, concurrent/change-input rejection, no partial effects from invalid mixed YAML |

SC5 stops both capture and reporting for every shared disable. Existing queues and
history remain retained. Restoration resumes reporting after fresh notification
qualification; capture needs separate qualification, a new manual run and activation.
SC6 must decide the adapter between immutable recovery-time configuration proofs
and later policy receipts; removing existing recovery guards is not an implementation.
SC4 defines command-provider removal as a retirement/admission dependency: no
removal workflow is enabled while the retained receiver or saved replies depend on
it. Empty ordinary delivery queues do not retire that authority.

## SC2 implementation boundary

AD producer policy is supported for the existing local backup identity on an
unrecovered Telegram-only gateway. Disable intake and apply first. Change only
producer quota, requiredness, allowed routes, health, context or timing; the active
backup report route must remain allowed. Effective changes pause delivery, record
both scopes and require notification qualification before apply resumes delivery.
Re-enable commands explicitly afterward. Quota reductions must fit retained work;
access changes require settled delivery messages. Queued commands, saved results
and replies remain retained, with the exact original receiver and reduced rights.
Context capabilities cannot expand actor, subject or receiver authority.

Equivalent explicit/default declarations record scope without a runtime restart,
delivery pause, queue drain or qualification renewal. Review proves identical
runtime bytes and the disabled command head. Effective migrations use typed
command-producer receipts 34/39 (39 retains LED output); both installed executables
must prove capability before a pause. Original observations, producer identity,
reporting handoff, LED incidents and foreign clients remain. Exact retries pin
both notification and full backup declarations. Completed declaration history
permits later command activity and independent qualification renewal.

This does not implement recovered or Signal command migrations, new producers,
provider/destination changes, route definitions, backup report-route selection or
derived backup freshness. Those retain the packages below.

## SC3 implementation boundary

Unrecovered Telegram-only command gateways support route-only changes after a
separate disable/apply. Delivery messages must settle; command requests, saved
results and replies remain. The backup report route and producer permissions
must remain valid. Receipts 33/38 and an executable routing capability pin the
original runtime interaction, receiver, provider inputs and LED state. Effective
changes pause delivery and need notification qualification before apply resumes it.

A0 and AD installations can select an already declared route through either
worker. Explicit producer permission must already allow it. Frozen backup exports
must settle before selection; default producer permission changes also require
settled gateway messages. An already allowed explicit route retains the exact
runtime and qualification. Backup scope version 24 fences reporting, records the
selected route, and retains the original immutable handoff, owner and pipeline
binding. A compact SQL reference pins the immutable scope receipt. New exports
use the selected route; frozen messages keep their original audiences. Missing
SQL or file authority fails closed. Exact route history is checked by planning,
status, continued reporting and subsequent shared updates.

AD default-producer freshness uses backup scope version 25 and the existing
command timing receipts 32/37. Capture/report fences, both runtime publications
and the archive checkpoint resume under the existing lock order. Original
observations are retained. Notification qualification resumes delivery only;
backup capture remains staged for its own qualification and explicit activation.
Commands must be re-enabled explicitly. Explicit producer timing stays independent
of backup freshness. Recovered and Signal command variants, provider replacement
and shared activity remain outside SC3.

## SC5 implementation boundary

Either short worker command coordinates changes to the three activity controls.
Only activity fields may differ. The complete declaration is pinned alongside the
notification scope, so removing a disabled dependency from a derived scope cannot
hide a backup edit. Extra producers, recovery, pending policy transitions and
enabled command intake or an unadmitted enable request require a separate
command-disable/apply before activity.

The backup setup journal and SQL fence retain each exact operation and stop all
owned activation units, capture/health/maintenance services and report units before
the gateway changes. Reporting keeps its shared owner and immutable cutover; no
legacy fallback is selected. Gateway suspension uses a typed service receipt,
retaining runtime, identity, producer generations, command/credential history and
native Signal ownership. Explicit uninstall cannot supply that receipt.

Restore starts verification. An existing delivery owner needs fresh notification
observations before its queued delivery and report triggers resume. The original
LED permission may be restored after that qualification; previously disabled
output and command intake cannot be inferred. Capture remains staged. Backup
qualification is bound to the activity operation, and activation needs a manual
backup begun after restoration. Lost stop/start/commit replies resume their pinned
phase without re-enrollment or a replacement reporting handoff.

## SC4 implementation boundary

Unrecovered Telegram-only gateways with retained commands support private
reference rotation for the same bot and fixed alert destination changes after a
separate disable/apply. Same-destination changes retain queued delivery messages;
changed destinations require settled delivery messages. Queued commands, saved
results, original replies, cursor, receiver, permission reductions, producer,
reporting handoff and LED/foreign-client ownership remain. Receipts 35/40 retain
the disabled command head; both executables must prove command-provider capability
before a pause. Qualification renews delivery and does not re-enable intake.
Account/name metadata, new bots or command audiences and profile retirement still
require their own identity/admission/disposition workflow. The existing unused
Telegram presence workflow beside owned Signal remains command-free.

Owned direct Signal gateways support same-account target-private reference and
recipient changes before shared activation and on unrecovered command-free
production owners. Account input bytes, link, native account/inbox, socket,
service/unit, helper and native CLI remain pinned. Production destination changes
settle delivery messages; same-destination rotations retain them. Effective
production changes pause for fresh qualification and retain original reporting
ownership and every private-file exclusion.

Each selected Signal reference set has an immutable approval under the original
native service. The relay resolves that approval for a send's frozen destination;
its original service/health/account/inbox proof is unchanged. The gateway and its
helper plus the native service executable must prove the new capability before
production pauses. Retained source files and approval records are checked during
retries, subsequent applies, qualification and explicit service repair. Missing
completed approvals fail closed. Service repair uses the original setup proofs;
recipient selection cannot link, replace or take over an account. Native Signal
commands, recovered updates, service/artifact changes and legacy adoption remain
separate workstreams.

## Acceptance and evidence

SC1 has a table-driven executable matrix covering the ten states V0/VU/A0/AU/
AD/AE/Q/R0/RD/RE for representative budget, timing, quota, explicit declaration,
route, Telegram reference/metadata, Signal destination, derived freshness, report
route, activity, dependency and mixed changes. Additional cases cover no-op and
unbound delegation, read-only files, private-value redaction, pending-operation,
canonical drift and helper precedence, and backup planning without gateway authority.
See [shared_configuration_plan_test.go](../../internal/workerctl/shared_configuration_plan_test.go).

SC6–SC8 require domain/coordinator checks as well as classification coverage.
Exercise read/mutation intake, relevant Telegram/owned-Signal profiles, LED output
states and foreign clients, queued messages/commands/results/replies, narrowed
permissions and completed prior policy/source/helper changes. Interrupt each
retained intent, fence, domain commit, publication, scope acknowledgement and
qualification/admission boundary. Re-read canonical outcomes after lost replies.

Run meaningful focused tests and compiled short-CLI scenarios for each newly
supported cell. Complete regular tests/vet on final source before race tests and
applicable isolated host/VM checks. Live providers, ARM64/Pi/physical LEDs and
backup/restore observations remain separately recorded manual qualification.
Exact SC1–SC3 verification is recorded in [validation](../development/validation.md)
and the [checkpoint](../../progress.md); prior step-50 evidence remains scoped to
its original source inventory.
