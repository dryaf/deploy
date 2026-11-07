# Validation record

This page preserves the latest useful verification boundary from the retired
progress journals. It is not a continuously running test report. Current work is
tracked in the [roadmap](../roadmap.md).

## Original implementation baseline

The original implementation recorded 84/84 software criteria: deployment 10,
backup 43, notification 16 and interactive commands 15. See the
[acceptance criteria](acceptance.md) for the original contracts and evidence
families. This historical count excludes unperformed live observations and does
not measure the later YAML usability milestone.

Some specialist lifecycle APIs are implemented even though their short YAML
coordination remains unfinished. Do not infer short-command availability from
the original software count.

## Latest usability checkpoint: 2026-11-07

Initial Telegram read commands, coordinated receiver/admission setup and command
processing through the backup report timer were verified as follows:

| Check | Recorded result and scope |
| --- | --- |
| Full regular `make test` and vet | Passed on the integrated command code. Predates the final missing-bot-name prompt correction and extra coordinator process-death test. |
| Final focused tests | Passed after those additions: coordinator SIGKILL, guided input, enrollment and generated field reference. |
| Final actual CLI fixture | Passed in an offline namespace, covering missing-input behavior and real helper enrollment plus unchanged apply/resume/status/plan. No live provider admission was exercised. |
| Final repository vet | Passed after the final code additions. |
| Helper bundle | Static amd64/ARM64 helpers built. Backup/relay images and native Signal inputs were absent from this partial catalog. |
| Broad race gate | Deferred until usability implementation and regular checks are complete. No final usability race pass is claimed. |
| Live provider/hardware/host qualification | Pending; the operator will perform these checks separately. |

Original local logs (temporary paths, not portable repository artifacts):

```text
/tmp/deploy-usability-command-short-regular.log
/tmp/deploy-usability-command-final-focused.log
/tmp/deploy-usability-command-final-native.log
/tmp/deploy-usability-command-final-vet.log
/tmp/deploy-usability-command-final-bundle.log
```

Frozen local source checkpoint: `dist/checkpoints/usability-read-commands-20261107`
(1,295 files). Adjacent manifest SHA256:

```text
8244ea3613d27394f72833ad0ff56c819600550fe2a44d4c27bd99d93f42a5fa
```

The checkpoint includes the final prompt fix and process-death fixture. The full
regular run followed by the final focused/CLI/vet checks is the evidence scope;
a separate full run from the frozen copy is not claimed. Local checkpoints also
retain the old implementation journals. They are not required to read these docs
and should not be edited during new work.

## Ordinary YAML workflow step: initial incident commands

Regular focused receiver/coordinator tests cover read and mutation modes, lost
replies, SIGKILL at command coordination boundaries, helper compatibility, stale
incident revisions and idempotent scheduled execution. The actual compiled CLI
passed both enrollment modes in an offline namespace, including unchanged
apply/resume/status/plan and refusal to stage a receiver without qualification.
Logs: `/tmp/deploy-yaml-incident-focused.log` and
`/tmp/deploy-yaml-incident-native.log`. No live provider observation is implied.
Full regular `make test` (all packages and vet) passed; log:
`/tmp/deploy-yaml-incident-regular.log`. The final status wording correction was
checked separately with focused tests and both actual CLI enrollment fixtures
(`/tmp/deploy-yaml-incident-final-cli.log` and
`/tmp/deploy-yaml-incident-final-native.log`). Static amd64/ARM64 helper builds
also passed. The final usability race gate remains deferred.

## Ordinary YAML workflow step: command activity

Focused tests cover disable/re-enable in both modes, lost gateway/scope/continuation
replies, preserved receiver/results, changed-input rejection and missing receipt
refusal. Backup tests retain the original reporting handoff and direct-sender
fence. The actual compiled CLI passed disable → enable → disable in both modes,
including both workers' apply/resume/status/plan after each change.

The CLI fixtures use disposable homes, local sockets, a private network namespace
and synthetic qualification records. They exercise admission and coordination;
they do not qualify a live provider or a real systemd installation.

Logs: `/tmp/deploy-yaml-command-activity-focused.log`,
`/tmp/deploy-yaml-command-activity-native.log` and
`/tmp/deploy-yaml-command-activity-vet.log`. The integrated full `make test`
(all packages and vet) passed; log:
`/tmp/deploy-yaml-command-activity-regular.log`. Both static helper builds and
local bundle assembly also passed, preserving the existing LED artifacts.
The partial bundle still lacks backup/relay images and native Signal artifacts.
Race testing remains deferred until implementation is complete.

## Ordinary YAML workflow step: command permission reductions

Focused tests cover read-only review, effective policy inspection, rejected
expansion, lost replies at coordination boundaries and original reporting-history
preservation. The compiled CLI passed permission and subject reductions in both
read and mutation modes, subsequent enable/disable cycles, both workers' ordinary
commands, and rejection of restored broader grants without changing the outer
journal. These fixtures run offline with synthetic qualification, not live
provider observations.

Logs: `/tmp/deploy-yaml-command-policy-profile.log`,
`/tmp/deploy-yaml-command-policy-coordinator.log`,
`/tmp/deploy-yaml-command-policy-backup.log` and
`/tmp/deploy-yaml-command-policy-native.log`. The integrated full `make test` gate (all packages and vet) passed; log:
`/tmp/deploy-yaml-command-policy-regular.log`. Static amd64/ARM64 helpers and
local bundle assembly passed, preserving the LED artifacts. The bundle still
lacks backup/relay images and native Signal artifacts. Race testing remains
deferred.

## Ordinary YAML workflow steps: enrollment and supported mode changes

Focused coordinator tests cover alias changes and later permission reductions
across lost replies, mode changes within an original incident receiver, rejection
of a read-only receiver upgrade before retaining an operation, and unchanged
reporting/receiver authority. Installer tests verify read-only review, retirement,
zero remaining grants and refusal to redirect audiences, restore retired
conversations or replace the worker contract. Backup tests retain reporting
history through enrollment changes.

