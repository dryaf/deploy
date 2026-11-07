# Progress checkpoint

**Current task:** resumed to finish step 49 only, then pause again. Step 49 is
integrated, focused checks pass, and the full regular test gate is running.
The earlier pause checkpoint below will be replaced with the final result.

Earlier pause: **2026-11-07, 20:57 Europe/Vienna**. The earlier in-flight disposable
CLI test was cancelled; the resumed read/mutation scenarios have since passed.
This file is a resume checkpoint; [the roadmap](docs/roadmap.md) owns the remaining
scope and [validation](docs/development/validation.md) records completed checks.

## Current state

- **Integrated through ordinary YAML step 48.** Step 48 adds delivery
  requalification while Telegram commands are disabled. It retains permissions,
  receiver ownership, queues and results. Qualification leaves commands disabled;
  re-enabling requires an explicit YAML change and apply.
- **The full regular gate through step 47 passed**, including `go vet`.
  The worker coordinator package took 1479.220 seconds. The helper bundle build
  also passed, but its artifact catalog is incomplete for release.
- Step 48 passed focused installer/coordinator tests, compiled CLI tests for both
  read and mutation modes, vet and documentation checks. A subsequent user-run
  `make test` found one test-fixture failure in conversation credential rotation;
  all other packages passed. The fixture is now fixed and focused checks pass.
  **A full post-fix regular gate is still pending.**
- **Step 49 is unfinished and is not integrated.** Its changes are in
  `dist/workspaces/yaml-command-delivery-limits/`, not the root source tree.
- No race tests or live qualification were run. There is no current usability
  completion percentage. The earlier 84/84 software score is a different milestone.

## Test-fixture repair after the pause

The user supplied a full `make test` log with one failure:
`TestManagedConversationIsolationThroughCredentialRotation` reported `invalid JSON`
at reactivation. Other packages passed, including workerctl (1460.027s); the
failed test prevented `make test` from reaching vet.

The command fixture uses `/bin/true` and mocked executable capabilities. It was
missing the new command-pause capability mock, so renewal tried to decode the
empty executable output as JSON. `managedCommandsFixtureUsing` now supplies that
mock alongside its existing command and delivery mocks. Production capability
checks are unchanged; the tests that reject incapable executables still pass.

The failing case, disabled-command renewal, permission retention and refusal of
incapable executables passed three consecutive runs (4.167s total). Package vet
passed. Logs: `/tmp/deploy-command-renewal-fixture-fix.log` and
`/tmp/deploy-command-renewal-fixture-vet.log`. The full suite was not rerun for
this focused fixture repair. Step 49 remains paused and unintegrated; when resuming,
carry this root fixture repair into its workspace and update the corresponding
baseline entry before comparing stage changes.

## Step 49: notification budgets with retained disabled commands

Intended user flow: disable commands and apply; edit notification limits in YAML;
apply; qualify; apply; explicitly re-enable commands when desired.

Implemented in the isolated workspace:

- Storage migration receipts pin the disabled command-history head and retain
  delivery ownership. New receipt versions 31/36 distinguish this path from
  command-free changes and account for retained LED output.
- Preflight checks file/SQL command authority, disabled intake and storage
  capacity. The runtime migration changes notification budgets while preserving
  command enrollment. Existing command-specific limits remain unchanged.
- Installer review, migration and inspection use the explicit `command-limits`
  transition and probe both installed executables for support. Repeated updates
  retain the original owner and command history.
- Coordinator wiring adds `notification-command-limits`, full declaration
  snapshots, receiver-history traversal and renewal after the budget change.
  Runtime compilation retains the original command enrollment; later permission
  restrictions remain in command history.
- A disposable CLI scenario exercises budget edits, qualification, permission
  narrowing, explicit re-enable, repeated changes and both workers' short commands.

This is work in progress, not a supported completed feature yet.

## Evidence and its limits

Selected logs and the step-49 baseline were copied from `/tmp` to
`dist/workspaces/yaml-pause-20261107/` so they survive temporary-file cleanup.

| Check | Result | Log basename in that directory |
| --- | --- | --- |
| Full `make test` through step 47, including vet | Passed | `deploy-yaml-command-helper-regular.log` |
| Helper bundle through step 47 | Passed; partial catalog | `deploy-yaml-command-helper-bundle.log` |
| Step 48 compiled read/mutation CLI scenarios | Passed, 405.730s total | `deploy-yaml-command-requalification-native-final.log` |
| Step 48 final pending-declaration/lost-reply checks | Passed, 0.350s | `deploy-yaml-command-requalification-pending-final.log` |
| Step 48 focused vet | Passed | `deploy-yaml-command-requalification-vet-final.log` |
| Integrated step 48 documentation/reference checks | Passed | `deploy-yaml-command-requalification-integrated-docs.log` |
| Step 49 storage checks, including saved result and pending reply | Passed, 0.246s | `deploy-yaml-command-delivery-limits-storage4.log` |
| Step 49 installer interruption and capability checks | Passed, 3.214s | `deploy-yaml-command-delivery-limits-installer3.log` |
| Existing command-renewal coordinator regression tests with step 49 changes | Passed, 0.333s | `deploy-yaml-command-delivery-limits-coordinator-initial.log` |
| Step 49 read-mode compiled CLI | Cancelled at user pause, incomplete | `deploy-yaml-command-delivery-limits-native2.log` |

