# Progress checkpoint

Paused at the user's request after completing step 49 on **2026-11-07, 22:18 Europe/Vienna**.
The [roadmap](docs/roadmap.md) lists the remaining scope. No implementation or
test process is left running by this task.

## Completed and integrated

Ordinary YAML steps 1–49 are integrated. Step 49 lets an unrecovered Telegram
gateway with already admitted local backup commands change notification queue
and storage limits while retaining disabled command authority.

The user workflow is:

1. Set `notification_worker.commands.enabled: false` and apply.
2. Change only `notification_worker.limits`; plan and apply.
3. Qualify the notification worker, then apply to resume delivery.
4. Set commands enabled and apply explicitly when intake is wanted again.

Both read and mutation modes retain the original receiver, reporting owner,
permission restrictions, queued commands, saved results and reply destinations.
Existing LED disposition and foreign clients remain. Global message budgets also
update derived producer limits; command-specific budgets stay separate. Reduced
budgets are refused when retained messages or storage will not fit. Repeated
changes, exact retries and later independent qualification renewals are supported.
Recovered gateways and other production command-policy/provider migrations remain
outside this completed step.

Coordinator completion added checks for the original receiver before effects on
first apply and retry, and during status/plan. Typed history and full declaration
snapshots bind interrupted changes to their original target, helper and YAML.
The test-fixture capability repair from the earlier failure is included.

## Verification

- Full `GOCACHE=dist/test-go-cache make test`: **passed**, including all regular
  packages and `go vet ./...`. Worker coordinator package: 1942.693s.
- The integrated gate runs the read- and mutation-mode compiled CLI scenarios
  against final source. The earlier resumed focused CLI run also passed in
  307.491s; final receiver and derived-budget refinements were checked by the
  integrated gate.
- Focused coordinator/installer/storage checks passed for lost replies,
  interruption/resume, repeated updates, changed/missing authority, retained
  command results/replies, independent renewal and LED/foreign-client retention.
- Documentation links, generated field reference and whitespace checks passed.
- Race tests and manual/live qualification remain deferred.

The regular package timeout changed from 25 to 35 minutes for the expanded
compiled SSH scenarios. This is a timeout ceiling, not an added delay.

Final regular log: `/tmp/deploy-yaml-step49-integrated-regular.log`.
Durable local copies, focused logs, integration script and a 33-file SHA-256
inventory are in `dist/workspaces/yaml-step49-evidence/`.
See [validation](docs/development/validation.md) for exact check scope and history.

## Resume after this break

Continue with the next chosen item in the roadmap. **Step 49 is already finished
and integrated**; do not reapply its old integration script or copy its workspace
over the root. `dist/workspaces/yaml-command-delivery-limits/` and the older pause
evidence remain as references. The historical cancelled run is superseded by
completed focused and integrated checks.

Preserve the current working tree: it contains extensive pre-existing tracked
and untracked work. No reset, cleanup, staging or commit was performed. Do not
modify `dist/checkpoints/`. No subagents were used.

Remaining work includes:

- Other production/shared provider, destination, freshness and activity changes,
  including recovered and command-bearing variants.
- Retired source alias/identity reuse with explicit archive/history disposition.
- Command enrollment on an activated command-free gateway, new permission/audience
  authority, receiver upgrades, recovered/legacy qualification, Signal intake and
  interactive alert destinations.
- Production LED mapping/device/daemon replacement and recovery migrations.
- Legacy Signal takeover/adoption and relinking beyond current owned-service paths.
- Shared gateway/source helper upgrades and complete disable/removal/reinstallation
  and existing-directory relocation workflows.
- Public-field acceptance, release bundles, onboarding/prerequisites and guidance.
- The separately requested application workflow for local image builds, direct SSH
  transfer, signing and remote verification; it remains planned, not implemented.
- Final milestone regular tests, then race tests, applicable isolated host/VM
  checks and release automation.

The user will perform live provider, Raspberry Pi/ARM64/LED and other host/hardware
qualification manually after implementation. Do not request live credentials or
count fixture evidence as live qualification. No current usability completion
percentage is asserted; the older 84/84 software score is a different milestone.