The compiled CLI passed alias changes, conversation retirement, subsequent
permission reductions and enable/disable cycles in both modes. Log:
`/tmp/deploy-yaml-command-enrollment-native.log`. The additional CLI mode-transition check passed, including read/mutation cycles
without restored permissions and refusal of an original read-only receiver
upgrade (`/tmp/deploy-yaml-command-enrollment-mode-native.log`). The first full regular run passed all other packages but failed during initial
command admission in the offline worker CLI fixture
(`/tmp/deploy-yaml-command-enrollment-regular.log`). The fixture had a five-second
stop limit despite the unit's 35-second budget and one non-atomic manager-state
write. Both are corrected; a deterministic six-second shutdown check passed
(`/tmp/deploy-yaml-manager-stop-budget.log`). The failed CLI scenario and shutdown regression passed on rerun in both
command modes (`/tmp/deploy-yaml-manager-permissions-native.log`). The full regular retry passed the command-permission scenario but failed the
separate native gateway CLI while applying a limit update, with a service-readiness
timeout (`/tmp/deploy-yaml-command-enrollment-regular-retry.log`). Runtime diagnostics
were added. Five consecutive focused reruns passed (171.750 seconds;
`/tmp/deploy-yaml-native-readiness.log`), so the failure has not reproduced and its
cause is not yet established. The later full regular run for the production LED
output step passed (see below). Focused logs:
`/tmp/deploy-yaml-command-enrollment-coordinator.log`,
`/tmp/deploy-yaml-command-enrollment-profile.log` and
`/tmp/deploy-yaml-command-enrollment-backup.log`. All CLI qualification data is
synthetic and confined to offline fixtures; no live observation is claimed.

## Ordinary YAML workflow step: source replacement under a new alias

The compiled CLI passed separate add-then-retire applies for standalone and
shared backups. It verifies worker/archive/producer identity, unchanged original
source authorization during addition, retained new-source authorization during
retirement, a lost revocation reply, refused endpoint retargeting while pending,
resume and unchanged apply. Log: `/tmp/deploy-yaml-source-replacement-native.log`.
The fixture stages configuration and synthetic source qualification; it does not
claim a real application backup/restore or provider observation.

## Ordinary YAML workflow step: production LED output

Qualified, unrecovered gateways with local shared backups support output-only
YAML toggles. Focused tests passed for active producer ownership, interleaved
commands, lost domain/scope/continuation replies, unchanged incidents and foreign
output, old-helper refusal and expired qualification refusal before an operation.
The native Signal coordinator check resumes the retained child without relinking
and refuses a damaged output intent before restart. Log:
`/tmp/deploy-yaml-delivery-led-focused.log`.

The actual compiled CLI passed delivery-only and command-enabled installations
(155.571 seconds), toggling output three times and exercising both workers'
apply/resume/status/plan and later command toggles. Log:
`/tmp/deploy-yaml-delivery-led-native.log`. Hardware/provider evidence was synthetic
inside a network-isolated fixture. No physical or live observation is claimed.
Documentation checks and vet passed on the step's source copy. The integrated
documentation check passed (257 links), and static amd64/ARM64 helpers rebuilt
with the existing LED artifact retained (`/tmp/deploy-yaml-delivery-led-bundle.log`).
The bundle still lacks backup/relay images and native Signal artifacts. Full
regular `make test` passed all packages and vet on the step's isolated source
copy (`/tmp/deploy-yaml-delivery-led-regular.log`; worker CLI 1050.186 seconds).
That copy predates combined source replacement and the guided LED wizard. Race
remains deferred.

## Ordinary YAML workflow step: combined source replacement

One YAML apply can add new aliases and explicitly retire old aliases. Focused
tests passed for standalone/shared publication and rollback, preserved old/new
keys, read-only reservation review, rejected retargeting and changed dispositions,
process death after intent/key/enrollment, and growing/shrinking/equal-size source
sets. Logs: `/tmp/deploy-yaml-source-combined-domain.log` and
`/tmp/deploy-yaml-source-combined-final-domain.log`.

The compiled CLI passed standalone and shared replacement (37.462 seconds),
including lost enrollment and revocation replies, refused retargeting, activation
fencing, resume, unchanged apply and both workers' status/plan paths. Log:
`/tmp/deploy-yaml-source-combined-native.log`. Source qualification was simulated
inside the offline fixture; application backup/restore checks remain separate.
Documentation checks and vet passed. Static amd64/ARM64 helpers rebuilt, retaining
the LED artifact (`/tmp/deploy-yaml-source-combined-bundle.log`). Backup/relay
images and native Signal artifacts remain absent from the bundle. Full regular
checks on this final source are pending; race remains deferred.

## Ordinary YAML workflow step: guided LED qualification

The short qualification wizard now records the observed LED daemon version and
SHA-256 digest of an operator-local configuration file. Only the digest is sent;
physical checks remain explicit observations. Deferred answers leave qualification
incomplete, retries preserve the original observation window, and changed retained
environment data is refused. Backup qualification rejects gateway-only metadata.

Focused installer, backup and wizard tests, documentation checks and vet passed
on the step's source copy. The compiled CLI passed the actual terminal wizard and
subsequent production LED toggles for delivery-only and command-enabled fixtures
(188.568 seconds; `/tmp/deploy-yaml-led-qualification-native.log`). Evidence in
these network-isolated fixtures is synthetic; no physical LED or provider check
is claimed. Integrated documentation checks and static amd64/ARM64 helper builds
passed, retaining the LED artifact. The bundle still lacks backup/relay images
and native Signal artifacts. Full regular `make test` on this integrated source
failed in two native gateway fixtures with `disk quota exceeded`; all other
packages passed (`/tmp/deploy-yaml-guided-led-regular.log`). Moving the task build
cache off `/tmp` freed about 21 GiB. Both affected fixtures then passed together
(65.776 seconds; `/tmp/deploy-yaml-guided-led-quota-retry.log`). This focused retry
is not a full-suite pass; the next integrated source still needs `make test`.
Race remains deferred.

## Ordinary YAML workflow steps: shared verification policy

Shared budget, producer timing, route and existing-producer policy changes are
integrated for local shared backups before delivery activation, without LED
policy or recovery. They preserve the original enrollment, backup/archive/source
identities, pending messages and observation timestamps. Route and access-policy
changes refuse pending messages; quota reductions refuse insufficient capacity.
Unchanged explicit defaults publish the declaration without restarting services.

Focused tests passed across worker state, installer, backup scope and coordinator
boundaries, including actual process death, lost replies, missing history, native
Signal child resume, old-helper refusal, and interleaved policy updates. Final
focused log: `/tmp/deploy-yaml-shared-producer-final-focused.log`. The compiled
CLI passed the two consolidated policy scenarios with and without Telegram
commands (317.774 seconds; `/tmp/deploy-yaml-shared-producer-native.log`). They
exercise repeated budgets, explicit defaults, timing, routes, quotas, requiredness
and context capabilities, then qualification, delivery/command admission and both
workers' ordinary commands. Earlier route-only scenarios also passed (259.347
seconds; `/tmp/deploy-yaml-shared-routes-native.log`).