The step-48 compiled CLI run preceded the final small pending-backup-declaration
and resume-order guards; focused tests covered those guards. The next integrated
regular gate still needs to cover the final source together.

The cancelled step-49 run reported `bwrap: context canceled` after 117.035s. Its
output shows one complete budget/renewal/narrowing/re-enable cycle and part of the
second cycle, ending after notification status. It is **not a passing run** and
must be rerun. The earlier `...native.log` is also **not acceptance evidence**:
its test selector missed the intended inner scenario. That selector was corrected
before `...native2.log`.

## Resume here, step by step

1. Preserve the root working tree and the step-49 workspace. There are substantial
   pre-existing tracked and untracked edits; do not reset, clean or overwrite them.
   Do not modify `dist/checkpoints/`.
2. Work in `dist/workspaces/yaml-command-delivery-limits/`. Rerun the read-mode CLI
   scenario to completion, then the mutation-mode scenario. The fixtures need
   local namespace/socket permissions but use no real destinations or services.
3. Add focused coordinator tests for step 49 itself: interrupted pause/migration,
   lost scope/completion replies, exact retry declarations, missing/changed
   authority, repeated budget changes, later renewal and permission changes.
   The passing existing renewal regression tests do not replace these tests.
4. Review the new history and renewal paths carefully. In particular, verify
   original receiver checks on retry and read-only status/plan, full declaration
   pins during unfinished operations, retained runtime/input selection after
   previous updates, and unchanged permission restrictions. Check LED-retaining
   budget migrations and capacity refusals with retained queues.
5. Run relevant installer/storage/coordinator regressions and vet. Update the
   guide, field inventory/generated reference, roadmap and validation evidence
   only for behavior that has actually passed.
6. Integrate step 49 using a baseline comparison, not a blind directory copy.
   The saved baseline is
   `dist/workspaces/yaml-pause-20261107/deploy-yaml-command-delivery-limits-baseline.json`.
   A 25-file change inventory is in that directory as `step49-changed-files.txt`.
   Exclude all nested `dist` paths and preserve root-only documentation changes.
   **No step-49 integration script has been created or applied.**
7. Run the full regular `make test` on the integrated milestone. Its current
   package deadline is 25 minutes; the last passing coordinator package was close
   to that deadline before adding steps 48–49. Review fixture scheduling or the
   deadline if necessary; do not remove assertions to make the gate pass.
8. Continue the remaining ordinary YAML work listed in the roadmap. Run race
   tests only after implementation is complete and regular tests pass.

Useful focused command, run from the step-49 workspace:

```sh
GOCACHE=/home/sal/code/deploy/dist/test-go-cache go test ./internal/workerctl \
  -run '^TestWorkerCLICommandLimits(Read|Mutation)ThroughSSH$' \
  -parallel=1 -count=1 -timeout=20m
```

The older step-48 integration script in `/tmp` has already been applied. Do not
rerun it as a new integration. Root documentation checks and `git diff --check`
passed before this checkpoint was written. No commit or staging was performed.

## Remaining scope after step 49

- Other production/shared policy, provider, destination, freshness and activity
  combinations, including recovered and command-bearing installations.
- Retired source alias/identity reuse with explicit archive and history disposition.
- Command enrollment on an already activated command-free gateway; new permission
  and audience authority; receiver upgrades; recovered/legacy qualification;
  Signal intake and interactive alert destinations.
- Production LED mapping, daemon/device replacement and recovery migrations.
- Legacy Signal takeover/adoption and relinking beyond current owned-service paths.
- Shared gateway/source helper upgrades, disable/removal/reinstallation and
  existing-directory relocation.
- Complete public-field behavior and software acceptance scenarios, release
  bundles, onboarding/prerequisites and short-command guidance.
- Final regular tests/vet, then race tests, applicable isolated host/VM checks and
  release automation.

The user will perform live Telegram/Signal, Raspberry Pi/ARM64/LED and other
hardware/host qualification manually after implementation. Do not request live
credentials or count synthetic fixture observations as live qualification.