Documentation checks and vet passed on the final source copy. Integrated
documentation checks passed (257 local links), and the static amd64/ARM64 helper
bundle rebuilt with the LED artifact retained
(`/tmp/deploy-yaml-shared-policy-bundle.log`). Backup/relay images and native
Signal artifacts remain absent. The CLI evidence is synthetic inside isolated offline fixtures; no live provider, hardware or
backup/restore qualification is claimed. Full regular `make test` failed in two CLI scenarios when the simulated service
manager read `/proc` during process exit (`ProcessLookupError`). All other
packages passed, including the installer (1278.535 seconds); vet did not run.
Log: `/tmp/deploy-yaml-shared-policy-regular.log`. The fixture now treats both
ENOENT and ESRCH as a stopped process. Both complete affected fixtures then passed (432.670 seconds;
`/tmp/deploy-yaml-shared-policy-fixture-retry.log`), including all their command
modes. A full pass is not claimed; the next integrated regular run remains
required. Race is deferred.

## Ordinary YAML workflow step: shared Telegram profiles

Before delivery activation, local shared installations can rotate direct Telegram
credential references or the fixed alert destination. The selected provider set,
Signal service, producer identity and original enrollment remain. Both old and
new private files stay pinned; pending messages block the change. Command-enabled,
production and recovered profile changes still require separate workflows.

Focused domain/installer/backup/coordinator tests passed, including lost replies,
process death, old-helper refusal, altered credential refusal and native Signal
child resume. Logs: `/tmp/deploy-yaml-shared-providers-final-focused.log` and
`/tmp/deploy-yaml-shared-providers-coordinator-regression.log`. Both compiled CLI
policy scenarios passed (407.808 seconds;
`/tmp/deploy-yaml-shared-providers-native.log`). The consolidated assertions then
passed again (333.663 seconds;
`/tmp/deploy-yaml-shared-policy-acceptance-native.log`): every policy group checks
both workers' apply/resume/status/plan; intermediate revisions retain status/plan
checks. Documentation checks and vet passed on the source copy. This does not
replace the full regular gate or any live qualification. Race remains deferred.

## Ordinary YAML workflow step: shared policy with LEDs

The shared verification policy workflow now preserves installed LED bindings,
endpoint and completed output history. It retains unresolved faults and other
clients' output. Disabled output stays disabled across policy edits; subsequent
output toggles and further policy edits retain the same enrollment. Qualification
still records actual observations of the revised configuration.

Focused worker, installer and coordinator checks passed for mapped history,
interruption/resume, damaged or missing authority, old-helper refusal, unchanged
output and private-file preservation. Logs:
`/tmp/deploy-yaml-shared-led-final-focused.log`,
`/tmp/deploy-yaml-shared-led-kinds.log`, and
`/tmp/deploy-yaml-shared-led-providers.log`. The compiled offline CLI passed a
mapped command-enabled gateway through budgets, timing, routes and producer-policy
changes, later qualification and command admission (155.404 seconds;
`/tmp/deploy-yaml-shared-led-native2.log`). The final disabled-output and subsequent
toggle/policy sequence also passed (158.723 seconds;
`/tmp/deploy-yaml-shared-led-native3.log`). Documentation checks and vet passed on
the source copy. Production/recovery migrations remain unfinished; no live LED or
provider qualification is claimed. Full regular checks remain pending and race is
deferred.

## Ordinary YAML workflow step: alternating LED mappings and policy

Before activation, shared installations can alternate completed LED mapping and
policy edits. Each change follows the exact retained history. Missing mappings,
unproved configuration gaps and unapplied backup policy changes are refused
before a new intent is retained. The YAML mapping path still requires enabled
output; production and recovered mapping workflows remain unfinished.

Focused worker, installer and coordinator checks passed, including alternating
histories, disabled output preservation, lost replies and missing authority
(`/tmp/deploy-yaml-led-sequences-final-focused.log`). The compiled offline CLI
passed mappings before and after budget, timing, routing and producer changes,
then qualification and command admission (200.868 seconds;
`/tmp/deploy-yaml-led-sequences-native2.log`). Documentation checks and final vet
passed. These checks do not replace the full regular gate or live qualification.
Race remains deferred.

## Integrated regular check through alternating LED/policy updates

Full regular `make test` passed on the integrated source through step 16,
including all packages and vet. The worker CLI package took 1455.808 seconds.
Log: `/tmp/deploy-yaml-led-sequences-regular.log`. This includes the corrected
simulated-manager process-reaping behavior. It predates the command updates below.
The amd64/ARM64 helper bundle and documentation checks passed; the bundle remains
partial without backup/relay images and native Signal artifacts. Race is deferred.

## Ordinary YAML workflow step: restoring original command permissions

Disabled Telegram intake can explicitly restore actors, rights and subject access
within the original receiver's admitted authority. Aliases and current audiences
remain unchanged; new authority and retired audiences are refused. Accepted
commands, saved results, reporting and receiver identity remain.

Focused domain, installer and coordinator tests passed, including process death,
exact resume, old-helper refusal and recovery after restored permissions. Logs:
`/tmp/deploy-yaml-command-grants-final-focused.log` and
`/tmp/deploy-yaml-command-grants-recovery.log`. The compiled offline CLI passed
read and mutation scenarios (255.825 seconds;
`/tmp/deploy-yaml-command-grants-native2.log`). The final capability check for
later transitions is covered by focused/recovery tests; that small addition
postdates the CLI run. Documentation, vet and the actual capability probe passed.
These checks do not replace the next integrated regular gate or live qualification.

## Ordinary YAML workflow step: commands on an existing verification gateway

A command-free Telegram gateway with an enrolled local backup can declare initial
commands before delivery activation, preserving the reservation, archive, source
keys and LED output history. Both workers publish the revised declaration;
qualification, receiver setup and intake admission remain separate.

Domain/installer/backup/coordinator tests passed for durable interruption, process
death, aliases, old-helper refusal and enabled/disabled LED output. Logs:
`/tmp/deploy-yaml-existing-commands-focused.log`,
`/tmp/deploy-yaml-existing-commands-domain3.log`,
`/tmp/deploy-yaml-existing-commands-install-scope.log` and
`/tmp/deploy-yaml-existing-commands-coordinator2.log`. The compiled offline CLI
passed command enrollment after budgets, further LED and policy changes, then
qualification/admission (217.973 seconds;
`/tmp/deploy-yaml-existing-commands-native.log`). The final foreign command-history
directory refusal is covered by focused domain checks and postdates that CLI
build. Documentation and vet passed. A broader backup subset needed local socket
permissions and passed when rerun in the authorized fixture environment
(`/tmp/deploy-yaml-existing-commands-backup-retry.log`). Full regular verification
of the newly integrated command changes remains pending. Race is deferred.

## Ordinary YAML workflow step: command policy before activation

Before any delivery/command admission, existing Telegram command declarations can
change actors, rights, scopes, aliases, conversations/topics and read/mutation
mode. The backup reservation, archive, original enrollment and disabled LED output
remain; old aliases stay reserved. Missing history and existing intake/ownership
are refused. Receiver review and admission use the revised policy.

Domain, installer, coordinator and backup scope tests passed across durable
boundaries, lost replies and process death. Logs:
`/tmp/deploy-yaml-verification-commands-focused.log` and
`/tmp/deploy-yaml-verification-commands-scope.log`. The compiled offline CLI passed
actor/alias/audience changes, read-to-mutation declaration updates, both workers'
ordinary commands, later LED/policy changes and final receiver admission
(222.981 seconds; `/tmp/deploy-yaml-verification-commands-native2.log`). The first
CLI attempt lacked its source-copy entry point; restoring that directory fixed
the build prerequisite. Documentation checks and vet passed. The final plan/help
wording and bounded two-fixture test concurrency postdate this CLI build. A full
regular run is still required for the integrated command changes; race and live
qualification remain deferred.

## Integrated regular check through command policy before activation

Full regular `make test` passed on the integrated source through step 19, including
all packages and vet. Log: `/tmp/deploy-yaml-command-policy-regular.log`. The worker
CLI package took 1090.859 seconds with at most two independent offline namespace
fixtures running concurrently, compared with 1455.808 seconds in the preceding
run. This is a measured run-to-run comparison, not a benchmark guarantee. The
amd64/ARM64 helper bundle and documentation checks also passed. The bundle still
lacks backup/relay images and native Signal artifacts; no race or live pass is claimed.

## Ordinary YAML workflow step: command activity before activation

A verification gateway can disable and later restore its exact unadmitted command
policy. Alias reservations, backup scope and LED history remain; neither direction
admits intake or a receiver. Permission edits and re-enable are separate changes.

Worker, installer, coordinator and backup checks passed for both directions,
process death, lost replies, changed-policy refusal and intervening budgets. Logs:
`/tmp/deploy-yaml-verification-activity-focused.log` and
`/tmp/deploy-yaml-verification-activity-scope.log`. The compiled offline CLI passed
both toggles, both workers' ordinary commands, further LED/policy changes and final
admission (323.809 seconds; `/tmp/deploy-yaml-verification-activity-native.log`).
Documentation checks and vet passed. Final plan wording postdates that CLI build.

## Ordinary YAML workflow step: notification-first command declarations

An unbound gateway can retain changed future command settings before any backup
producer is reserved. No service restart, runtime replacement or identity
reservation occurs. Interrupted declaration publication resumes; already reserved
backup dependencies and changed installed authority are refused before effects.

Focused declaration, reservation and preceding lifecycle tests passed. Logs:
`/tmp/deploy-yaml-command-drafts-focused3.log` and
`/tmp/deploy-yaml-command-drafts-reservation.log`. The compiled offline CLI passed
notification-first edits, later limit updates, backup enrollment, verification
command updates and final admission (316.314 seconds;
`/tmp/deploy-yaml-command-drafts-native.log`). Missing-declaration errors now
preserve their cause so interrupted publication can resume; that fix and the final
plan wording are covered by focused checks and postdate this CLI build.
Documentation checks and vet passed.

## Ordinary YAML workflow step: Telegram files with unadmitted commands

Before activation, Telegram token/reference and fixed alert destination changes
can retain configured command policy for the same bot. Alert and intake tokens
stay aligned. Disabled verification commands retain their account; old/new files,
producer identity and LED history remain. Pending input or admitted commands are
refused; this does not qualify a replacement bot or production migration.

Worker, installer and coordinator checks passed across durable boundaries,
including changed-bot refusal, divergent-token refusal and retained command input.
Logs: `/tmp/deploy-yaml-command-providers-focused.log`,
`/tmp/deploy-yaml-command-providers-identity.log` and
`/tmp/deploy-yaml-command-providers-final-coordinator.log`. The compiled offline CLI
passed credential changes with configured and disabled commands, restoration,
later LED/policy changes and final admission (331.535 seconds;
`/tmp/deploy-yaml-command-providers-native.log`). Documentation checks and vet passed;
final wording/comment changes postdate that CLI build. The next full regular gate
covers these newly integrated steps together. Race remains deferred until the
implementation milestone is complete; all live qualification remains operator work.

## Regular gate through ordinary YAML step 22

Full `make test` and vet passed after integrating steps 20–22. The installer
package took 1185.826 seconds, the worker package 368.654 seconds and workerctl
1072.736 seconds. Log: `/tmp/deploy-yaml-command-batch-regular.log`. Documentation
and diff checks passed. Matching static amd64/ARM64 helpers built successfully;
the development catalog remains partial because backup/relay images and native
Signal artifacts are not supplied. Bundle log:
`/tmp/deploy-yaml-command-batch-bundle.log`.

## Ordinary YAML steps 23–25: freshness, reporting route and destination

Derived backup freshness now coordinates the staged backup runtimes, archive
checkpoint and gateway deadline. Focused checks passed for exact resume at every
publication boundary, SIGKILL with the runtime fence retained, busy readers,
changed inputs, unrelated edits and unchanged retries. Coordinator tests cover
later policy, qualification and native-service continuations. Logs:
`/tmp/deploy-yaml-derived-freshness-final-focused.log`,
`/tmp/deploy-yaml-derived-freshness-domain2.log` and
`/tmp/deploy-yaml-derived-freshness-coordinator3.log`.

Shared report-route tests passed for default producer permission changes and
selection of an explicitly allowed route without restarting. Scope tests verify
both declaration publications and preserved source/runtime authority. Logs:
`/tmp/deploy-yaml-report-route-focused3.log` and
`/tmp/deploy-yaml-report-route-scope.log`. The compiled offline CLI passed both
freshness and report-route workflows through both short commands, later policy
changes and delivery admission (209.177 seconds;
`/tmp/deploy-yaml-report-route-native.log`). The earlier longer command/LED
scenario hit the existing bounded installation history after its freshness checks;
the new cases were moved to the shorter policy scenario without increasing the
production bound or removing the command scenario's prior coverage.

Standalone Telegram destination tests passed for pending/covered/reserved work
refusal, same-bot checks, pinned inputs, version downgrade refusal and publication
rollback. The real offline CLI passed the ordinary apply/resume/status/plan path
and refusal to activate without qualification (9.661 seconds). Logs:
`/tmp/deploy-yaml-backup-destination-regression.log`,
`/tmp/deploy-yaml-backup-destination-focused.log` and
`/tmp/deploy-yaml-backup-destination-native.log`. No provider sends are exercised.

Focused vet and documentation checks passed for these three steps; final reference
wording postdates the CLI builds. They are now integrated for the next full
regular gate. Race remains deferred until the implementation milestone is complete;
manual provider/hardware qualification remains operator work.

## Regular gate through ordinary YAML step 25

Full `make test` and vet passed after integrating steps 23–25. Workerctl took
1024.557 seconds; the unchanged installer package reused its passing cache.
Log: `/tmp/deploy-yaml-reporting-batch-regular.log`. Documentation/diff checks and
static amd64/ARM64 helper builds passed. The artifact catalog remains partial;
backup/relay images and native Signal artifacts are still absent. Build log:
`/tmp/deploy-yaml-reporting-batch-bundle.log`.

## Ordinary YAML steps 26–27: Signal startup and direct Telegram profiles

Fresh Signal bootstrap now accepts an explicit local backup-producer declaration
while withholding its identity and authority until reservation. Compiler and
coordinator tests passed for Signal-only/combined setup, later enrollment,
explicit route selection and refusal of unknown producers or incompatible policy.
Logs: `/tmp/deploy-yaml-signal-producer-compiler.log` and
`/tmp/deploy-yaml-signal-producer-focused.log`.

An owned Signal gateway can add/remove an unused direct Telegram profile before
activation. Installer and coordinator tests passed for interrupted publication,
repeated removal/re-addition, retained native ownership, unchanged backup
enrollment and subsequent policy edits. Rejection tests cover referenced profiles,
foreign authority, credential drift, invalid private files and incapable helpers.
Logs: `/tmp/deploy-yaml-profile-focused.log` (workerctl 5.565 seconds;
installer 52.185 seconds), `/tmp/deploy-yaml-profile-coordinator2.log` and
`/tmp/deploy-yaml-profile-installer.log`. These are offline domain/coordinator and
native-account fixtures; no live Signal or Telegram exchange was performed.
Documentation checks and focused vet passed. The integrated regular gate through
step 29 also passed; race remains deferred until the implementation milestone is complete.

## Ordinary YAML steps 28–29: delivery qualification renewal

Focused installer and coordinator tests passed for repeated pause/qualification/
activation cycles, active producers, retained queued messages, lost replies,
missing proof and refusal of old observations. The guided draft is bound to each
pause so identical configuration cannot reuse the previous cycle's report.
Owned native Signal fixtures also retain account/inbox records, resume the same
service explicitly and reject changed artifact pins. The wizard passes only the
selected native digest; it does not transfer credentials or relink accounts.
Final focused log: `/tmp/deploy-yaml-requalification-final29.log` (installer 3.136
seconds; workerctl 0.819 seconds). Documentation checks and focused vet passed:
`/tmp/deploy-yaml-requalification-docs29.log` and
`/tmp/deploy-yaml-requalification-vet29.log`.

The compiled offline CLI passed one renewal cycle in
`/tmp/deploy-yaml-requalification-native5.log` (218.174 seconds). That run predates
the repeated-draft fix; the final CLI fixture now exercises two cycles. The
integrated full `make test` and vet passed in
`/tmp/deploy-yaml-requalification-batch-regular.log` (workerctl 1071.787 seconds),
including the final two-cycle CLI fixture. Static amd64/ARM64 helper builds passed
in `/tmp/deploy-yaml-requalification-batch-bundle.log`; the catalog remains partial.
No live provider exchanges occurred. Race remains deferred.

## Ordinary YAML step 30: renewal after production LED output changes

Short qualification/apply now renews delivery after supported production LED
output toggles, retaining the original output permission, backup scope receipts,
reporting snapshot and other LED clients. Originally disabled output cannot gain
permission by renewal. Focused installer/backup/coordinator tests passed, including
repeated cycles, interrupted pauses, missing historical receipts and reversed
snapshot ancestry. Log: `/tmp/deploy-yaml-led-requalification-focused8.log`.

The actual compiled CLI passed three alternating output/renewal cycles and both
workers' apply/resume/status/plan in an offline namespace (161.362 seconds):
`/tmp/deploy-yaml-led-requalification-native4.log`. Final focused vet and
documentation checks passed. The integrated full `make test` and vet passed
in `/tmp/deploy-yaml-led-requalification-regular.log` (workerctl 1056.778 seconds). Static amd64/ARM64 helper builds also passed in
`/tmp/deploy-yaml-led-requalification-bundle.log`; the catalog still lacks the
backup/relay images, LED artifacts and native Signal inputs. No live LED or
provider observations were performed; race remains deferred.

## Ordinary YAML step 31: production Telegram credential references

The short apply/qualify/apply workflow rotates private file references for the
same bot and chat on an unrecovered Telegram-only shared gateway without command
history. It preserves active producers and queued messages, fences reporting
while expanding old/new credential exclusions, and retains the original reporting
handoff. Both declarations, service units and exact retries remain coordinated.
Signal, command, destination and recovered variants are still separate work.

Focused tests passed for interrupted pause/rotation/report publication, repeated
rotations, retained LED output states, input drift, changed audiences, old helper
refusal and read-only inspection. Credential-history pins survive deactivation,
a new specialist admission and specialist recovery; missing history cannot drop
old report exclusions. Logs: `/tmp/deploy-yaml-delivery-provider-final-focused.log`,
`/tmp/deploy-yaml-delivery-provider-final-domain.log`,
`/tmp/deploy-yaml-delivery-provider-led-coordinator2.log` and
`/tmp/deploy-yaml-delivery-provider-recovery-exclusions.log`. Final focused vet and
documentation checks passed.

The final compiled CLI passed two full rotations, qualification, audience-change
refusal and both workers' apply/resume/status/plan in an offline namespace
(69.882 seconds): `/tmp/deploy-yaml-delivery-provider-native5.log`. Earlier CLI
runs found and led to fixes for apply success reporting and validation of the
original reporting handoff against expanded report units. The integrated full
regular gate and vet passed in `/tmp/deploy-yaml-delivery-provider-regular.log`
(installer 1169.915 seconds, workerctl 1165.853 seconds). Fixtures do not qualify real providers or systemd;
race remains deferred until the implementation milestone is complete.

## Ordinary YAML steps 32–33: owned Signal and production limits

Production Telegram credential rotation now retains an unchanged owned native
Signal service. Production notification budget changes retain the original
immutable delivery owner, active generations, pending messages and report-service
restrictions. Both workers' declarations advance together; delivery resumes only
after fresh qualification. These paths cover unrecovered direct-alert gateways;
command-bearing/recovered changes and declaration-only budget no-ops remain open.

Focused tests passed for interrupted coordinator, store, installer and reporting
transitions, repeated budget changes, native service resume, Signal-only gateways,
LED output preservation and unresolved faults. Changed native artifacts/private
inputs, lost intents and undersized budgets are refused before their relevant
effects. Logs:

- `/tmp/deploy-yaml-signal-delivery-provider-final-focused.log`
- `/tmp/deploy-yaml-signal-delivery-provider-domain.log`
- `/tmp/deploy-yaml-production-limits-worker-led.log`
- `/tmp/deploy-yaml-production-limits-coordinator-final2.log`
- `/tmp/deploy-yaml-production-limits-signal-only.log`
- `/tmp/deploy-yaml-production-limits-coordinator2.log` (backup scope tests passed;
  coordinator fixture failures were corrected and passed in later logs)

The compiled offline CLI passed budget change → credential rotation → budget
change, qualification, and both workers' apply/resume/status/plan commands
(118.509 seconds): `/tmp/deploy-yaml-production-limits-native2.log`. This precedes
the final native-resume guard changes, which passed the focused coordinator tests.
Documentation checks passed. The full regular worker/configuration packages
passed in `/tmp/deploy-yaml-production-limits-worker-regular.log` (worker 307.628
seconds). Static amd64/ARM64 helpers built successfully in
`/tmp/deploy-yaml-production-policies-bundle.log`; the catalog still lacks the
backup/relay images, LED artifacts and native Signal inputs. The integrated regular suite in `/tmp/deploy-yaml-production-policies-regular.log`
passed the installer (1286.427 seconds) and worker (382.760 seconds), but failed
one CLI assertion that still expected production budgets to be unsupported.
That assertion now checks refusal of a mixed activity/budget edit; the combined
suite below verifies the correction. Vet did not run after that failed make target. No live provider or hardware observations were performed, and race
checks remain deferred until implementation is complete.

## Production producer, route and provider workflows (steps 34–37)

Integrated on 2026-11-07. Unrecovered command-free local shared gateways now support
producer policy, route changes, Telegram chat/bot changes, and unused Telegram
addition/removal beside unchanged owned Signal. Tests cover retained original
ownership and observations, queued-work preservation where allowed, settled-queue
requirements for access/destination changes, interrupted publication and retries,
reporting fences and credential exclusions, and enabled/disabled LED history.

Focused checks passed:

- Producer storage, coordinator, installer and reporting tests:
  `/tmp/deploy-yaml-production-producer-worker-final.log`,
  `/tmp/deploy-yaml-production-producer-coordination.log` and
  `/tmp/deploy-yaml-production-producer-route-guard.log`.
- Route storage, coordinator, installer and reporting tests:
  `/tmp/deploy-yaml-production-routes-worker-final.log` and
  `/tmp/deploy-yaml-production-routes-coordination.log`.
- Destination checks: `/tmp/deploy-yaml-production-destinations-worker-final.log`,
  `/tmp/deploy-yaml-production-destinations-coordination.log` (coordinator and
  reporting passed; its installer fixture was corrected), and
  `/tmp/deploy-yaml-production-destinations-domain.log` (corrected installer passed).
- Provider enrollment: `/tmp/deploy-yaml-production-provider-enrollment-coordination.log`
  (installer and coordinator passed; its backup fixture was corrected), and
  `/tmp/deploy-yaml-production-provider-enrollment-backup2.log` (corrected reporting
  fixture passed). This step has component coverage, not a dedicated compiled
  native-Signal CLI walkthrough.

Compiled offline CLI fixtures passed producer → credential → producer policy
(127.316 seconds), route addition → credential rotation → route removal
(136.379 seconds), and chat change → same-chat credential rotation → bot/chat
change (133.697 seconds). Logs:
`/tmp/deploy-yaml-production-producer-native.log`,
`/tmp/deploy-yaml-production-routes-native.log` and
`/tmp/deploy-yaml-production-destinations-native.log`. Each checks both workers'
apply/resume/status/plan and explicit fixture qualification. These runs precede
the final provider-enrollment dispatcher and the corrected mixed-edit assertion.

Documentation checks and focused vet passed for each step. The full integrated
`make test` plus vet passed through step 37 in
`/tmp/deploy-yaml-production-workflows-regular.log` (installer 1280.614s,
coordinator 1345.286s). It predates steps 38–39 below.
Static amd64/ARM64 helpers built successfully in
`/tmp/deploy-yaml-production-workflows-bundle.log`. The catalog remains partial: both
architectures lack backup/relay images and native Signal/LED artifacts.
Race checks remain deferred. No live provider, physical Pi or VM qualification
was performed.

## Ordinary YAML workflow steps 38–39: declarations and derived freshness

Equivalent explicit/derived producer defaults retain the running gateway and
qualification. Read-only review works with a live runtime lock and queued report,
and rejects missing owner records or effective edits presented as no-ops. The
compiled offline CLI passed repeated declaration changes and a subsequent real
budget change (`/tmp/deploy-yaml-production-declaration-native3.log`, 107.554s).
Focused storage/installer/coordinator/backup checks, documentation and vet passed;
logs use the `/tmp/deploy-yaml-production-declaration-` prefix.

Production derived freshness coordinates the default producer deadline, both
backup runtimes and archive checkpoint. Focused tests pass at each publication
boundary, preserve reporting ownership and stage capture; coordinator cases cover
unchanged native Signal and enabled/disabled LEDs. The actual CLI passed two
successive updates, one through each worker, followed by qualification and both
workers' ordinary commands (`/tmp/deploy-yaml-production-derived-freshness-native3.log`,
92.361s). Runtime projection is limited to completed, pinned freshness history;
unrelated edits are rejected. Final focused checks and vet passed in
`/tmp/deploy-yaml-production-derived-freshness-final-focused.log` and
`/tmp/deploy-yaml-production-derived-freshness-final-vet.log`.

The combined regular gate passed the backup and notification packages but failed
in the command-enrollment CLI fixture during initial worker readiness
(`/tmp/deploy-yaml-production-freshness-regular.log`, workerctl 1275.985s).
The failure preceded that scenario's configuration edits. Its focused rerun
passed both read and mutation enrollment scenarios in 404.827s
(`/tmp/deploy-yaml-command-enrollment-readiness-recheck.log`). The cause of the
initial readiness timeout remains unconfirmed; fixture failures now include the
worker's runtime log. The failed full gate is not claimed as passed. No race or live provider,
hardware or VM qualification is claimed. Further ordinary YAML work remains in
the roadmap, including backup policy updates following shared policy history.

## Ordinary YAML step 47: compatible command-bearing backup helper upgrades

The offline CLI passed read and mutation modes through an interrupted helper/image
upgrade, exact resume, command disable/re-enable, and a second helper upgrade.
Both workers passed apply/resume/status/plan after each change. Original receiver
and reporting records, pending requests and saved results remained retained.
`/tmp/deploy-yaml-command-helper-native-final.log` records 161.672 seconds.
Artifacts are synthetic compatible test executables, not release ABI qualification.

Focused helper review/publication/rollback checks passed (4.907s,
`/tmp/deploy-yaml-command-helper-backup-final2.log`), as did coordinator regressions
for complete declaration snapshots, applied-backup continuation and refusal of
unapplied or changed authority (`/tmp/deploy-yaml-command-helper-coordinator-final.log`).
Earlier CLI failures exposed notification-scope snapshot reuse and history cycles;
command operations now retain their complete declaration without replacing old
scope snapshots. Final focused vet and documentation checks passed in
`/tmp/deploy-yaml-command-helper-vet-final.log` and
`/tmp/deploy-yaml-command-helper-docs-final.log`.

The integrated regular gate remains pending. Race checks remain deferred until
implementation and regular checks are complete. No live qualification was run.

## Recording future results

Use [development and testing](README.md) for commands. Record the exact source,
command, outcome and skipped environments for a release checkpoint. Keep current
work in the roadmap and keep manual qualification separate. Replace superseded
status narratives instead of appending another competing progress ledger.


## Ordinary YAML step 40: backup updates after shared notification changes

Completed shared policy history now supports subsequent backup schedule, snapshot
budget and independent freshness changes. Immutable completion receipts retain the
original reporting handoff and source ownership; notification changes can follow
those completed backup changes. Missing receipts and unapplied backup edits are
refused before a new notification operation.

Focused backup/coordinator checks and vet passed
(`/tmp/deploy-yaml-shared-backup-continuation-focused3.log` and
`/tmp/deploy-yaml-shared-backup-continuation-final-vet.log`). The compiled CLI
sequence passed in 182.742s
(`/tmp/deploy-yaml-shared-backup-continuation-native4.log`): alternating freshness,
schedule and snapshot-budget changes, equivalent explicit producer defaults, and
an independent backup deadline change, with both workers' apply/resume/status/plan.
Documentation checks passed. These checks do not replace the next integrated
regular gate; race and live qualification remain deferred.


## Ordinary YAML step 41: retained source endpoint moves after shared changes

Typed source endpoint receipts preserve the original runtime and reporting handoff
after completed notification/backup changes. Projection checks the runtime as well
as the worker policy fingerprint, since changing an inventory endpoint leaves
that fingerprint unchanged. Missing history and unrecorded endpoint changes are
refused.

Focused backup checks passed in
`/tmp/deploy-yaml-shared-source-endpoints-focused.log`; vet and documentation
checks also passed. The compiled CLI passed in 224.141s
(`/tmp/deploy-yaml-shared-source-endpoints-native.log`), covering a lost inspection
reply, resume, both workers' ordinary commands and a later notification limit
change. The integrated regular gate through this step reached the 25-minute workerctl
package timeout (`/tmp/deploy-yaml-shared-history-regular.log`, 1500.085s).
No individual assertion failure preceded the timeout; the gate did not reach vet.
The expanded namespace-fixture suite now permits three concurrent fixtures
(previously two); the next regular gate must verify that scheduling change.
No live qualification or race result is claimed.


## Ordinary YAML steps 42–44: source lifecycle after shared policy changes

Typed source history now supports additions, retirement and combined replacement
after completed shared notification and backup changes. Enrollment survives local
rollback; retirement completion requires the retained remote acknowledgements.
Combined replacement keeps both intents after local publication and before remote
revocation completes. Original reporting ownership and source-key history remain.

Focused publication/rollback/history checks passed in
`/tmp/deploy-yaml-shared-source-addition-focused3.log` (4.511s),
`/tmp/deploy-yaml-shared-source-retirement-focused.log` (7.475s) and
`/tmp/deploy-yaml-shared-source-replacement-focused.log` (10.106s).
The compiled CLI passed each corresponding sequence:

- Addition and a later notification change, with the new source's enrollment reply
  lost: `/tmp/deploy-yaml-shared-source-addition-native2.log` (107.425s).
- Addition, retirement, lost revocation reply, resume and later notification
  changes: `/tmp/deploy-yaml-shared-source-retirement-native.log` (178.173s).
- Combined replacement, with both enrollment and revocation replies lost:
  `/tmp/deploy-yaml-shared-source-replacement-native.log` (109.625s).

The longer addition-after-endpoint sequence also passed in
`/tmp/deploy-yaml-shared-source-addition-native.log` (320.662s); its fixture was
subsequently split so the specific new-source reply loss is checked independently.
Documentation checks and vet passed for each step. These checks used disposable
offline fixtures, not real providers, source hosts or hardware.


## Ordinary YAML step 45: shared backup-image changes

Pinned image-only updates retain the backup helper, source helpers, endpoints,
archive and original reporting handoff. The producer reservation reviews the
future apply without writes and accepts the exact pending image on resume.
Mixed helper, source, policy, enablement and unpinned-image changes are refused.

Focused publication/rollback and source-history checks passed in
`/tmp/deploy-yaml-shared-backup-image-final-focused.log` (11.749s), with focused vet
in `/tmp/deploy-yaml-shared-backup-image-final-vet.log`. Read-only review and pending
reservation checks also passed. The compiled CLI passed after a lost source
inspection reply, retained the same image operation, and completed a subsequent
notification limit change (`/tmp/deploy-yaml-shared-backup-image-native4.log`,
160.866s). Documentation checks passed. The earlier fixture needed the archive
path and recovery supervisor that a completed real installation already owns;
those facts are now seeded before capturing its notification history.

The integrated regular gate through steps 42–45 completed in 1414.984s with one
failure: `TestWorkerCLICommandEnrollmentChangesThroughSSH` reached its eight-minute
namespace deadline while running both modes sequentially. No assertion failure
preceded that deadline; other packages passed. The regular gate did not reach vet
(`/tmp/deploy-yaml-shared-source-image-regular.log`). The two modes now run in
separate fixtures under the same three-fixture concurrency limit; each retains
all assertions and its eight-minute deadline. The next gate must verify this split.
The helper bundle build and documentation checks passed. Race and live
qualification remain deferred.


## Ordinary YAML step 46: command-free shared backup helper upgrades

A typed helper review and completion receipt retain the original artifact,
reporting handoff and source/producer identities. Local rollback restores the old
artifact while retaining the exact resumable upgrade. Changed retry YAML, retained
command authority and mixed source/policy edits are refused. A read-only reporting
view validates the new helper without admitting another cutover.

The regular backup package passed in 225.516s
(`/tmp/deploy-yaml-helper-backup-package.log`). Final focused tests also passed
(11.708s, `/tmp/deploy-yaml-helper-final-focused.log`), including replay after a lost
outer completion write, malformed snapshots and unchanged immutable receipts.
Vet and documentation checks passed. The compiled CLI passed after source
replacement, an image update, a lost helper-upgrade inspection reply, resume and
a later notification change (`/tmp/deploy-yaml-helper-native.log`, 277.185s).
The fixture uses distinct offline artifact bytes and a simulated container
self-test; it does not qualify real release compatibility or providers.
The integrated gate including this step remains pending.

## Integrated regular gate through step 47

`GOCACHE=dist/test-go-cache make test` passed, including vet, in
`/tmp/deploy-yaml-command-helper-regular.log`. The worker coordinator package
completed in 1479.220s. This verifies the split command-enrollment fixtures and
steps 42–47 together. The helper bundle build also passed in
`/tmp/deploy-yaml-command-helper-bundle.log`; its partial artifact catalog does
not constitute release or live qualification.

## Ordinary YAML step 48: renew delivery with disabled commands

The short qualification workflow now pauses and renews an unrecovered gateway
whose command intake has already been disabled. It preserves command permissions,
receiver ownership, pending requests and results. Fresh delivery qualification
leaves intake disabled until the user explicitly enables it in YAML and applies.

Installer interruption, permission-retention and LED checks passed. The final
compiled CLI fixtures passed for read mode (195.90s) and mutation mode (209.80s)
in `/tmp/deploy-yaml-command-requalification-native-final.log` (405.730s total).
They exercise repeated renewals, narrower permissions, explicit re-enable and
both workers' apply/resume/status/plan commands. Focused pending-operation checks
subsequently passed for the final backup-declaration guards and lost replies
(`/tmp/deploy-yaml-command-requalification-pending-final.log`, 0.350s).
Vet, documentation links and the generated field reference passed. These are
synthetic offline observations, not provider qualification. A full integrated
regular gate including step 48 remains to be run; race tests remain deferred.

## Step 48 follow-up: conversation-rotation fixture repair

A user-run full `make test` passed all other packages, including workerctl
(1460.027s), but failed `TestManagedConversationIsolationThroughCredentialRotation`
with `invalid JSON` during delivery reactivation. Vet was not reached.
The tiny `/bin/true` command fixture lacked the new command-pause capability mock;
its empty output was being parsed as capability JSON. The fixture now supplies
that mock. Production capability enforcement is unchanged.

The failing test, disabled-command renewal, retained permission reductions and
incapable-executable refusal tests passed three consecutive runs in 4.167s
(`/tmp/deploy-command-renewal-fixture-fix.log`). Package vet also passed
(`/tmp/deploy-command-renewal-fixture-vet.log`). A full post-fix regular gate
remains pending. No race or live checks were run for this repair.

## Ordinary YAML step 49: notification budgets with disabled commands

Limits-only changes on unrecovered Telegram gateways preserve disabled command
history, original receiver/reporting ownership, queued commands, saved results,
reply destinations and LED output disposition. Receipt versions 31/36 pin the
exact disabled command history and retained LED output where applicable. Both
installed executables must support this migration. Reduced budgets are checked
against retained message and storage usage before the delivery pause. Derived
producer message limits follow their global budget. Delivery is qualified again
and command intake remains disabled until explicitly enabled in YAML.

Focused coordinator, installer and storage checks passed in
`/tmp/deploy-yaml-command-limits-final-focused.log` (1.826s, 2.656s and 0.283s,
respectively). They cover repeated changes, lost pause/migration/scope/renewal
replies, changed retry declarations, lost receiver authority, read-only durable
receipts, permission/enrollment retention, saved results/replies, LED output and
foreign clients. SQLite read-only preflight may create coordination sidecars;
its durable database and ownership/history records remain unchanged.

Both compiled CLI modes passed in the resumed offline run
`/tmp/deploy-yaml-command-limits-native-resume.log` (307.491s total). Further
receiver rechecks and derived-budget handling were then covered by focused tests
and by the integrated regular gate against final source, which runs those CLI
scenarios again. They exercise two budget changes, narrower permissions, explicit
re-enable and both workers' apply/resume/status/plan commands.

`GOCACHE=dist/test-go-cache make test` passed after integration, including
`go vet ./...`, in `/tmp/deploy-yaml-step49-integrated-regular.log`. The worker
coordinator package reported 1942.693s. This gate also covers step 48 and the
conversation-rotation fixture repair. The regular package timeout is now 35
minutes to accommodate the expanded compiled SSH scenarios; successful tests
finish normally and caching remains enabled. Documentation/reference checks
passed. No race tests or live qualification were run.

Logs and the integrated-file hash inventory are retained in
`dist/workspaces/yaml-step49-evidence/`. Step 49's workspace is retained for
reference; its code has been integrated and must not be copied over the root
again. Work is paused after step 49.
