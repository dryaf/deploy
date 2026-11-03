# Implementation handoff — 2026-11-03

Work resumed on 2026-11-03 at the user's request. The latest checkpoint below
supersedes the original pause and its immediate next-step list. Preserve the
working tree; no implementation commit has been created.

## Latest checkpoint: managed Signal verification service

Continued through 2026-11-04. Added the separate native user-service lifecycle
for an already enrolled Signal verification endpoint and gateway supervision
adoption. This supersedes the prior checkpoint's first next step. All changes
remain in the existing dirty working tree; no commit, real service installation,
provider call, remote-host modification or LED operation was performed.

Implemented:

- `signal-service-setup`, `signal-service-resume`, `signal-service-inspect` and
  `signal-service-uninstall` manage the fixed separate
  `deploy-notification-signal-verification.service`. Setup uses an empty private
  root, a frozen native executable, independently retained endpoint manifest,
  root/coordination inode pins and immutable intent/ready/completion records.
  Candidate capability is checked before endpoint binding or unit publication.
- The endpoint retains an independent service root/hash binding. Once enrolled,
  ordinary foreground verification cannot bypass managed ownership. Account
  recovery records and original native/Podman relay units/configuration remain
  unchanged. The verifier is native even for a recovered Podman relay.
- A resumable journal covers binding, unit/publication, reload, enable/start and
  health. A successful active retry avoids reload/start; an exact inactive
  installation may restart. Lost staging after binding, replaced coordination
  files, missing records and changed permissions fail closed. Runtime holds a
  shared installation lease plus existing endpoint/account/inbox authority.
- Service readiness verifies the exact loaded unit and typed ExecStartEx,
  absence of aliases/drop-ins/environment/mount overrides, bounded stop profile,
  actual executable/UID/argv and Unix socket peer PID. Complete property output
  survives a nonzero `systemctl show` exit so an unowned/not-found unit can be
  proven; partial/error output is refused. The final regression exercises a real
  subprocess failure with stdout retained and private stderr excluded.
- The runtime checks its installed executable inode, manifests, root/lease pins,
  exact unit/executable permissions, endpoint publication and removal fence on
  startup and every audit. Its unit exposes only Unix networking and permits
  writes only to the endpoint root. Account/inbox checks stay read-only.
- Uninstall freezes removal intent, stops/drains the owned process, disables and
  removes only the exact unit, reloads and verifies absence. It retains binaries,
  publication and account fences. Interrupted removal resumes without activation.
  Foreign units are neither stopped nor deleted. Remove the gateway first, then
  the verifier. A retired gateway may validate retained service removal and repeat
  completed uninstall/resume; an installed gateway never accepts that removal as
  service authority. Combined-provider tests cover this deinstallation sequence.
- Gateway references may now include `service_root`/`service_hash`. A first
  `adopt-signal` can adopt the supervised endpoint directly. `supervise-signal`
  upgrades an already adopted foreground binding without changing endpoint or
  runtime bytes. The new operation retains its predecessor and upgrades the
  helper; candidate/helper require a distinct supervision capability. Gateway
  units gain Requires/After ordering. Updates retain the pins; rollback cannot
  cross back to an unsupervised binding.
- Existing Telegram renewal evidence crosses both the original adoption and the
  explicit supervision enrichment without polling or re-dating. Regression
  tests found and fixed reused-struct JSON decoding retaining stale predecessor
  fields; predecessor intents are now decoded into a fresh value.
- Added service/supervision example specs, expanded SIGNAL_VERIFICATION.md and
  README, updated all three plans, and included the native service integration
  test in the isolated test script.

Validation:

- Initial service lifecycle/interruption/authority-loss/combined-Telegram tests:
  32.217s, `/tmp/deploy-signal-service-tests3.log`.
- Eight real SIGKILL boundaries plus binding-gap/lost-staging and foreign-unit
  removal checks: 11.810s, `/tmp/deploy-signal-service-crash.log`. Resume succeeds
  with the original binary input deleted after durable staging. Manager state is
  persisted test state, not a real user manager.
- Expanded final lifecycle/CLI/permissions/supervision interruption suite:
  59.076s, `/tmp/deploy-signal-service-final.log`.
- Native service + gateway with actual executables, capability and health checks:
  `/tmp/deploy-signal-service-native.log` (7.112s). Expanded under race with real
  Unix peer PID, systemd unit syntax checks and evidence-loss termination:
  `/tmp/deploy-signal-service-native-race.log` (44.770s).
- Native integration after final unit/permission checks: 7.200s,
  `/tmp/deploy-signal-service-native-final.log`. Only the manager is simulated;
  processes, leases, sockets and /proc evidence are real. No runtime is
  intentionally left running after successful cleanup.
- Full repository tests passed: `/tmp/deploy-signal-service-all.log`, installer
  197.481s, backup 33.870s; worker/other packages passed or were cached. Subsequent
  localized permission/manager tightening is covered by the expanded tests above
  and final manager regression below.
- Final manager property/nonzero-output regressions passed:
  `/tmp/deploy-signal-service-manager-final.log` (0.008s).
- Final `go vet ./...` and Linux ARM64 cross-build passed:
  `/tmp/deploy-signal-service-vet-complete.log` and
  `/tmp/deploy-signal-service-arm64-complete.log`; binary
  `/tmp/deploy-signal-service-arm64`. This is not hardware qualification.
- The broad race suite hit its 300s timeout during a later interruption fixture's
  SQLite recovery preparation (no data-race report). It did not finish and is not
  counted as passed: `/tmp/deploy-signal-service-race.log`. Normal interruption
  coverage passed. The focused lifecycle/manager/both-supervision-path race run
  passed (148.401s): `/tmp/deploy-signal-service-race-targeted.log`.
- The final retained-removal boundary passes normally (7.706s) and under race
  (133.760s): `/tmp/deploy-signal-service-removal-boundary.log` and
  `/tmp/deploy-signal-service-retired-race.log`. Completed gateway uninstall/resume
  still validates after verifier removal, while a live gateway rejects it.
- CLI dry-run/regression checks passed (2.435s):
  `/tmp/deploy-signal-service-cli-final.log`.
- The final broader Signal/uninstall rerun initially hit sandbox `setsockopt`
  denials only in Unix-health fixtures (`/tmp/deploy-signal-service-retired-final.log`).
  The authorized rerun outside the sandbox passed all selected Signal/service/
  uninstall regressions (85.262s):
  `/tmp/deploy-signal-service-retired-final-unsandboxed.log`.
- All test/build runs from this checkpoint have finished. No test-owned service
  or runtime is intentionally left running. The broad race timeout remains a
  coverage limit; the focused race and native integration runs above passed.
- Example JSON, shell syntax, Go formatting and tracked diff whitespace checked.

Remaining work:

1. Compose production provider/command admission and explicit activation gates;
   migrate outbound Signal profile pins through a proven transition. Verification
   health grants no delivery, intake or command execution authority.
2. Implement immutable-service binary upgrade/re-enrollment and replacement for
   later recovery epochs. This first verifier installation supports exact restart
   and terminal removal, not arbitrary revision/configuration updates.
3. Finish legacy backup relay adoption and managed backup cutover.
4. Qualify actual user-systemd/rootless-container operation on disposable hosts.
   ARM64/physical LED qualification remains deferred/manual as agreed. Live
   provider qualification still requires explicitly chosen test accounts and
   destinations. No production-ready claim is made.

## Previous checkpoint: managed Signal gateway configuration adoption

Continued on 2026-11-03. Added `notification-worker adopt-signal --local --spec
ABS` for an existing recovered managed gateway verification installation. This
applies the endpoint's exact staged configuration through the existing managed
stop/publish/reload/restart transaction. The Signal verifier stays a separate
process and must already be running; this turn does not install or supervise its
service. No real account/provider, real user-systemd unit, remote host or LED was
used. Preserve the dirty working tree; no commit was created. Plans remain partial.

Implemented:

- Managed requests accept an adoption-only `signal_verification` root/hash
  reference. The new manifest retains that reference and its immutable adoption
  operation. Only `adopt-signal` can introduce it; ordinary updates cannot change
  configuration or replace/drop the binding. Adoption requires an existing
  recovered verification service and the exact original runtime bytes retained
  by endpoint preparation. It changes only the frozen Signal socket/inbox profile;
  worker configuration, provider profiles and canonical database identity remain
  unchanged. Account/inbox/source paths stay outside gateway installation authority.
- The installer reads endpoint publication while the separate endpoint holds its
  locks, validating root/lease inodes, intent/completion and staged bytes. Its
  independent retained `operations/<id>/signal-endpoint.json` copy, original
  adoption intent and external publication are required by later operations and
  runtime startup. Exact old/new configuration comparison prevents unrelated edits.
- Both new executable and replacement recovery helper must advertise
  `__managed-signal-verification-probe` support before stopping the gateway.
  Account recovery inputs/targets and original relay unit/configuration remain
  untouched, preserving their audit/retry semantics. The gateway unit gains no
  account mounts or writable account/inbox paths.
- Endpoint liveness and all health pins are checked before adoption and again
  around publication/start/completed resume. The client uses bounded, same-UID,
  private Unix sockets, rejects redirects and oversized/malformed responses, and
  compares manifest/installation/commit/recovery/account/socket/inbox identities.
  A 503 busy reply receives bounded retries; runtime/intake authority is refused.
- Managed runtime verifies the endpoint before exposing its API, publishes the
  process-local `signal_verification_hash` and `signal-verification` loop health,
  then periodically rechecks retained evidence and the independent endpoint.
  Losing/mismatching the endpoint terminates the gateway runtime and joins its
  services. A native two-process test exercises startup, restart and endpoint loss.
- Updates and compatible binary rollback retain binding/helper capabilities;
  rollback cannot cross the Signal configuration change. Completed uninstall
  retains evidence and does not require endpoint liveness, but missing immutable
  evidence still fails closed. Pending adoption must be resumed before uninstall.
  Uninstall never stops/removes the separately owned verifier or account records.
- An existing Telegram renewal may cross only this proven Signal-only change.
  Its immutable receipt is neither rewritten nor re-dated. Combined-provider tests
  delete the credential during adoption/update, verify zero new provider calls,
  then explicitly renew Telegram again and confirm the Signal binding survives.
- CLI dry-run validates local input/publication without manager, endpoint,
  executable or recovery-database activity. Added managed-signal-adoption.json,
  expanded SIGNAL_VERIFICATION.md, updated README/all plans and included the
  native Signal gateway test in the isolated integration script.

Validation:

- New lifecycle/interruption/drift tests passed (18.008s):
  /tmp/deploy-managed-signal-lifecycle.log. Nine injected interruption points
  complement five real SIGKILL boundaries below.
- Real SIGKILL at prepared, unit-published, manifest-published, start-returned and
  complete passed (7.320s): /tmp/deploy-managed-signal-crash.log. Resume uses staged
  artifacts after deleting the original input executable; completed retry does
  not stop/start again. Service manager state is a persisted test fixture.
- Two separate native executables passed (5.005s):
  /tmp/deploy-managed-signal-native.log. The installer uses a fake manager with
  real gateway processes and real capability/health checks; the separate verifier
  is a real CLI process. Endpoint loss causes gateway termination.
- Bounded Unix health contracts passed under race (23.968s):
  /tmp/deploy-managed-signal-health-race.log. Includes wrong identities, wrong
  mode/intake, redirection, missing endpoint and busy-then-ready behavior.
- Combined Signal/Telegram adoption, credential-free update and later Telegram
  renewal passed (3.667s, direct test output).
- CLI dry-run and rejection of unmanaged worker health binding passed:
  /tmp/deploy-managed-signal-cli.log (installer 1.425s, worker 0.021s).
- Expanded actual compatible binary rollback and offline-endpoint uninstall
  passed (2.043s): /tmp/deploy-managed-signal-rollback.log.
- Full repository suite passed: /tmp/deploy-managed-signal-all.log (worker
  164.997s, installer 149.732s, backup 34.967s; remaining packages passed/cached).
  The final expanded rollback assertion above ran separately after this suite
  had started; it changes tests only, not production code.
- `go vet ./...` and Linux ARM64 cross-build passed:
  /tmp/deploy-managed-signal-vet.log and /tmp/deploy-managed-signal-arm64.log;
  executable /tmp/deploy-managed-signal-arm64. No native ARM64 qualification
  is claimed. Formatting, shell syntax, example JSON and diff checks passed.
- Extended lifecycle/combined-provider/SIGKILL race validation passed (287.729s):
  /tmp/deploy-managed-signal-race.log. Includes adoption authority drift, retained
  Telegram evidence and all five real crash/resume boundaries. The separately
  expanded compatible rollback/offline-uninstall assertion passed as noted above.
- All test runs from this turn have finished. No verifier/gateway test process
  or test-owned service is intentionally left running.

Next work:

1. Implement managed supervision of the separate verification endpoint, retaining
   the original native/Podman relay service binding. This must start the account-
   owning verifier before the adopted gateway, validate the exact installed
   executable/unit, and resume partial service transitions. The present gateway
   adoption intentionally requires an independently running foreground endpoint.
2. Compose production provider/command admission and explicit activation gates;
   migrate outbound Signal profile pins through their own proven transition.
   Verification health is not delivery/intake/command authority. Legacy backup
   relay adoption and managed backup cutover are still unfinished.
3. Qualify real user-systemd/container supervision and later recovery epochs.
   Native ARM64 and physical LED checks remain deferred/manual as agreed. Live
   provider qualification still needs explicit test accounts/destinations.

## Previous checkpoint: durable Signal verification endpoint

Continued on 2026-11-03. Added `signal-verification-prepare --local --spec ABS`
and `signal-verification-serve --local --root ABS --manifest-hash HASH`.
This completes a separately enrolled foreground verification endpoint after the
native/Podman account handoff. It does **not** yet apply the staged configuration
to the managed gateway or install/change its service. The overall plans remain
incomplete. No real provider, account, systemd unit, remote host or physical LED
was used. Preserve the existing dirty working tree; no commit was created.

Implemented:

- `signalaccount.OpenRecoveryVerification` accepts only exact completed fenced
  recovery pins. It retains the existing shared account-operation and exclusive
  account lease, checks linking/maintenance/recovery history, and never creates
  missing coordination files. Directory, account/operation/control lock identity
  and lease metadata are rechecked; concurrent check/close is synchronized.
- `relay.OpenSignalRuntimeVerification` holds both source/destination inbox locks
  for its lifetime. It validates the retained source, runtime image, destination
  schema/baseline and independent completion records without opening the writable
  runtime. Source/restored-stage and database/image inode substitutions are
  rejected during verification. Its narrow API has no receive/list/ACK operation.
- Offline artifact reads now use SQLite immutable mode when all sidecars are
  absent; existing sidecars retain ordinary read-only handling in older callers.
  The new verifier rejects sidecars entirely. An inotify test observes no inbox
  file creates/modifications/deletions/moves during opening/checking/closing.
- Endpoint preparation checks original gateway JSON bytes, the exact publication
  cursor, account recovery target/result and disjoint paths before writing its
  root lease. Immutable intent, an independent destination binding, staged
  `gateway-next.json` and independent completion make the four publication steps
  resumable. Completed retries do not reconstruct lost intent/configuration/lease/
  destination receipt. A lost root intent cannot enroll a replacement endpoint.
- Staging changes only `interactive_signal.socket` and inbox identity in the raw
  configuration. It preserves other profiles and unresolved route labels; normal
  decoding resolves revisions exactly once. The original gateway JSON and live
  gateway database remain unchanged. This is verification configuration, not a
  production outbound-profile migration.
- The foreground server retains endpoint/account/source/destination authority,
  but releases the gateway lease so a later managed gateway verifier can coexist.
  Its owned 0600 Unix socket admits only same-UID peers and bodyless GET /health.
  Health includes exact manifest, installation, recovery, publication and Signal
  profile pins; intake is false. Other requests are denied. Requests and periodic
  audits revalidate evidence; authority loss terminates serving. Resource limits,
  request deadlines and joined handlers bound shutdown. Socket creation uses the
  retained directory and no-follow chmod; cleanup only removes its own inode.
- Added CLI dry-run checks, fixture/configuration/replay/loss tests, four real
  SIGKILL publication boundaries and a native executable restart test with a
  genuinely stale socket after SIGKILL. No signal-cli invocation occurs.
- Added examples/notification-worker/SIGNAL_VERIFICATION.md and private-request
  template signal-verification.json. README, all three plans and the isolated
  integration script now include the foreground endpoint and its limitations.

Validation in this turn:

- Focused lease/inbox race tests passed (relay 3.297s, account 1.129s):
  /tmp/deploy-signal-verification-leases.log. Includes the final staged-source,
  control-lock, strict receipt and no-write refinements.
- Endpoint preparation/server race tests passed (239.337s):
  /tmp/deploy-signal-verification-race.log. Initial sandbox socket tests were
  denied by setsockopt restrictions; the successful run used authorized local
  socket access outside the sandbox.
- All four publication SIGKILL boundaries passed under race (104.436s):
  /tmp/deploy-signal-verification-crash.log.
- Built executable preparation, SIGKILL, stale-socket restart and graceful stop
  passed (2.671s): /tmp/deploy-signal-verification-native.log.
- Final publication/foreground/native race checks passed (62.587s):
  /tmp/deploy-signal-verification-final-race-native.log. These include the final
  no-follow socket chmod, strict receipt/source/control identity checks and
  validation-before-root-lock ordering refinements.
- Full repository suite passed: /tmp/deploy-signal-verification-all.log
  (deployment 170.789s, worker 162.890s, installer 115.204s, backup 34.500s,
  relay 6.678s, account 1.518s; all other packages passed). This run started
  before the final no-follow chmod refinement, which the final native/race
  tests above cover. LED tests used the local simulator fixture, not hardware.
- Final `go vet ./...` and Linux ARM64 cross-build passed:
  /tmp/deploy-signal-verification-final-vet.log and
  /tmp/deploy-signal-verification-final-arm64.log; executable
  /tmp/deploy-signal-verification-arm64. This is not native ARM64 qualification.
  Formatting, shell syntax, example JSON parsing and diff checks passed.
- No test process or service from this turn remains running. The full-suite
  and native-test artifacts under /tmp are disposable validation evidence.

Next work, in order:

1. Managed endpoint/unit/configuration adoption. Preserve the original service
   adapter binding and account recovery target so native/Podman handoff retries
   still verify their original unit/configuration; do not rewrite those proofs.
   Adopt the staged verification configuration through an explicit managed
   transition, with independent endpoint health pins and restart/crash evidence.
   The ordinary gateway updater still rejects arbitrary configuration changes.
2. Compose that service transition with provider/command admission and explicit
   production activation gates. A healthy foreground verifier grants no intake,
   delivery, command execution or backup cutover authority. Existing outbound
   Signal profile socket/relay pins are intentionally not migrated by staging.
3. Legacy backup-relay adoption/cutover, including the existing Quadlet Volume
   renderer qualification noted in the previous checkpoint. Later recovery epochs
   and outstanding qualification requirements in all plans remain applicable.
4. Live provider/account qualification requires chosen test accounts/destinations.
   ARM64 native and physical LED checks remain deferred/manual as agreed. A
   successful ARM64 cross-build does not satisfy those environment-dependent gates.

## Previous checkpoint: rootless Podman Signal recovery handoff

Continued on 2026-11-03. Added the concrete interactive Quadlet backend and
`notification-worker recovery-signal-podman-handoff --local --spec ABS`. It uses
real container/image/mount/process proofs with the existing account fence and
published gateway/inbox coordinator. The overall plans remain incomplete. No
live Signal account, provider, real user-systemd unit, remote host or LED was
used. No commit was created; preserve the working tree.

- The private request pins source/generated Quadlet hashes, local image config
  ID, original container ID, deploy executable SHA-256, relay/gateway JSON,
  operation/publication and all account/inbox/socket/credential paths. The source
  must match the supported pure renderer. Credentials and Podman graph/run roots
  are part of the persistent binding. Dry-run reads local inputs only.
- The adapter requires local rootless cgroup-v2 Podman and a native Linux image.
  It checks loaded typed start/stop/cleanup commands, stable manager properties,
  exact container identity, creation command, executable arguments, isolation,
  capabilities, mounts/modes and conmon ownership. No remote client/pull, extra
  creation options, alias, drop-in, stale reload or pending job is accepted.
- A separate read-only process helper runs under `podman unshare`, executing the
  installer's already-open ELF descriptor. It checks the actual container PID's
  executable hash/inode, argv, owner/start identity, namespace-visible mount
  inodes, and held exclusive account/inbox flock descriptors. A same-byte file
  replacement at a bind source fails this proof. No exec enters the live container.
- `__signal-podman-recovery-probe` independently returns the image's recovery
  version and own executable hash in a disposable container with network=none,
  no account/configuration mounts, read-only rootfs, no added capabilities,
  bounded resources and ignored image volumes. It verifies the executable pin
  even when the original container has already been removed. Older images fail
  before durable fencing/stop. Operational errors omit command stderr/secrets.
- The existing coordinator fences the account before stopping the fixed unit,
  holds gateway/account-operation authority through installation, and verifies
  retained results independently. Exact retry handles a lost stop response and
  a removed original container. Renaming or replacing that container is refused.
  Completion does not restart a receiver or release admission/command fences.
- Added private request/Quadlet examples and SIGNAL_PODMAN_RECOVERY.md. README,
  all three plans and the isolated script describe the supported backend. The
  real-container test is opt-in with DEPLOY_PODMAN_FIXTURE=1; it owns a separate
  temporary VFS store and builds a scratch fixture without pulling an image.

Evidence and implementation refinements:

- Local Podman/Quadlet is 5.7.0, rootless, cgroup v2. Real generator tests exposed
  that Volume values must be unquoted for this profile and some independent
  options can appear in varying order. The validator accepts only reordering of
  the complete known option/value set; duplicates/extra options remain rejected.
  Five repeated generator checks passed: /tmp/deploy-signal-podman-generator.log.
- The actual user-namespace test showed a parent /proc/PID/exe link is unsuitable
  after unshare; the final implementation passes an open executable descriptor.
  Real inspect fixtures also verified image ID field naming, Args excluding argv0
  when a command is present, and protective nosuid/nodev mount options. Fixtures
  were corrected to match actual Podman output rather than weakening the pins.
- First complete real-container/built-CLI handoff passed (81.484s):
  /tmp/deploy-signal-podman-container.log. It uses synthetic flock-holding relay
  code and fake systemctl/busctl, with real rootless Podman in a private store.
  A deliberately lost stop response resumes to completion and a further retry
  performs no second stop. Account runtime remains disabled.
- Expanded Podman race/generator/real-container suite passed (97.159s; container
  scenario 95.72s): /tmp/deploy-signal-podman-final-container-race.log. This includes
  actual deploy capability/hash output and rejection of a replaced bind-source
  inode. Final targeted capability-hash rejection passed (0.008s):
  /tmp/deploy-signal-podman-capability-pin.log.
- Earlier focused native/Podman/coordinator race checks passed (20.978s):
  /tmp/deploy-signal-podman-race.log. Native built-CLI account-fence/handoff and
  private D-Bus regressions passed after shared parser/lease refactoring (35.423s):
  /tmp/deploy-signal-podman-native-regression.log.
- Full repository suite passed: /tmp/deploy-signal-podman-all.log (installer
  89.138s, backup 33.899s; worker and remaining packages passed/cached). This run
  preceded the final extra check of the container's retained creation arguments;
  the final focused tests cover that additional guard.
- Vet and Linux ARM64 cross-build passed: /tmp/deploy-signal-podman-vet.log,
  /tmp/deploy-signal-podman-arm64.log; binary /tmp/deploy-signal-podman-arm64.
  Cross-build is not native ARM64 qualification. Formatting, shell syntax and
  diff checks passed.

Final creation-command guard verification:

- Focused Podman authority/race tests passed (1.436s):
  /tmp/deploy-signal-podman-final-authority.log. This includes rejection of an
  extra environment option in the actual container creation command.
- The complete real-container/built-CLI test passed again under the race detector
  with that final guard (90.488s; scenario 89.46s):
  /tmp/deploy-signal-podman-final-authority-container.log. It was not skipped.
  Both the successful proof and rejected same-byte mount replacement, isolated
  image capability/hash probe, lost stop response, completed retry and single
  stop passed. The test's containers/image/store were cleaned up.
- Final vet and ARM64 cross-build passed after the guard:
  /tmp/deploy-signal-podman-final-vet.log,
  /tmp/deploy-signal-podman-final-arm64.log. All launched tests/builds have ended;
  no background test remains running.

Next: durable endpoint/configuration cutover and verification-server startup.
Then production Signal/Telegram admission, subsequent recovery epochs, legacy
backup-relay adoption/managed backup cutover, and the remaining migrations,
cleanup and qualification requirements. The current Podman profile supports the
interactive notification relay, not deploy-backup-signal.service. The legacy
backup renderer still needs real-generator qualification (including its quoted
Volume values) before claiming adoption support. Native ARM64, live-systemd/
provider and physical LED qualification remain outstanding/deferred as agreed.

## Previous checkpoint: native systemd Signal recovery handoff

Continued on 2026-11-03. Implemented a concrete native user-systemd adapter and
`notification-worker recovery-signal-handoff --local --spec ABS`. It connects the
previous account fence/coordinator to an existing native relay and the exact
published gateway/offline inbox installation. The overall three plans remain
incomplete. No actual user manager, live provider/account, remote host or physical
LED was contacted. No commit was created.

- The private version-1 request pins gateway JSON/publication, operation ID,
  disjoint account/gateway/source/destination directories, and native unit,
  executable and relay-config SHA-256. The fixed unit must belong to the current
  user's systemd configuration directory. Account/recipient fingerprints join
  the binding; state and writable socket directories cannot contain the inputs.
- Native service inspection requires a stable loaded unit with no pending job,
  alias, drop-in, stale daemon reload, wrapper or auxiliary Exec command. A narrow
  host profile rejects path remapping, environment overrides, user switches and
  systemd-managed directories that could remove account data. Stop must use
  control-group/SIGKILL and TimeoutStopSec=30s.
- Exact executable/argv/prefix flags are checked using the typed ExecStartEx
  D-Bus property through busctl JSON, not systemctl's display string. Repeated
  snapshots and file reads detect changes around inspection. Active-process
  checks verify the executable inode, argv, UID, start identity and the actual
  exclusive account AND source-inbox flock descriptors through /proc, including
  their namespace-visible inode. Matching argv alone is insufficient.
- The capability probe executes the already-open pinned ELF without account or
  configuration arguments. It runs before durable fencing/service stop. A
  separate probe version prevents accepting an older linking-only binary.
  Manager/probe errors do not print stderr or credential contents.
- The existing coordinator holds gateway/account-operation authority, publishes
  the account fence before stopping the fixed service, and installs/verifies the
  exact offline inbox. Interrupted/lost stop responses use the identical command
  and operation. A saved result is verified without reinstalling; a stopped
  service is not stopped again. Completion keeps runtime disabled. No receiver
  start, unit write/reload, endpoint switch or activation is implemented here.
- Dry-run reads/validates pinned input files only: no probes, manager calls or
  account/inbox/database opening. Added private request example and detailed
  native-profile/retry instructions in SIGNAL_MAINTENANCE.md. README, all three
  plans and the isolated integration script include the new slice.

Verification:

- Final native unit/contract tests passed (see
  `/tmp/deploy-signal-native-final-contract.log`): manager/property/argument drift,
  old probes before fencing, unsafe files/credentials, account flock proof and
  mutation-free dry-run. Tests never use a real service manager.
- Built-executable fixture passed initially (5.794s):
  `/tmp/deploy-signal-native-executable.log`. The real CLI stops a disposable native
  process holding a real flock through strict fake systemctl/busctl programs.
  Successful stop, lost response after stop, exact completed replay, single stop,
  credential drift and retained account fence are checked.
- Focused race suite including existing native account fence/inbox installation
  and gateway composition passed (69.622s): `/tmp/deploy-signal-native-race.log`.
  Final expanded native race tests passed after adding source-inbox descriptor
  verification (22.892s): `/tmp/deploy-signal-native-final-race.log`. They also
  reject same-byte replacement executable inodes, changed argv, and account/inbox
  configuration redirected after process start.
- A real busctl/private D-Bus integration passed without skipping (0.182s):
  `/tmp/deploy-signal-native-bus.log`. Both accepted plain commands and rejected
  execution flags use actual typed D-Bus values. The temporary bus was stopped;
  no call reached the user's bus. The isolated script includes this test; other
  environments need Python dbus/GLib plus dbus-run-session or it reports a skip.
- Full `go test ./...` passed: `/tmp/deploy-signal-native-all.log` (installer
  93.855s, worker 163.100s, backup 35.581s; remaining packages passed/cached).
  Socket/process fixtures ran outside the sandbox. This run began before the
  final source-inbox descriptor guard; that additional guard is covered by the
  final native contract/race tests above.
- Final `go vet ./...`, Linux ARM64 cross-build, formatting, shell syntax and
  diff checks passed. Logs: `/tmp/deploy-signal-native-final-vet.log`,
  `/tmp/deploy-signal-native-final-arm64.log`; binary
  `/tmp/deploy-signal-native-arm64`. Cross-build is not native ARM64 qualification.

Next: implement the rootless Podman Signal adapter with pinned image/container,
loaded Quadlet and actual account/inbox mount proofs. Then implement the durable
endpoint/configuration cutover and verification-server startup, followed by
production Signal/Telegram admission/activation, subsequent recovery epochs,
managed backup cutover and remaining migration/cleanup/qualification requirements.
The native handoff above neither adopts the backup Podman relay nor supplies a
production activation path. Native ARM64 and physical LED qualification remain
deferred as requested. Keep all earlier account/gateway/inbox evidence intact.

## Previous checkpoint: Signal account recovery fence and installer integration

Continued on 2026-11-03. Implemented the durable account/service coordination
required before Signal endpoint cutover. This is an internal lifecycle integration,
not yet a complete systemd/Podman service switch. No live provider, real manager,
remote host or LED was contacted; no commit was created.

- `signalaccount.FenceRecovery` pins an exact operation, service binding and target,
  directory/account-lock identities, and the existing link/maintenance history.
  Recovery-capability verification is mandatory before intent publication or stop;
  the older linking capability alone cannot authorize this transition.
- Immutable account intent/fence records precede service shutdown. The same
  service-operation lock used by linking maintenance spans the whole operation;
  short control locks cover file changes only. An exclusive account lease proves
  that cooperating CLI/JVM children have exited before the offline action begins.
- An independently verified result is saved before the completion record. Partial
  retries use the frozen request. A saved result is verified without applying the
  transition again; interrupted apply uses the downstream idempotent protocol.
  Missing or inconsistent records, changed targets, service activity/binding,
  directory/lease replacements and changed maintenance history stop progress.
- Ordinary account acquisition, Link, Resolve and MaintainLink honor the fence.
  Diagnostic status does not recreate a missing enrolled account lease. Completion
  keeps runtime disabled and never restarts a receiver; there is no release or
  second-recovery operation yet. All four deploy-recovery JSON records are retained.
- `InstallSignalRecoveryWithService` composes the fence with published gateway
  authority and `InstallSignalRuntime`. It holds the gateway lease through service
  work, installs the exact source/target cursor and independently verifies both
  source and destination on replay. Resolved runtime input is detached without
  re-hashing routes. `WithPublishedSignalCursorChecked` supplies mid-transition
  verification and also pins directory, worker-lock and canonical database inodes.
- `signal-recovery-status --local --state-dir ABS` is available for offline audit;
  `__signal-recovery-probe` advertises the separate capability. The built-executable
  test verifies that native legacy/interactive relay startup and linking refuse
  the gate before invoking the fake signal-cli or creating a socket.
- Added six SIGKILL boundaries, an actual inherited child-process lease test,
  service/capability/state drift, lost evidence and gateway/inbox composition tests.
  README, Signal maintenance guide, all three plans and the isolated test script
  reflect the new behavior and the unfinished service adapter.

Verification:

- Signal account tests passed normally (1.181s) and the full account package passed
  under the race detector after the final history/lease checks (9.028s):
  `/tmp/deploy-signal-fence-account.log`,
  `/tmp/deploy-signal-fence-account-race.log`. Includes all six real SIGKILL cases
  and a subprocess retaining the inherited account descriptor after service stop.
- Installer/gateway/inbox contracts passed after the final identity refinement
  (9.504s): `/tmp/deploy-signal-fence-final-contract.log`.
- The focused recovery race suite passed (account 1.277s, installer 95.719s):
  `/tmp/deploy-signal-fence-race.log`. Additional race coverage for the final
  directory/worker-lock/database replacement cases passed (31.794s):
  `/tmp/deploy-signal-fence-identity-race.log`. No timeout in this checkpoint.
- Final published Signal runtime regressions passed (4.321s):
  `/tmp/deploy-signal-fence-publication.log`.
- Built-executable account-fence and inbox-installation tests passed (3.566s):
  `/tmp/deploy-signal-fence-final-native.log`. Synthetic signal-cli remains
  uninvoked; neither legacy nor interactive startup creates its socket.
- Full repository suite passed: `/tmp/deploy-signal-fence-all.log` (worker
  167.750s, installer 97.228s, account 1.443s, backup 35.980s; others passed/cached).
  Socket/process fixtures ran outside the sandbox. The final gateway inode guard
  was added after this run launched and is covered by the final contract,
  publication, native and additional race checks above.
- Vet and Linux ARM64 cross-build passed:
  `/tmp/deploy-signal-fence-vet.log`, `/tmp/deploy-signal-fence-arm64.log`; binary
  `/tmp/deploy-signal-fence-arm64`. Cross-building is not native ARM64 qualification.
  Formatting, shell syntax and diff checks pass. All launched jobs have ended;
  no background test remains running.

Next: implement the concrete managed Signal service adapter with pinned unit,
image/executable and account mount checks; use the coordinator for the actual
endpoint/configuration cutover and verification-service startup. Then implement
production Signal/Telegram handoff/activation, repeated recovery epochs, managed
backup cutover and the remaining migration/cleanup/qualification items. Native
ARM64 and physical LED qualification remain deferred as requested. Do not mark
these plans or production deployment complete.

## Previous checkpoint: managed Telegram renewal and helper upgrade

Continued on 2026-11-03. Connected Telegram renewal to the managed verification
lifecycle. The overall plans remain incomplete. No live provider, remote host,
actual systemd service or physical LED was contacted. Tests use fake HTTP
transports/service managers and disposable native worker processes. No commit.

- `renew-telegram --local --root ABS` requires an adopted recovery verification
  installation with interactive Telegram configuration. Runtime and helper must
  advertise renewal support. The operation stops the exact local unit and holds
  the operation, installation and worker leases during provider work. The user
  must still stop any external bot consumers; local service inspection cannot
  prove global provider-account exclusivity.
- An immutable `operations/ID/telegram-request.json` freezes the predecessor before
  any polling. Service binding/activity, unit bytes, runtime, lifecycle pointer,
  root and lease identities are checked around provider preflight and polling.
  Authority loss prevents another positive ACK. Existing worker page/checkpoint
  ordering, zero-offset retry and archive limits remain unchanged.
- `operations/ID/telegram-receipt.json` independently retains completion. Resume
  after worker completion does not poll or require credentials, including when
  the managed receipt has not yet been written. Missing request evidence after
  worker progress and missing receipt evidence after publication fail closed.
- Verification startup fully audits the latest proof while holding its lifetime
  worker lease, and health includes `telegram_renewal_id`. The manifest carries
  that pin through updates, compatible binary rollbacks and uninstall. A stale
  observation is sufficient only for offline audit/verification startup; no
  freshness is renewed and no production cutoff or intake permission is installed.
- `upgrade-helper --local --spec ABS` fixes the existing-installation upgrade path:
  ordinary updates retain the old helper, which cannot read new renewal metadata.
  This explicit transition stages and capability-probes both replacements while
  preserving schema/configuration/identity/recovery pins and all old artifacts.
  It uses the durable lifecycle, needs the new helper for crash resume, and creates
  a helper boundary that binary rollback cannot cross. Interrupted helper upgrade
  and renewal must finish before uninstall.
- Added worker audit/lease tests, managed lifecycle/drift/successor tests, eight
  renewal SIGKILL boundaries, seven helper-upgrade SIGKILL boundaries, provider
  page tests proving service drift prevents a positive ACK, and a built-executable
  integration covering successful restart and credential-free completed resume.
  The isolated test script includes that new native test. README, installation
  guide and all three plans describe usage and remaining scope.

Verification:

- Focused audit/lifecycle contracts passed: worker 1.520s, installer 37.676s;
  `/tmp/deploy-managed-telegram-contract.log`.
- Native executable integration passed (11.947s):
  `/tmp/deploy-managed-telegram-native.log`. Includes real executable/helper
  capability probes, helper upgrade, worker startup/Unix health, stale-pin
  rejection, completed resume while the runtime owns its lease, and restart after
  a completed drain with the fake credential removed. No real manager/provider.
- Full repository suite passed: `/tmp/deploy-managed-telegram-all.log` (worker
  172.527s, installer 90.545s, backup 36.528s; others passed/cached). This includes
  the eight renewal and seven helper-upgrade SIGKILL cases and page/ACK drift test.
  Local socket/process fixtures ran outside the sandbox.
- The final test strengthening exercises rollback to a different binary
  and unsupported helper-upgrade rejection before intent/service changes. Both
  passed under race detection (82.487s):
  `/tmp/deploy-managed-telegram-upgrade-race.log`. These test-only additions were
  made after the full suite launched; production code was unchanged.
- Vet and Linux ARM64 cross-build passed:
  `/tmp/deploy-managed-telegram-vet.log`, `/tmp/deploy-managed-telegram-arm64.log`;
  executable `/tmp/deploy-managed-telegram-arm64`. This is not native ARM64
  qualification. Formatting, shell syntax and diff checks pass.
- The combined race job hit Go's default 600-second package timeout during
  `TestManagedTelegramRenewalRejectsAuthorityDrift/request-loss`, with no reported
  data race or assertion failure. Worker audit/lease passed (30.107s); preceding
  installer crash/page/lifecycle/interruption blocks had returned before timeout.
  The job as a whole did **not** pass: `/tmp/deploy-managed-telegram-race.log`.
  The remaining drift/dry-run/successor block passed separately (143.724s) with
  verbose output: `/tmp/deploy-managed-telegram-remaining-race.log`. All seven
  authority-drift cases passed, including missing request/receipt and unsupported
  capabilities. The final helper/actual binary rollback tests have their clean
  race result above. The isolated full notification race script now explicitly
  allows 45 minutes for the growing SQLite proof/crash suite; that entire long
  script was not rerun this turn. All launched jobs have terminated; no background
  test remains running.

Next: managed Signal account/receiver shutdown, endpoint/configuration cutover,
Signal verification startup; then production Telegram cutoff installation and
activation with fresh provider/service/clock checks. Repeated recovery epochs,
managed backup cutover and the earlier remaining migration/cleanup/qualification
items are still open. Native ARM64 and physical LED qualification remain deferred
as requested. Do not claim the plans or production deployment are complete.

## Previous checkpoint: durable Telegram handoff renewal

Continued on 2026-11-03. Implemented the missing renewed Telegram drain as a
separate, bounded archive tied to the exact published gateway, original cursor
and completed predecessor. The overall plans remain incomplete. No live provider,
remote host, real systemd service or physical LED was contacted; tests use injected
HTTP transports/fake providers. No commit was created.

- `RenewPublishedTelegramHandoff` is an internal lifecycle API. The caller must
  stop all other account receivers before invoking it; this implementation holds
  the gateway lease throughout but cannot prove external receiver shutdown.
  A stable request ID plus predecessor hash makes retries exact. Completed retry
  returns the same observation without provider preflight, polling or freshness
  renewal. A new generation requires a new ID and the exact completed predecessor.
- Immutable sanitized pages live under
  `recovery/OPERATION/telegram-renewals/ACCOUNT_HASH/REQUEST_ID/`; the independent
  `telegram-renewal-ACCOUNT_HASH.json` root index is the acknowledgement checkpoint.
  Each page and checkpoint are synced before the next positive provider offset.
  Every new invocation starts polling at zero. A page committed one step ahead of
  the checkpoint can be recovered; missing pages, larger disagreement, altered
  completion, missing index with retained history and unexpected files fail closed.
- Source publication, canonical SQLite bytes, request inventory, old operational
  zero offset and original command identities remain unchanged. Retained mappings
  classify known replay; new inputs remain archive evidence only and cannot mint
  commands, confirmations or grants. Unauthorized new text is discarded. Reused
  native IDs with different evidence fail before another acknowledgement.
- Each generation retains and verifies every predecessor. Per-account limits are
  16 generations, 4,096 aggregate pages and page bytes bounded by the smaller of the
  configured database limit and 64 MiB. Resource exhaustion leaves intake fenced.
- Shared publication sessions check live SQL, root identity/quarantine, directory
  and lock identity, and the canonical database inode around provider work. No
  SQLite transaction spans network I/O. Loss of those authorities stops the next
  provider ACK. Historical publication proof is independently reverified.
- `VerifyRenewedTelegramHandoff` and `recovery-telegram-check --renewal-hash HEX`
  validate the latest completion and its complete archive before checking boot,
  wall/monotonic time, expiry and cutoff. Once renewal starts, original or older
  proof inspection is refused, including during an interrupted successor. Results
  retain intake_enabled false and requires_activation_recheck true. No polling
  renewal CLI, receiver startup or production cutoff installation is introduced.
- Implementation is separated into publication-session, coordinator, store,
  history, journal and event-classification modules. README and all three plans
  describe the contract and remaining managed activation work. The isolated native
  test script includes inspection of renewed handoffs.

Verification:

- Existing published handoff tests passed after the session refactor (7.838s).
- Combined worker/CLI regression passed (worker 20.524s, installer 10.963s):
  `/tmp/deploy-telegram-renewal-combined.log`.
- Extended renewal contracts passed (24.958s):
  `/tmp/deploy-telegram-renewal-contract.log`. Includes eight real SIGKILL cases,
  including response loss after provider-side ACK; durable ACK ordering;
  checkpoint damage; native ID conflicts; clock regressions; predecessor retention;
  incomplete successors; lost leases/recovery gates and replaced canonical DBs.
- Full repository suite passed: `/tmp/deploy-telegram-renewal-all.log`
  (worker 162.230s, installer 44.897s, backup 35.739s; others passed/cached).
  Local socket/process fixtures ran outside the sandbox; actual services were not
  modified. The final temporary-file and early budget refinements below followed
  this run's launch and have their own regression coverage.
- Native original and renewed handoff CLI checks passed in 11.037s:
  `/tmp/deploy-telegram-renewal-native.log`. Fake provider credentials were removed
  before invoking the built executable; no real network transport was used.
- Final renewal contracts passed after temporary-file/budget refinements (27.463s):
  `/tmp/deploy-telegram-renewal-final-contract.log`. Incomplete private atomic-JSON
  temporaries are tolerated without becoming ACK evidence; unsafe modes fail.
- Additional race test for zero-length/partial private temporaries, unsafe modes
  and refusing archive pages before exceeding the reader byte budget passed
  (17.363s): `/tmp/deploy-telegram-renewal-extra-race.log`.
- Final vet and ARM64 cross-build passed:
  `/tmp/deploy-telegram-renewal-final-vet.log`,
  `/tmp/deploy-telegram-renewal-final-arm64.log`; binary
  `/tmp/deploy-telegram-renewal-arm64`. This is not native ARM64 qualification.
  Formatting, shell syntax and diff checks pass.
- The larger targeted race suite passed, including all eight SIGKILL cases and
  the original/published/renewed handoff and CLI contracts:
  `/tmp/deploy-telegram-renewal-race.log`. The separate final temporary/budget test
  above covers the refinements made after this long run started. All tests launched
  for this checkpoint completed successfully; no background test remains running.

Next: integrate managed Signal/Telegram receiver shutdown and startup, Signal
endpoint/configuration cutover, and installation of the renewed Telegram cutoff
into production admission under the same lifecycle authority; then production
activation and immutable ancestry/repeated gateway recovery. Managed backup
cutover and remaining migration/cleanup/qualification work are still open.
Native ARM64 and physical LED checks remain user-deferred.

## Previous checkpoint: installed Signal runtime and published Telegram handoff check

Continued on 2026-11-03. Signal recovery now installs a baseline-aware inbox in a
separate private destination, tied by the CLI to the exact completed gateway
publication. The source and destination remain fenced for ordinary receiver
startup. This completes another local recovery integration; the three overall
plans remain incomplete. No live provider, real user service, remote host or
physical LED was contacted, and no commit was created.

- Added `relay.InstallSignalRuntime`, `VerifyInstalledSignalRuntime` and
  `OpenInstalledInboxVerification`, split across transaction, filesystem, source,
  image and verification modules. Both surviving and snapshot-restored sources
  are supported, including preserved earlier baseline history. The current source
  database, historical ACK/listed counters and all recovery artifacts are retained.
- The source intent binds an exact cursor and one destination path/device/inode.
  Source and destination leases span installation. A source cannot be silently
  duplicated to another destination or resumed against a replaced directory.
  Missing locks, unsafe files/sidecars, changed source checkpoints and unowned
  destination artifacts are refused. The original source's normal runtime and
  restored recovery mutators become fenced once installation starts.
- The installer copies the schema-4 cursor candidate, transactionally promotes an
  owned copy to schema 5, verifies baseline/event history/reserved positions and
  zero runtime ACK/listed counters, then seals the immutable runtime image. It
  copies that image to inbox.db, installs matching file/SQL identity and baseline,
  and writes independent destination/source completions. No process is started.
- Exact retry resumes partial copies and transactions, including SIGKILL with an
  owned temporary rollback journal. Sealed images are never rebuilt. Changed
  seals, intent/lease loss and inconsistent completions fail closed. Once a
  verification runtime has opened the canonical database, losing a source
  completion cannot be repaired by pretending the modified file is the image.
- The verification opener validates retained source evidence, immutable image,
  directory identity, both completion records and live SQL baseline/history.
  It refuses CheckActive, CanReceive, Enqueue, List and Ack; ordinary OpenInbox
  remains blocked at both paths. This is not a production receiver opener.
- Added `worker.WithPublishedSignalCursor`: it verifies the full gateway
  publication, reads the cursor from its retained immutable image, compares the
  live cursor/request seal, and retains the worker lease across the relay action.
  No SQLite transaction spans that action. The same evidence is rechecked afterward.
- Added `notification-worker recovery-signal-install --local --config ABS
  --state-dir ABS --commit-hash HEX --relay-source ABS --relay-destination ABS`.
  It derives the cursor from the publication, checks the configured Signal account
  and original inbox identity, rejects overlapping paths, and returns the exact
  installed proof with intake_enabled false. Dry-run opens no state or provider.
  The CLI does not change gateway/relay service configuration or endpoints.
- Closed a verification-mode bypass in the legacy RunTelegramIntake loop: it now
  rejects before provider preflight, matching the supervised PullTelegram path.
- Added `VerifyPublishedTelegramHandoff` and the local `recovery-telegram-check`
  command. It verifies the exact completed gateway publication, compares retained
  and live cursor/request evidence, requires an explicit operational zero-offset
  row, then rechecks boot, wall/monotonic freshness and the five-second cutoff
  under the worker lease. It neither polls nor reads bot credentials, changes
  cursors or renews the original observation. Its point-in-time result explicitly
  requires another activation check and keeps intake_enabled false. Expired
  observations still require a separately journaled new drain; renewal is open.
- README and all three plans record both commands and their activation
  limitations. The native regression script includes both new CLI integrations.

Verification:

- Full repository suite passed: `/tmp/deploy-signal-runtime-all.log` (notification
  worker 124.550s, installer 33.983s, relay 6.884s, backup 35.818s; other packages
  passed or were cached). Local socket/process fixtures ran outside the sandbox
  with permission; no real systemd service was changed.
- Targeted race suite passed: `/tmp/deploy-signal-runtime-race.log` (relay 20.037s,
  worker 65.787s, installer 15.697s). Includes ten interruption/SIGKILL boundaries
  for each of surviving/restored sources, gateway lease composition and CLI retry.
- Additional race tests passed: `/tmp/deploy-signal-runtime-extra-race.log` (relay
  3.070s, worker 1.162s) for boundary tampering, lost completion after verification,
  prior-baseline preservation and the legacy Telegram verification guard.
- Final CLI race tests passed: `/tmp/deploy-signal-runtime-final-cli-race.log`
  (23.438s), including configured inbox identity drift before source fencing.
- Built-native CLI/managed recovery integration passed in 6.370s:
  `/tmp/deploy-signal-runtime-native.log`. The final native CLI rerun after the
  added identity check passed in 1.334s:
  `/tmp/deploy-signal-runtime-final-native.log`.
- Final vet, ARM64 cross-build, formatting, shell syntax and diff checks passed.
  Logs: `/tmp/deploy-signal-runtime-final-vet.log`,
  `/tmp/deploy-signal-runtime-final-arm64.log`; binary
  `/tmp/deploy-signal-runtime-arm64`. This is not native ARM64 qualification.
- The initial CLI test fixture used an unresolved route revision and correctly
  failed publication binding; it now uses ReadRuntime's normalized configuration.
  The final suites above pass. The stricter CLI identity check and legacy loop
  guard added after the full run were covered by the final targeted tests.

Additional verification after the published Telegram handoff implementation:

- Full repository suite passed again after all production code changes:
  `/tmp/deploy-recovery-final-all.log` (worker 130.188s, installer 38.405s,
  relay 6.657s, backup 35.092s; remaining packages passed/cached).
- Telegram handoff race suite passed: `/tmp/deploy-telegram-handoff-race.log`
  (worker 125.080s, installer 13.138s). Covers valid and expired observations,
  cutoff boundaries, wall/monotonic drift, reboot, unavailable clocks, cancellation,
  lease exclusion, missing/changed cursors, publication and retained-image damage.
  Reads leave historical/live database bytes unchanged and normal startup fenced.
- Final added regression cases also passed under race detection:
  `/tmp/deploy-telegram-handoff-final-race.log`; inspection remains valid after
  managed verification adoption/runtime use, while a valid but rehashed/changed
  live cursor is refused against the immutable original. These test additions
  followed the full-suite launch; production code was unchanged.
- CLI tests passed in 5.248s; the built native command passed in 5.255s:
  `/tmp/deploy-telegram-handoff-cli.log`, `/tmp/deploy-telegram-handoff-native.log`.
  Fixtures use an injected HTTP transport, delete the fake credential before the
  check, and demonstrate repeat inspection without provider access or renewal.
- Vet and ARM64 cross-build passed again: `/tmp/deploy-recovery-final-vet.log`,
  `/tmp/deploy-recovery-final-arm64.log`, binary `/tmp/deploy-recovery-final-arm64`.
  Formatting, shell syntax and diff checks pass. This is still not live-provider,
  native ARM64, actual systemd-service or physical-LED qualification.

Next: integrate managed Signal account/service shutdown, receiver startup and
endpoint/configuration cutover with the exact installed proof and gateway epoch;
implement separately journaled Telegram handoff renewal and bind the current
checks to managed activation; then complete
production activation and immutable ancestry/repeated recovery. Managed backup
cutover and the remaining migration/cleanup/qualification work from earlier
checkpoints remain open. Native ARM64 and physical LED checks are user-deferred.

## Previous checkpoint: managed recovery verification runtime

Continued on 2026-11-03. Completed publication is now connected to the managed
verification service, including restart, update and uninstall. The overall three
plans remain incomplete: this does not enable production or repeated recovery.
No live provider, remote host, actual user service or physical LED was contacted.
The dirty working tree is preserved; no commit was created.

- Added `ReviewPublishedRecovery`, `AdoptPublishedRecoveryVerification` and
  `OpenManagedRecoveryVerification`. Exact publication commit, original/new
  identities, configuration, independent publication records, immutable image,
  full retained checkpoint proof and retained original bytes bind adoption.
- `recovery-verification.json` intent and independent completion admit verification
  mode only. No canonical SQL writes occur before that completion: retry requires
  the exact installed image. Once admitted, normal verification-runtime writes
  can occur; restarts validate both root records plus the live SQL publication
  anchor, recovery binding, identity and current schema. A lost completion after
  runtime writes cannot be recreated from the modified database. Unsafe SQLite
  sidecars, changed root phases/candidate pins and lost lock authority are refused.
- Normal Store.Open/serve remain blocked by quarantine. The special managed opener
  sets verification mode before exposing any API. Provider intake, command grants,
  activation and replies remain unavailable; health cannot report healthy/green.
  Existing retained Signal/Telegram cursor and inherited LED fixtures now compose
  publication, adoption, reopen and status without provider/daemon calls.
- `notification-worker adopt-recovery --local --spec ABS` is an explicit managed
  identity transition. Its spec requires `recovery_commit_hash` from completed
  publication; setup/update refuse that input. Preflight checks the old identity
  before publishing an installation intent. Resume stops the pinned service,
  holds the exclusive installation lease, rechecks the exact transition and
  adopts the published state. Candidate and promoted helper must advertise the
  dedicated recovery-verification capability. Runtime health binds identity,
  installation revision and recovery commit; the installation lease is released
  before starting the process, as in the existing lifecycle.
- Verification update/uninstall retain the commit automatically. Rollback cannot
  cross the recovery identity/helper transition. Interrupted adoption must resume
  before uninstall; schema/configuration migration and repeated recovery of an
  adopted state remain explicitly unsupported until ancestor handling exists.
- Tests cover two worker persistence boundaries and thirteen managed lifecycle
  interruption/SIGKILL boundaries, including resume after original installer
  inputs disappear. Negative cases cover wrong pins, incapable candidate/helper,
  lost or changed root/SQL evidence, lost completion after runtime writes,
  sidecars and CLI dry-run/flag isolation. The built executable test runs an actual
  worker process on temporary Unix sockets, checks new/old epoch clients, resumes
  while the process owns its lease, updates it and uninstalls it. The service
  manager is simulated; this is not live systemd qualification.
- README documents the complete adoption/resume/update flow. All three plans
  record this integration without marking production activation complete.

Verification:

- Full repository tests passed (`/tmp/deploy-recovery-verification-all.log`):
  deployment 169.218s, notification worker 125.636s, installer 33.487s,
  backup 36.308s and all remaining packages. Local socket/process fixtures
  ran outside the sandbox with permission.
- Recovery/adoption race tests passed: worker 158.550s, installer 395.073s;
  `/tmp/deploy-recovery-verification-race.log`. Includes both worker and all
  thirteen installer SIGKILL boundaries, exact resume and lost-input recovery.
- Final targeted race tests passed: worker 141.828s, installer 1.114s,
  `/tmp/deploy-recovery-verification-final-targeted.log`. These include the
  final stricter root phase/candidate/source binding, lost completion, altered
  evidence, unsafe sidecars, disabled grants/intake and CLI dry-run checks.
- Built-native managed lifecycle/recovery/schema/LED-migration/publication
  regression suite passed in 13.820s,
  `/tmp/deploy-recovery-verification-final-native.log`. It uses the pinned
  `/home/sal/code/pi-status-led` offline fixture, not the Raspberry Pi.
- `go vet ./...` and Linux ARM64 cross-build passed on the final source:
  `/tmp/deploy-recovery-verification-final-vet.log`,
  `/tmp/deploy-recovery-verification-final-arm64.log`;
  binary `/tmp/deploy-recovery-verification-arm64`.
- Formatting, shell syntax and `git diff --check` passed. Initial native test
  wiring errors (client timeout/identity fields and omitted LED fixture variable)
  were fixed; final native runs above pass. No hardware or live provider
  qualification is claimed.

Next: implement production service/provider handoff validation and activation,
then immutable ancestor references and snapshot/recovery support for later epochs.
The broader remaining list in the preceding checkpoint still applies (managed
backup cutover, Signal installation, worker-wide LED cleanup, remaining migration/
compaction/decommission work and qualification). Native ARM64 and physical LED
checks remain user-deferred; a cross-build is not hardware qualification.

## Previous checkpoint: canonical recovery database publication

Continued on 2026-11-03 at the user's request to finish implementation. The actual
canonical database exchange is now implemented. The overall plans are still not
complete: managed activation and repeated recovery remain substantial open work.
No remote host, live provider, system service or physical LED was contacted. No
commit has been made; preserve the existing working tree.

- Added public `PublishRecovery` and `notification-worker recovery-publish --local`
  with exact operation/review/checkpoint/proof/image pins. It validates readiness,
  the current review and the independently retained image under one worker lease
  and all producer leases. It refuses stale/unready inputs before freezing state.
- Publication requires durable preparation complete. A producer explicitly fenced
  as absent may retain pending delivery/command reconciliation; its unfinished
  message decisions still block. Provider activation rechecks are deferred to
  activation. The no-LED exemption requires no configured bindings, no independent
  LED recovery evidence and empty LED history across restored/original/reconciled
  databases. Config absence alone cannot erase a retained LED fault.
- A root `recovery-publication.json` intent freezes further reconciliation.
  Only the exact publisher can reopen the frozen projection read-only. The
  canonical-source check accepts the pinned original or pinned installed image
  only within this publisher; normal readers/mutators cannot bypass the gate.
- The intent owns a temporary `recovery-publication-exchange.db`. Its exact bytes
  are copied from the retained image, synced and recorded in
  `recovery-publication-sealed.json`. Linux RENAME_EXCHANGE atomically swaps it
  with state.db, retaining the original inode/bytes at the exchange path. Both
  sides, review, historical proof and seals are checked again before completion.
  `recovery-publication-complete.json` independently acknowledges the transition.
- Resume identifies the pre/post-exchange state from both byte hashes rather than
  trusting a phase flag. Unsealed intent-owned copies may be rebuilt; damaged or
  missing sealed copies are refused. An intact exchanged pair can finish a lost
  completion acknowledgment. Existing inconsistent completions and rolled-back
  completed pairs fail closed. Exact retries return the same commit/result.
- CLI integration exposed harmless original SQLite sidecars left after a completed
  checkpoint. The publisher now removes empty WAL/journal files and the rebuildable
  SHM index only under the frozen intent and worker lease, after verifying original
  bytes and private files. Nonempty WAL/journal data and unsafe/oversized files are
  refused. This has targeted regression coverage and its own crash boundary.
- Normal startup is still refused, including if recovery.json is lost while the
  publication intent remains. Delivery, commands, provider intake and LED health
  are not enabled. Independent historical checkpoint/image verification continues
  to work after the canonical database changes.
- Tests exercise real installation/replay, original-byte preservation, stale and
  unready inputs, damaged evidence, seal/completion/lease mutations before exchange,
  sidecars, retained LED history, interruption and SIGKILL at all seven persistence
  boundaries. Backup composition publishes notification-only handoffs, refuses
  incomplete interactive intake and confirms source/command fences remain intact.
  Additional composition tests publish retained Signal/Telegram cursors and adopted
  LED history without contacting providers/daemon or enabling normal startup.
  CLI, dry-run and built executable tests cover publication and later image proof.

Verification:

- Final full repository tests passed: deployment 171.321s, notification worker
  119.160s, backup 38.608s and all remaining packages,
  `/tmp/deploy-publish-final-all.log`. Local socket/process fixtures ran outside
  the sandbox with permission.
- Publication race tests passed: worker 261.735s, installation 25.590s,
  `/tmp/deploy-publish-final-race.log`. Includes seven SIGKILL boundaries, exact
  resume/replay, source retention, stale pins, corrupted evidence and sidecars.
- Additional provider/LED/backup composition race tests passed: worker 31.323s,
  backup 17.944s, `/tmp/deploy-publish-composition-race.log`. These were added
  after the full run; production implementation did not change afterward.
- Built-native publication and historical image verification passed (1.948s),
  `/tmp/deploy-publish-final-native.log`.
- `go vet ./...` and Linux ARM64 cross-build passed:
  `/tmp/deploy-publish-final-vet.log`, `/tmp/deploy-publish-final-arm64.log`;
  binary `/tmp/deploy-publish-arm64`. This is not native ARM64 execution.
- Formatting, isolated-script shell syntax and `git diff --check` passed.
  Earlier CLI/native/full runs found the checkpointed sidecar case described
  above; final runs include the fix. No live provider, systemd host, remote
  machine or physical hardware qualification was performed.

Next: completed publication is not managed activation. Implement current
service/provider handoff validation, runtime acceptance of completed publication,
immutable ancestor references and snapshot/recovery support for later epochs
before removing quarantine gates. Worker-wide LED cleanup, Signal installation,
configuration/UID/socket transitions, managed backup cutover, remaining compaction/
decommission and live qualification also remain open. Native ARM64 and physical
LED execution are user-deferred. No percentage increase is asserted from this step.

## Previous checkpoint: independent prepared-image verification

Continued on 2026-11-03. Prepared publication images now have an independent,
read-only verifier that remains usable after canonical and mutable recovery state
changes. All three plans remain incomplete. No remote host, live provider,
system service or physical LED was contacted; no commit has been made.

- Added `worker.VerifyRecoveryPublication` and
  `notification-worker recovery-publication-verify --local --config … --state-dir …
  --operation … --review-hash … --checkpoint-hash … --proof-hash … --image-hash …`.
  Requires the original configuration, exact references and existing offline
  worker lease. Missing state or lock files are never initialized.
- Validates the complete retained checkpoint proof and its original/source/
  candidate/projection databases, archived records and ownership histories. Reads
  the frozen review, validates the image's historical SQL anchor, checks unchanged
  logical contents and exact bytes, and requires matching preparation intent,
  seal and completion. Sidecars, unsafe links/modes, partial preparations and
  altered permission claims are refused without repair or success output.
- Repeats retained observations under one worker lease, checking lock and directory
  identities before, between and after reads. Preparation and verification share
  the completed-image reader; records are checked before and after the image's
  SQL/byte inspection. Existing checkpoint verification shares the same directory
  and lease guard without changing its evidence or result format.
- Current state.db, identity/recovery files, ownership heads and reconciled.db are
  not inputs. Tests verify old images after a newer checkpoint, replacement of
  canonical records and removal of mutable stage files. Byte/mode tree fingerprints
  prove both successful and failed verification leave state unchanged.
- Tests cover wrong references/configuration, contention, missing state/lock,
  cancellation, damaged/missing artifacts and mutations during reading. Native
  executable coverage verifies the same output after deleting current files and
  refusal to repair a missing completion. Delivery, commands, Signal/Telegram,
  inherited LED and backup handoff composition exercise the new reader.
- The result is the original `RecoveryPublicationImage`, with all permission flags
  false. This proves completed preparation only, not a completed canonical swap,
  current provider/service freshness, ancestor-chain closure or activation.
  README, CLI help, all three plans and the isolated test script are updated.

Verification:

- Final full repository suite passed: deployment 165.682s, worker 100.208s,
  backup 40.126s and all other packages, `/tmp/deploy-image-proof-final-all.log`.
  This includes recovery composition and the existing preparation SIGKILL tests.
  Local socket/process fixtures ran outside the sandbox with permission.
- Final focused race tests passed: worker 182.528s, installation 8.275s,
  `/tmp/deploy-image-proof-final-race.log`. These include historical checkpoint
  regression tests, new image verification and preparation mutation checks.
- Both built-native publication preparation and verification tests passed
  (3.487s), `/tmp/deploy-image-proof-final-native.log`.
- Final `go vet ./...` and Linux ARM64 cross-build passed:
  `/tmp/deploy-image-proof-final-vet.log`,
  `/tmp/deploy-image-proof-final-arm64.log`; binary
  `/tmp/deploy-image-proof-arm64`. Cross-building is not native ARM64 qualification.
- Formatting, isolated-script shell syntax and `git diff --check` passed.
  No implementation changes followed the final tests. No live providers, remote
  hosts, system services or physical hardware were exercised.

Next: publication readiness and completed transition authority, followed by the
canonical database switch and runtime/snapshot/recovery support for subsequent
epochs. The new image verifier can check the retained image after that switch;
current quarantine/original-source gates must remain until the transition and
ancestor contracts are implemented. Managed handoffs, worker-wide LED cleanup,
Signal installation, configuration/UID/socket transitions, backup service cutover,
remaining compaction/decommission and live qualification are still open. Native
ARM64 and physical LED execution remain user-deferred.

## Previous checkpoint: sealed publication image preparation

Continued on 2026-11-01. Added a prepared database image with its historical
checkpoint proof anchored in SQLite. This completes another publication
prerequisite, not canonical publication or activation. All three plans remain
incomplete. No live provider, remote host, system service or physical LED was
contacted. No implementation commit has been made; preserve the working tree.

- Added `worker.PrepareRecoveryPublication` and the local
  `notification-worker recovery-publication-prepare` command. Inputs require the
  exact operation, current review, checkpoint and historical proof digests. The
  existing worker and all producer leases span preparation and revalidation.
- Copies the retained checkpoint to an intent-owned private temporary, validates
  its exact source bytes before opening SQLite for writing, then inserts one
  `recovery-publication-v1` metadata anchor containing the fenced historical
  proof. Journaling is disabled only on this disposable copy. Existing anchors
  cannot be overwritten; ancestor reconciliation remains a future requirement.
- Verifies that excluding only the exact anchor leaves the complete reviewed
  logical projection unchanged, including schema, user/application versions,
  typed rows and implicit row IDs. NULL metadata keys remain in the comparison.
  Integrity, file safety, sidecars, byte digests and configured size/free-space
  bounds are checked. The image is synced before sealing.
- Under `recovery/<operation>/checkpoints/<review_hash>/`, the private
  `publication.db` is bound by `publication-intent.json`,
  `publication-sealed.json` and `publication-image.json`. The result includes
  `image_hash`, database/anchor/checkpoint/proof digests and byte count. Only an
  intent-owned unsealed temporary may be discarded on retry. Lost or damaged
  sealed images are refused; exact completed retries return the same result.
- Canonical state.db, current reconciliation, receipts, provider cursors and all
  source/delivery/execution fences remain unchanged. All three permission flags
  remain false, and normal startup still refuses the recovery quarantine. This
  image may retain pending stages; it is not evidence that handoffs are ready or
  provider cutoffs remain fresh. A changed review requires a new checkpoint/image.
- Tests cover stale/wrong pins before writes, anchor overwrite refusal, exact
  replay, missing/corrupt/unsafe artifacts, changed evidence or leases,
  cancellation, SQL mutations and NULL-key preservation. Replaced temporary
  symlinks/hardlinks/bytes/sidecars are refused before the anchor write, with the
  original canonical bytes retained. Error injection and real subprocess
  SIGKILL exercise all eight persistence boundaries and exact resumption.
- Composition covers recovered delivery, commands, Signal/Telegram cursors and
  inherited LED evidence. Backup handoff tests use the public API with both
  notification-only and interactive workers. CLI tests cover flags, dry-run,
  deterministic native output and refusing to rebuild a lost sealed image.
  README, CLI help, all three plans and the isolated script include the workflow.

Verification:

- Final full repository suite passed: deployment 160.023s, worker 80.424s,
  backup 34.087s and all other packages, `/tmp/deploy-publication-final-all.log`.
  Local socket/process fixtures ran outside the sandbox with permission.
- Final publication race tests passed (186.938s), including all eight SIGKILL
  boundaries and replaced-temporary rejection,
  `/tmp/deploy-publication-final-race.log`.
- Recovery composition race tests passed (backup 12.645s, worker 41.353s,
  installation 8.951s), `/tmp/deploy-publication-composition-race.log`. This run
  preceded the final temporary-file guard; the final full suite above includes
  the composition tests with that guard.
- Final CLI and built-native executable tests passed (2.173s),
  `/tmp/deploy-publication-final-cli.log`.
- Final `go vet ./...` and Linux ARM64 cross-build passed:
  `/tmp/deploy-publication-final-vet.log`,
  `/tmp/deploy-publication-final-arm64.log`; binary
  `/tmp/deploy-publication-arm64`. This is not native ARM64 qualification.
- Formatting, isolated-script shell syntax and `git diff --check` passed.
  No implementation changes followed the final checks. No live provider,
  remote-host, service-manager or physical hardware qualification was performed.

Next: implement publication readiness, completed publication authority and the
canonical database transition using these sealed images. Revalidate current
provider/service conditions under leases and preserve the historical ancestor
chain. Extend runtime/snapshot/recovery readers for subsequent epochs before
removing any quarantine gate or original-source invariant. Other unfinished work
remains worker-wide LED aggregate cleanup, managed source/service handoffs,
Signal installation, activation/config/UID/socket transitions, backup service
cutover, remaining compaction/decommission orchestration and live qualification.
Native ARM64 and physical LED execution remain user-deferred.

## Previous checkpoint: independent historical checkpoint verification

Continued on 2026-11-01. Added a complete retained-checkpoint verifier as the next
prerequisite for publication/ancestor references. All three plans remain incomplete.
No live provider, remote host, system service or physical LED was contacted. No
implementation commit has been made; preserve the working tree.

- Added `worker.VerifyRecoveryCheckpoint` and
  `notification-worker recovery-checkpoint-verify --local --config … --state-dir …
  --operation … --review-hash … --checkpoint-hash …`. It requires exact pins and
  the original configuration, takes the existing offline worker lease, and never
  initializes state, repairs evidence or runs migrations.
- Verification reads the retained source, candidate, original and checkpoint
  databases immutably. It checks private-file bounds, exact byte digests, schema,
  SQL identities/recovery bindings, original-source/reconciliation acknowledgments,
  logical projection hash and all evidence archive records/copies. Any SQLite
  sidecar beside a retained database is refused, including empty sidecars.
- The frozen identity/recovery journal and original/reconciliation manifests come
  from the evidence archive. Complete saved ownership histories are replayed and
  checked against their archived heads and intents. Canonical state.db, current
  identity/ownership heads, mutable stage JSON and reconciled.db are not inputs.
  A later canonical transition therefore does not invalidate intact old evidence.
- The returned `RecoveryCheckpointProof` binds both epochs, snapshot, configuration,
  review/checkpoint/archive digests and all four database digests into a stable
  `proof_hash`. Pending stages remain the saved review's historical observation;
  publication/delivery/execution flags remain false. This is not proof that a
  publication completed or that provider/service handoff conditions are fresh.
- The reader verifies bytes before and after SQL checks, repeats the retained
  observation, and rechecks the stable worker lease and directory identities.
  Changed inputs, replaced leases/directories, unsafe files, wrong configuration
  or references, cancellation and missing archives fail without success output.
- Tests cover no-write replay, an intact old proof after a newer ownership
  checkpoint, replaced canonical files, deleted mutable projection/manifests,
  damaged inputs and sidecars, lost seals/evidence, wrong pins, lease contention
  and mutation during reading. Composition tests verify recovered delivery,
  commands, Signal/Telegram cursors and inherited LED evidence. Backup handoff
  tests use the public verifier for notification-only and interactive workers.
  Native CLI tests verify unchanged output after deleting canonical/mutable records
  and refusal after deleting a retained database. README, help, all three plans
  and the isolated script describe/include the workflow.

Verification:

- Final focused tests passed (worker 4.217s, installation 0.273s),
  `/tmp/deploy-retained-proof-final-focused.log`. Final race tests passed (worker
  56.789s, installation 4.936s), `/tmp/deploy-retained-proof-final-race.log`.
- Built-native CLI verification passed (installation 1.479s), including independence
  from canonical/mutable files and rejection of a lost retained database,
  `/tmp/deploy-retained-proof-native.log`.
- Public backup handoff verification passed with race detection (7.339s),
  `/tmp/deploy-retained-proof-backup-race.log`; local Unix-socket fixtures ran
  outside the sandbox with permission.
- `go vet ./...` and Linux ARM64 cross-build passed:
  `/tmp/deploy-retained-proof-vet.log`, `/tmp/deploy-retained-proof-arm64.log`;
  binary `/tmp/deploy-retained-proof-arm64`. This is not native ARM64 qualification.
- Full repository tests passed: deployment 160.291s, worker 66.572s, backup
  32.349s and all other packages, `/tmp/deploy-retained-proof-all.log`.
- Formatting, isolated-script shell syntax and `git diff --check` passed. All
  checks completed; no implementation changes followed them. No live provider,
  remote host, system service or physical hardware qualification was performed.

Next: use this historical proof as an explicit anchor in the ancestor/publication
protocol, implement completed publication authority and canonical replacement,
and extend runtime/snapshot readers for subsequent recovery epochs. The startup
quarantine gate and original canonical-source invariant remain intact. Other
unfinished work remains worker-wide LED aggregate cleanup, managed source/service
handoffs, Signal installation, activation/config/UID/socket transitions, backup
service cutover, remaining compaction/decommission orchestration and live
qualification. Native ARM64 and physical LED execution remain user-deferred.

## Previous checkpoint: retained recovery evidence and administrative review

Continued on 2026-11-01. Recovery checkpoints now retain independent proof bytes
alongside the SQL projection. All three plans remain incomplete. No live provider,
remote host, service or physical LED was contacted, and no commit has been made.
Preserve the working tree.

- Extended `recovery-checkpoint` without adding another command. Under the same
  worker/producer leases, it now captures stage JSON, worker identity/recovery
  records, enrolled ownership heads/intents and every linked ownership revision,
  plus administrative intents/results. Unrelated root files, credentials, live
  locks and temporary writes are outside this inventory.
- A shared bounded inventory drives both the recovery review hash and archive
  capture. It pins logical source names, exact byte counts and SHA-256 digests;
  invalid filenames, unsafe files and file/byte/history limits are refused.
  This strengthens the fingerprint: obtain fresh recovery-status output after
  upgrading from the earlier projection-only implementation. Old artifacts remain.
- Each checkpoint has `evidence-intent.json`, `evidence-sealed.json` and
  `evidence.json`; the numbered files under `evidence/` contain original bytes.
  The manifest binds the inventory to the exact checkpoint and review. Only an
  intent-owned incomplete temporary can be discarded on retry. Renamed corrupt
  copies and lost/damaged sealed archives are not silently regenerated.
- Added an internal retained-proof reader that checks checkpoint/review digests,
  manifest bindings, bounds, independent records and every saved file without
  consulting mutable canonical files. Tests show a later ownership transition
  and checkpoint preserve the earlier proof archive. This supplies current-epoch
  proof material; it does not implement the complete recovery ancestor chain.
- Read-only recovery review now verifies original administrative history, its
  reconciliation acknowledgment, exact archived SQL records and independent
  intents/results. Pending intents stay fenced, missing result acknowledgments
  stay pending, and no administrative decision is applied or result file repaired.
  Lost SQL authority, changed intents/results and deleted reconciliation proofs
  are errors. SQL and file histories participate in the same reviewed observation.
- Added corruption, unsafe-file, late mutation, changed leases, cancellation,
  bounds, partial-copy ownership, multiple checkpoint and administrative-state
  tests. SIGKILL coverage now includes five evidence stages in addition to the
  six existing projection stages. Native CLI tests verify evidence capture and
  reject a deleted sealed file; backup handoff tests compose the public checkpoint
  API for both notification-only and interactive workers.
- The initial new administrative test fixture omitted its private directory;
  corrected before the passing focused runs. Final review also strengthened the
  retained reader against altered structs that retain stale digest fields.
  README and all three plans describe the new checkpoint behavior and limits.

Verification completed:

- Full repository tests passed: deployment 161.019s, worker 64.009s, backup
  33.142s and all other packages, `/tmp/deploy-recovery-evidence-all.log`. This run
  preceded the final retained-reader digest guard; final tests below include it.
- Expanded stage/status/checkpoint race tests passed: worker 187.573s, installation
  4.905s, `/tmp/deploy-recovery-evidence-race.log`. All eleven SIGKILL boundaries
  passed. The backup portion of that invocation failed solely because the sandbox
  blocked its Unix-socket fixture (`setsockopt: operation not permitted`). Its
  permitted outside-sandbox rerun passed (6.282s),
  `/tmp/deploy-recovery-evidence-backup-race.log`.
- Final complete evidence race tests passed (65.168s), including the retained
  digest guard, bounds, administrative records, partial-copy ownership and
  multi-checkpoint history, `/tmp/deploy-recovery-evidence-final-race.log`.
- Final built-native status/checkpoint tests passed (installation 1.497s), including
  refusal of a deleted sealed evidence file,
  `/tmp/deploy-recovery-evidence-final-native.log`.
- Final `go vet ./...` and Linux ARM64 cross-build passed:
  `/tmp/deploy-recovery-evidence-final-vet.log`,
  `/tmp/deploy-recovery-evidence-final-arm64.log`; binary:
  `/tmp/deploy-recovery-evidence-arm64`. This is not native ARM64 qualification.
  Formatting, isolated-script shell syntax and `git diff --check` passed. All
  checks completed; no implementation changes followed them.

Next: integrate these retained proofs into an explicit ancestor chain and durable
completed publication authority, then implement canonical-database replacement.
Original-source checks still require the old canonical database, runtime startup
still refuses quarantine, and recovered runtime snapshots remain gated. Keep the
original/source/candidate databases and the full recovery tree. Other unfinished
work remains worker-wide LED aggregate cleanup, managed source/service handoffs,
Signal installation and activation/config/UID/socket transitions, backup service
cutover, remaining compaction/decommission orchestration and live qualification.
Native ARM64 and physical LED execution remain user-deferred.

## Previous checkpoint: review-pinned recovery projection

Continued on 2026-11-01. Added durable immutable projection checkpoints as the
next prerequisite for recovered-database publication. All three plans remain
incomplete. No live provider, remote host, service or physical LED was contacted;
no implementation commit has been made. Preserve the working tree.

- Added `notification-worker recovery-checkpoint --local --config … --state-dir …
  --operation … --review-hash …` and `worker.CheckpointRecovery`. The operation
  and current review are required; original-source preservation must be complete.
  A stale review fails before creating a checkpoint, even if an older copy exists.
- Checkpoints live in `recovery/<operation>/checkpoints/<review_hash>/`. The
  intent pins the complete review and recovery/candidate binding. A separate
  `sealed.json` pins exact bytes before the no-replace rename to `projection.db`;
  `checkpoint.json` records completion. Interrupted unsealed copies can resume;
  lost/damaged sealed or completed copies cannot be silently regenerated.
- SQLite online backup captures a pinned read transaction, including committed
  WAL contents and sparse implicit row IDs. It avoids VACUUM's potential row-ID
  renumbering. Captured databases pass integrity and logical projection checks,
  plus private-file checks, byte digests and file/directory fsync. Disposable
  copies have journaling disabled and are never used before sealing/validation.
- The worker lease, all producer leases and the reconciliation mutex span review,
  copying and repeated evidence verification. Changed SQL, source evidence,
  ownership locks, checkpoint directories/intents/seals and cancellation refuse
  success. The review fingerprint now includes SQLite schema-version/application-ID
  headers, which otherwise are absent from sqlite_master's table rows.
- Checkpointing writes no source SQL, ownership transition, receipt, result or
  grant. Publication/delivery/execution flags remain false. Its artifact is not a
  standalone recovery archive: original/source/candidate databases and independent
  stage/ownership histories must remain. Captures are bounded to 128 per operation
  and configured database/free-space limits; automatic pruning is not implemented.
- Added stable replay, WAL-only data, sparse row-ID and text/blob preservation,
  interrupted-copy, unsafe-file, changed-evidence and late-mutation regressions.
  Six real SIGKILL boundaries include a partially copied multi-page database.
  Composition tests retain reconciled delivery, commands, Signal/Telegram cursors
  and inherited LED faults without changing the review or contacting a daemon.
  CLI flag/dry-run and built-executable tests cover operator use.
- A final mutation test caught JSON field retention when reusing a decoded intent
  struct. The checkpoint reader now decodes into a fresh zero value. The focused
  suite passes after the fix; the failing earlier log is retained for diagnosis.
  README, CLI help, all three plans and the isolated script reflect this step.

Verification completed:

- Final expanded race tests passed: worker 85.490s, installation 4.239s,
  `/tmp/deploy-recovery-checkpoint-verified-race.log`. This includes all six SIGKILL
  cases, composed recovered projections and the late JSON-mutation regression.
- Final affected packages passed: worker 58.939s, backup 34.252s and all other
  notification/backup packages,
  `/tmp/deploy-recovery-checkpoint-verified-packages.log`.
- Full repository tests passed (deployment 166.348s, worker 56.368s, backup 32.898s),
  `/tmp/deploy-recovery-checkpoint-all.log`. This run preceded the final late-intent
  guard/decoder fix; the final affected-package and race runs above include it.
- Final built-native status/checkpoint tests passed (installation 1.541s),
  `/tmp/deploy-recovery-checkpoint-verified-native.log`. Final `go vet ./...` and
  Linux ARM64 cross-build passed, respectively
  `/tmp/deploy-recovery-checkpoint-verified-vet.log` and
  `/tmp/deploy-recovery-checkpoint-verified-arm64.log`; binary:
  `/tmp/deploy-recovery-checkpoint-arm64`. This is not native ARM64 qualification.
- Formatting, isolated-script shell syntax and `git diff --check` passed. No
  implementation changes followed these final checks. No live provider, service,
  remote host or physical hardware qualification was performed.

Next: preserve independent evidence and ancestor proof chains, then implement
explicit completed publication authority and the canonical-database transition.
The original-source invariant still checks canonical state.db, runtime startup
still refuses quarantine, and snapshots of recovered runtime state remain gated.
A projection checkpoint must not bypass any of these rules. Worker-wide LED
aggregate reconciliation, managed recovery/service handoffs, shared Signal
installation, activation/config/UID/socket transitions, backup service cutover,
remaining compaction/decommission orchestration and live qualification remain
unfinished. Native ARM64 and physical LED execution remain user-deferred.

## Previous checkpoint: offline recovery stage review

Continued on 2026-11-01. Added a read-only recovery inventory and operator CLI for
reviewing the durable stages needed by the eventual publication coordinator. All
three plans remain incomplete; this does not publish a database or activate a
runtime. No live provider, remote host, system service or physical LED was contacted.
Preserve the dirty working tree; no implementation commit has been made.

- Added `notification-worker recovery-status --local --config … --state-dir …
  --operation …` and `worker.ReviewRecovery`. They require an existing completed
  quarantine and initialized reconciliation projection. They do not create state,
  run initializers that write schema, repair evidence or complete pending intents.
- The reader retains the exclusive worker lease and every enrolled producer lease
  together, reads SQLite with `mode=ro`/`query_only`, and validates the exact
  operation/configuration/candidate/original-source authority. Busy or replaced
  leases, changed source/candidate/evidence, unsafe files/sidecars and mismatched
  identities are errors. A failed multi-producer acquisition releases prior leases.
- Reports contain sorted `complete`, `pending` and `retained_fenced` stage checks,
  plus logical projection, independent evidence and complete review digests.
  `publication_allowed`, `delivery_enabled` and `execution_enabled` stay false.
  The report is an observation, not a fresh health claim or publication grant.
- Producer checks validate canonical ownership and its complete history, retained
  source summaries/records/payloads/receipt provenance, compacted gaps, ownership
  transfer and staged generations. Explicit retry/cancel intents, applications and
  generation bindings remain separate. Undecided ambiguous messages and absent
  producers stay fenced; damaged completed history is not relabeled as pending.
- Command checks validate recovered source checkpoints, original mappings and
  installed outcome projections against both file intents and SQL acknowledgments.
  Valid interrupted installation remains pending. Missing reconciliation behind
  installed outcomes, lost intent/acknowledgment, altered projection or restored
  grant/confirmation authority is refused. Reviewing creates no receipt or effect.
- Intake checks validate request seals and installed Signal/Telegram cursor/handoff
  evidence. Valid cursor intents interrupted before SQL commit remain pending and
  are not completed. A separate pending activation recheck remains explicit: this
  offline review proves neither stopped receivers nor fresh provider cutoffs.
- LED checks validate retained adoption/preservation/inventory, epoch handoff and
  cleanup/import history. Inherited fault rows stay `retained_fenced`; review
  never contacts the daemon, clears a fault or renews healthy output.
- Projection hashing uses a bounded read transaction over schema and typed rows,
  including implicit row identities and committed WAL-visible data. Text is hashed
  as bytes so invalid UTF-8 cannot collapse distinct values. Evidence hashing is
  bounded and includes stage JSON and canonical ownership heads/intents. Historical
  ownership, immutable inputs and file digests are rechecked before output; a
  connection-pinned `PRAGMA data_version` detects concurrent outside SQL commits.
- Added CLI argument/dry-run/native tests, stable replay/no-init/no-repair checks,
  multi-producer lease tests, changed-evidence refusals, WAL/type/budget hashing,
  interrupted cursor/command intents, retained LED ownership, corrupt completed
  command/delivery evidence and actual backup handoff composition. Initial test
  compilation used the wrong existing command-fixture signature; corrected before
  the passing runs below. README, help, all three plans and the isolated script
  describe/include the new workflow.

Verification so far:

- Expanded stage/evidence race tests passed repeatedly. Final complete focused
  run: worker 19.798s, installation 2.190s,
  `/tmp/deploy-recovery-status-verified-race.log`. This includes the final pending
  intent handling and simultaneous producer-lease test.
- Backup handoff/read-only review composition passed with race detection (3.365s),
  `/tmp/deploy-recovery-status-backup.log`. Final built-native review and backup
  handoff passed with race detection: installation 2.650s, backup 3.111s,
  `/tmp/deploy-recovery-status-native-final.log`.
- Full repository `go test ./... -count=1 -timeout=10m` passed: deployment
  165.456s, backup 33.514s, worker 52.606s and all other packages,
  `/tmp/deploy-recovery-status-all.log`. This precedes the final receipt/projection
  checks and pending-intent refinements. Final affected packages passed afterward:
  worker 50.921s, backup 33.106s and all other notification packages,
  `/tmp/deploy-recovery-status-packages-final.log`.
- Final normal/tagged vet and Linux ARM64 cross-build passed:
  `/tmp/deploy-recovery-status-vet-final.log`,
  `/tmp/deploy-recovery-status-vet-native-final.log`,
  `/tmp/deploy-recovery-status-arm64-final.log`. Formatting, shell syntax and
  `git diff --check` passed. All final checks completed; no implementation changes
  followed them. This is a cross-build, not native ARM64 qualification.

Next implementation remains durable recovered-database publication and safe
repeated recovery, with explicit completed authority instead of deleting the
quarantine marker. The review hashes must be revalidated under publication leases;
they cannot replace source/service handoff, current intake cutoff checks or the
ancestor proof chain. Existing source/candidate/reconciled databases and per-epoch
proofs must remain intact. Other pending work remains worker-wide LED aggregate
reconciliation/cleanup; other producer adapters and managed recovery orchestration;
shared Signal service/account installation/handoff; qualified activation; managed
config/UID/socket transitions; backup report-service installation/cutover/rollback;
final outcome/message/reference/admin-audit compaction; decommission orchestration.
Live provider/external-monitor and actual user-systemd qualification are still
outstanding. Native ARM64 and physical LED execution remain user-deferred.

## Previous checkpoint: retained original recovery database

Continued on 2026-11-01. Recovery preparation now preserves the complete original
worker database independently of canonical `state.db`. All three plans remain
incomplete; this closes the source-preservation prerequisite for publication.
No live provider account, system service, remote host or physical LED was changed.
Preserve the dirty working tree; no commit made.

- `notification-worker recovery-prepare` now completes original-source archival
  before returning success. It returns `original_source_hash` alongside the
  existing operation/snapshot/candidate fields. Initial preparation and exact
  retry open the same offline reconciliation session and verify the exact operation.
- `recovery_original.go` coordinates a durable `original-source.json` intent,
  fixed private temporary, no-replace `original.db` publication, independent SQL
  acknowledgment and completed manifest. The record pins the operation, snapshot,
  before/after identities, configuration, schema, byte length and SHA-256 digest.
  Completed damaged/missing archives are never silently regenerated. Only an
  incomplete temporary owned by the matching intent may be removed on resume.
- `recovery_original_files.go` validates the source and SQLite sidecar types,
  checkpoints committed WAL contents under the exclusive worker lease, enforces
  database/free-space bounds, and syncs files/directories. Archived reads use
  SQLite immutable/read-only mode and refuse all archive sidecars. Canonical
  source checks still reject changed main-file bytes or nonempty WAL/journal;
  publication must explicitly replace this canonical-source invariant later.
- LED fault and domain readers now use the retained archive once present. Pending
  preservation cannot fall back to mutable state. Legacy low-level preparations
  remain readable before archival; `recovery-prepare` upgrades them through the
  same durable protocol. Original epoch, fault, operation and domain evidence
  remain unchanged, and ordinary worker startup remains quarantined.
- Authority is checked at stage boundaries, before SQL commit and before both
  manifest completion and CLI output. Review found and fixed a gap where the
  ready manifest could overwrite intent changes after SQL acknowledgment; a
  separate completion check now refuses changed/missing acknowledgment or intent.
- Tests cover a real SIGKILL leaving a committed transaction only in WAL (the
  immutable main DB does not contain it), interrupted/partial copies, five new
  archival SIGKILL boundaries, unchanged replay, original-source main/WAL drift,
  invalid source epochs/schemas/enrollment, missing/damaged file and SQL authority,
  symlinks/hardlinks/permissions, unowned artifacts, replaced leases, cancellation,
  source change before SQL commit and archive damage just before CLI output.
- Backup handoff and backup LED fixtures now prepare through the public archive
  path. Retained command outcomes and producer receipts survive handoff. Existing
  LED inventory/domain proofs survive preservation, including resume after an
  interrupted archive with LED evidence already recorded. The pinned mock-daemon
  cleanup/import workflow and built-native backup handoff exercise the archive.
- README and all three plans describe this step and its remaining publication
  limits. No recovery gate was deleted and no dispatch/command authority granted.

Verification:

- Initial focused race suite passed: worker 22.694s, installation 1.387s,
  `/tmp/deploy-original-race.log`. The expanded suite initially failed a new
  sidecar-symlink fixture because the expected WAL filename already existed;
  the fixture now retains that file before installing the test symlink. Corrected
  expanded race suite passed: worker 24.106s, installation 1.390s,
  `/tmp/deploy-original-race-verified.log`.
- Final source-authority/quarantine race tests passed after the completion-gap
  fix: worker 27.145s, `/tmp/deploy-original-authority-race.log`.
- Full `go test ./... -count=1 -timeout=10m` passed: deployment 162.251s,
  backup 31.142s, worker 49.716s and every other package,
  `/tmp/deploy-original-all.log`. This precedes the last completion guards and
  fixture updates. Final affected packages passed afterward: worker 54.034s,
  backup 34.904s, all other notification packages too,
  `/tmp/deploy-original-packages-final.log`.
- Final native CLI/mock-daemon tests passed with race detection: worker 4.208s,
  installation 2.101s, backup 3.324s,
  `/tmp/deploy-original-native-verified.log`. These use disposable local state and
  the pinned offline `/home/sal/code/pi-status-led` mock, not physical hardware.
- Final normal and tagged-integration vet passed; Linux ARM64 cross-build passed.
  Logs: `/tmp/deploy-original-vet-verified.log`,
  `/tmp/deploy-original-vet-native-verified.log`,
  `/tmp/deploy-original-arm64-verified.log`. Native ARM64 remains deferred.
- Broader backup handoff/LED race suite passed (163.220s),
  `/tmp/deploy-original-backup-race.log`. It began before the final completion
  guard; the final affected-package, authority-race and native runs above cover
  that guard. Formatting, shell syntax and `git diff --check` passed. All test,
  vet and build processes completed; no implementation changes followed them.

Next implementation remains final recovered-database publication with durable
completed authority and safe repeated recovery. Preserve ancestor fault ownership,
operation/cleanup/import proofs and per-recovery journals; do not delete the
quarantine gate or reinterpret singleton journals under another epoch. Also still
pending: worker-wide LED aggregate reconciliation/cleanup; other producer adapters
and managed recovery orchestration; shared Signal service/account installation/
handoff; qualified activation; config/UID/socket transitions; backup report-service
installation/cutover/rollback; final outcome/message/reference/admin-audit compaction;
decommission orchestration. Live provider/external-monitor and actual user-systemd
qualification remain outstanding. Native ARM64/physical LED tests remain deferred.

## Previous checkpoint: backup delivery and command recovery handoff

Continued on 2026-11-01. Completed the local backup recovery handoff coordinator
and its operator entry point. All three plans remain incomplete. No live provider
account, remote host, system service or physical LED was changed; no commit made.

- Added `backup-worker recovery-handoff --local --config … --state-dir …
  --notification-config … --notification-state … --operation … --summary-hash …`.
  `recovery-notifications` now includes the digest as `summary_hash`, alongside
  its existing summary fields. Handoff requires that exact completed summary on
  both source and gateway before proceeding.
- One `worker.RecoveryHandoffSession` holds the exclusive offline gateway lease
  through command reconciliation, producer ownership transfer, staged generation,
  source delivery identity/zero-cursor commit, command identity commit and retained
  outcome installation. Existing stage intents/proofs provide crash resume; the
  coordinator does not invent a second epoch/generation or checkpoint.
- Interactive sources retain their existing `commands.lock` throughout and must
  prove the original backup archive binding. The source schema lease excludes
  concurrent command initialization. Both lease file identities are checked across
  stage boundaries. Missing or changed state/leases are refused, not recreated.
- Added generic `interaction.OpenRecoveryLedger` to reopen either the original
  identity or the exact proof-backed committed identity. It validates the pinned
  producer, operation, before/after identities, retained capture and commit proof;
  partial/missing/foreign authority cannot trigger a fallback or initialization.
  Command challenges and summaries are checked before the coordinator can capture
  source history under a foreign recovery operation.
- Notification-only handoff requires actual absence of source command history and
  retained gateway commands/recovery/installation records. Missing enrollment is
  insufficient. The gateway repeats this check in `PrepareRecoveryProducerCommit`,
  so older callers also cannot bypass retained command history. Other producers'
  history does not prevent an independent source's handoff.
- Successful command results, original request mappings and receipt provenance
  survive. Pending commands remain uncertain. Old grants and confirmations stay
  fenced; delivery ownership has no active generation; the new source receipt
  cursor is zero. No domain operation runs, no new receipt is acknowledged, and
  no provider reply, service or recovered runtime is activated. Exact replay
  returns the same result, with both activation flags false.
- Tests use real backup and gateway databases with an executed command, its
  acknowledged result, a post-snapshot pending command and retained notification
  acceptance. They assert a single domain effect, immutable replay, wrong-summary/
  config/archive/epoch refusal, missing/busy/replaced leases, foreign challenges,
  damaged completion proofs, and continued quarantine. SIGKILL covers ten handoff
  boundaries, including source transactions and the two distinct identity commits.
- Built-native CLI tests cover interactive and notification-only sources. The
  first native fixture normalized its configuration before removing commands,
  causing the expected config-mismatch refusal; fixture order is corrected.
  Composition tests prove that both leases span the stages and existing exact
  LED cleanup/import remains usable after handoff. The initial LED write-count
  assertion incorrectly included fixture setup writes; the corrected test checks
  zero writes during handoff and exactly one additional cleanup Clear.
- README, all three plans, command help and the isolated test script were updated.
  The coordinator does not install a replacement command-runtime config or perform
  managed service cutover. Those steps and final publication remain pending.

Verification at this checkpoint:

- Initial handoff and strict-ledger race tests passed (backup 10.229s,
  interaction 1.355s), `/tmp/deploy-recovery-handoff-race.log`.
- Handoff/CLI/SIGKILL/ledger race suite passed (backup 26.988s,
  interaction 1.341s), `/tmp/deploy-recovery-handoff-race-final.log`.
  Corrected LED composition passed with race detection (5.045s),
  `/tmp/deploy-recovery-handoff-composition-final.log`; corrupted-completion
  replay tests passed (7.286s), `/tmp/deploy-handoff-damage-race.log`.
- Native handoff passed with race detection (2.626s),
  `/tmp/deploy-recovery-handoff-native-final.log`. Final native handoff and
  reconciliation with the retained-command guard passed (3.602s),
  `/tmp/deploy-handoff-native-guards.log`.
- Clean full `go test ./... -count=1 -timeout=10m` passed: deployment 160.316s,
  backup 30.618s, worker 48.156s and all other packages,
  `/tmp/deploy-handoff-all-final.log`. This predates the final command-absence
  guard; the final affected-package and focused race runs below cover that change.
  An earlier full default run failed only the corrected LED fixture assertion.
- The earlier full source race run likewise failed only that obsolete assertion
  (backup 241.720s); interaction passed (4.812s). Do not call this a clean full
  source race pass. Its log is `/tmp/deploy-handoff-source-race.log`; the corrected
  composition, full coordinator and command-absence race checks are separate.
- Final `go vet ./...`, tagged integration vet, formatting, shell syntax and
  `git diff --check` passed. Linux ARM64 cross-build passed; native ARM64 and
  physical LED execution remain deferred. Logs: `/tmp/deploy-handoff-vet-final.log`,
  `/tmp/deploy-handoff-integration-vet.log`, `/tmp/deploy-handoff-arm64-final.log`.

Final checks completed:

- Final handoff/producer/command-absence race tests passed: worker 8.392s,
  backup 38.717s, `/tmp/deploy-handoff-final-guards.log`. This includes all new
  coordinator tests, the corrected LED composition, damaged-completion checks,
  source/gateway absence guards and actual SIGKILL cases.
- All affected packages passed after the last production guard change: worker
  46.606s, backup 30.463s, interaction 0.304s,
  `/tmp/deploy-handoff-final-packages.log`. Final vet and ARM64 build processes
  completed successfully. No implementation changes followed these checks.

Remaining implementation: final recovered database publication and safe repeated
recovery; worker-wide LED aggregate reconciliation/cleanup; other producer adapters
and managed recovery orchestration; shared Signal service/account installation/
handoff; qualified activation; config/UID/socket transitions; backup report-service
installation/cutover/rollback; final outcome/message/reference/admin-audit compaction;
decommission orchestration. Live provider/external-monitor and actual user-systemd
qualification remain outstanding. The source-preservation, runtime quarantine and
ancestor-proof constraints recorded below still apply to final publication.

## Previous checkpoint: local recovery operator workflows

Continued on 2026-11-01. Added usable local entry points for snapshot quarantine,
backup producer reconciliation and the backup LED recovery coordinator. All three
plans remain incomplete; no production activation, live provider account, remote
host, system service or physical LED was changed. Preserve the working tree.

- `notification-worker recovery-prepare --local --config … --state-dir …
  --from … --snapshot-id … --database-hash …` verifies the exact selected artifact,
  acquires the offline worker lease, and prepares or resumes quarantine. First
  preparation pins the reviewed ID/database digest; resume also pins the full
  retained manifest. It returns the operation ID and before/after identities.
  It preserves the original DB and canonical producer histories, never initializes
  missing state and never starts services or publishes a recovered runtime.
- Preparation refuses a live worker lease, changed config/identity/artifact or
  damaged recovery authority. The original snapshot must remain available for
  retry. SIGKILL coverage spans prepared, identity publication, epoch publication,
  candidate binding/classification/commit, completed quarantine and lost output.
  Exact retry retains the operation and epoch. Native CLI and dry-run tests cover
  scope validation and normal startup remaining fenced.
- `backup-worker recovery-notifications --local --config … --state-dir …
  --notification-config … --notification-state … --operation …` connects the
  existing producer reconciliation coordinator to the CLI. The expected operation
  is checked on challenge and completed-summary responses before producer fencing
  or evidence storage. Existing source/gateway state and producer enrollment are
  required; config drift, busy leases, missing state and cancellation refuse.
  Retained messages/receipts and evaluated status cross the offline session;
  historical completion replay does not renew domain evidence. This does not
  reconcile commands, commit active binding/cursors or enable delivery.
- `backup-worker recovery-led-evidence` exchanges a real backup evaluation with
  the quarantined worker after completed producer reconciliation. It pins exact
  inventory ID/hash; existing domain locks and frozen report/ACK remain mandatory.
- `backup-worker recovery-led-clear` additionally pins the exact backup binding,
  fault, evidence hash and committed LED epoch handoff. Source leases remain held
  around fresh speculative domain checks and the bounded exact Clear. Only a
  committed acknowledged/guarded-observed-absence result is imported into the
  inherited ledger. Output includes result/hash/import; ambiguous outcomes remain
  unimported, and retries never re-dispatch a completed/uncertain attempt.
  Same-argument CLI replay requires the inventory still be current; historical
  receipt import remains separately available after inventory refresh.
- Private backup configuration validation is shared by both coordinators and
  reuses the ordinary installed-config validator. A negative test found that the
  initial generic file reader permitted mode 0644; recovery now requires an owned
  regular 0600 file, no symlink/hardlink and bounded strict JSON. CLI dry-runs read
  configuration only. No provider credential is read and no external call is made.
- Real process-kill tests cover LED evidence output, cleanup-before-import and
  import-before-output; producer CLI tests cover summary transaction/commit and
  output loss. A persistent Clear witness proves retries send no second RPC.
  Built-native producer recovery and LED evidence/clear/import use actual worker
  state and the pinned pi-status-led mock daemon; foreign/newer states survive.
  The first native fixture used a different socket from its sealed inventory and
  correctly failed. The fixture now pins the real socket from the start.
- README, all three plans, CLI help and the isolated qualification script now
  describe/include these operator workflows. They are local commands, not chat
  permissions or managed service recovery.

Verification completed so far at this checkpoint:

- Full backup race suite passed (201.492s),
  `/tmp/deploy-backup-led-cli-full-race.log`, after the LED CLI implementation.
- LED coordinator/CLI/SIGKILL race suite passed (47.502s),
  `/tmp/deploy-backup-led-cli-race.log`; final strict fault-ID flags passed
  separately (3.123s). All notification packages passed non-race, including
  worker 46.807s and installation 5.076s,
  `/tmp/deploy-backup-led-cli-notification-regression.log`.
- Producer CLI, crash and shared config-reader race tests passed (9.130s),
  `/tmp/deploy-backup-reconcile-cli-race-final.log`. Its earlier negative mode
  test failed before the private-reader correction above.
- Quarantine and existing snapshot recovery race tests passed: worker 8.693s,
  installation 1.254s, `/tmp/deploy-recovery-quarantine-race.log`.
  A later refinement adds the explicit initial database hash pin; final checks
  for that refinement are listed below when complete.
- Built-native quarantine, producer and LED recovery passed with race detection:
  installation 2.508s, backup 4.823s,
  `/tmp/deploy-recovery-operators-native.log`. Physical hardware was not tested.
- `go vet ./...` passed in `/tmp/deploy-recovery-operators-vet.log` before the
  final database-hash pin. An initial earlier vet found an unkeyed test literal;
  keyed literals corrected it. Linux ARM64 cross-building passed earlier in
  `/tmp/deploy-backup-led-cli-arm64.log`; this is not native ARM64 execution.

Final verification:

- Combined affected race suite passed: worker 9.289s, installation 1.267s,
  backup 121.517s, `/tmp/deploy-recovery-operators-race.log`. This covers existing
  snapshot recovery, producer exchange, new producer CLI, backup LED coordinator
  and their failure/crash tests. Final initial database-hash pin tests also passed:
  worker 5.259s, installation 1.264s,
  `/tmp/deploy-recovery-quarantine-final-pin.log`.
- Final built-native preparation with the ID/hash pin passed with race detection
  (1.823s), `/tmp/deploy-recovery-quarantine-native-final.log`.
- Final `go vet ./...`, tagged native-test vet, Linux ARM64 cross-build, formatting,
  shell syntax and `git diff --check` passed. Logs:
  `/tmp/deploy-recovery-operators-vet-final.log`,
  `/tmp/deploy-recovery-operators-integration-vet.log`,
  `/tmp/deploy-recovery-operators-arm64-final.log`.
- Full `go test ./... -count=1 -timeout=10m` passed: deployment 163.224s,
  backup 28.467s, worker 49.113s, installation 5.320s and all other packages.
  Log: `/tmp/deploy-recovery-operators-all.log`. It predates only the final
  quarantine database-hash pin, which is covered by the final focused/native
  runs above. All verification processes have exited; no live test services remain.

Remaining implementation: worker-wide LED aggregate reconciliation/cleanup and
final recovered database publication/runtime lifecycle (including safe second
recovery); other producer adapters and managed recovery orchestration; shared
Signal service/account installation/handoff; qualified activation; config/UID/
socket transitions; backup report-service installation/cutover/rollback; final
outcome/message/reference/admin-audit compaction; decommission orchestration.
Live provider/external-monitor and actual user-systemd qualification are still
outstanding. Native ARM64 and physical LED checks remain user-deferred. The
publication constraints in the previous checkpoint remain mandatory. No commit.

## Previous checkpoint: LED epoch history and completed-cleanup import

Continued on 2026-11-01. Implemented two more recovery stages; all three plans
remain incomplete and production activation remains unavailable. No live provider
account, system service, remote host or physical LED was changed.

- Added `recovery-led-handoff --local --config … --state-dir … --adoption-hash …`.
  It requires committed adoption, current schema 10, unchanged configuration,
  socket, UID and preserved source evidence. One transaction archives the original
  LED operations, mapping transitions, layout and adopted fault union, then starts
  empty operation/mapping histories under the recovery epoch. Every fault retains
  its original identity. The original live and snapshot databases remain intact.
- An independent epoch intent and SQL acknowledgement support exact crash resume.
  Both archived and new histories are validated using their own original identities;
  missing/changed archive tables, contents, provenance or retained rows fail closed.
  Retrying never resets new-epoch operations. Both initial and previously migrated
  LED layouts are covered, including the maximum 2,048-fault union.
- Added `recovery-led-import-cleanup --local --config … --state-dir …
  --epoch-hash … --result-hash …`. Only a committed named-binding `acknowledged`
  or domain-authorized `observed_absent` clear receipt can retire its exact inherited
  row. Uncertain/not-sent attempts cannot be imported. The original incarnation
  stays archived; the complete clear intent/result/domain proof remains mandatory.
- Cleanup imports use an independent chained head/per-decision file and atomic SQL
  acknowledgement/row removal. Pending decisions must resume before runtime
  ownership is accepted. Exact historical retry remains valid after proof expiry
  or reboot and sends no RPC. Restored rows, missing other faults, lost or damaged
  receipt/import/archive history and truncated heads are refused. Runtime excludes
  only committed imports from inherited counts, keeping newer fault IDs fenced.
- Review now includes `epoch_handoff`, durable `cleanup_imports`, up to 32 distinct
  `importable_cleanup` receipts and their full count. Neither new command clears
  daemon state, renews an evidence lease, asserts domain health, releases quarantine
  or grants provider/command/producer activation. Worker aggregate cleanup remains
  separate. CLI argument/dry-run/offline-only behavior is covered.
- Actual SIGKILL tests cover all five epoch boundaries and all five import
  boundaries. Tests also cover preserved-source drift before/during handoff,
  missing provenance, wrong hashes/socket, clock rollback, cancellation, storage
  exhaustion, stable-lock replacement, pending-import isolation and chained replay.
- Pinned pi-status-led mock-driver integration now exercises cleanup followed by
  adoption/history handoff/import, including the built native import CLI. The
  separate built-native preservation/adoption test exercises handoff and retry.
  Foreign and newer states survive; imports and retries do not mutate the daemon.
  These are disposable local mock tests, not physical hardware qualification.

Verification at this checkpoint:

- Initial focused epoch race suites passed: worker 48.977s, installation 1.097s,
  `/tmp/deploy-led-epoch-race.log`. The earlier initial non-race epoch attempt found
  a reopen validator using the old epoch against new active journals; reopening
  now validates the immutable old archive and new journals separately. Corrected
  focused tests passed (2.278s), `/tmp/deploy-led-epoch-focused-2.log`.
- Epoch guards and maximum-union race tests passed (119.970s),
  `/tmp/deploy-led-epoch-guards.log`. Final preflight non-race tests passed (4.710s),
  `/tmp/deploy-led-epoch-final-focused.log`.
- Combined epoch/import race suites passed: worker 89.286s, installation 1.182s,
  `/tmp/deploy-led-import-race.log`. Final import race coverage including SIGKILL,
  chained imports and all preflight failures passed (52.886s),
  `/tmp/deploy-led-import-final-race.log`.
- Affected non-race suites passed: worker 47.172s, backup 23.517s, installation
  4.701s and all remaining notification packages,
  `/tmp/deploy-led-recovery-packages.log`. The later bounded review-list refinement
  is included in the final import race suite and completed full race run.
- Pinned mock integration passed with race detection: worker 3.494s, installation
  3.158s, `/tmp/deploy-led-import-native.log`. Extended built-native import CLI
  integration passed (3.602s), `/tmp/deploy-led-import-native-cli.log`.
- Final pinned mock LED regression matrix passed: LED client 2.287s, worker
  8.064s, `/tmp/deploy-led-recovery-pinned-regression.log`. This includes normal
  fault cleanup, inherited ownership, producer retirement, recovery preservation/
  cleanup/import and verification mode. Formatting, shell syntax and
  `git diff --check` passed.
- `go vet ./...` and Linux ARM64 cross-build passed, in
  `/tmp/deploy-led-recovery-vet.log` and `/tmp/deploy-led-recovery-arm64.log`.
  Cross-building is not native ARM64 execution.
- Full `go test -race ./internal/notification/... ./internal/backup -count=1
  -timeout=20m` passed outside the sandbox: worker 610.721s, backup 157.059s,
  installation 57.538s and all other notification packages. Log:
  `/tmp/deploy-led-recovery-full-race.log`. All test processes have exited; final
  validation runs passed. Only documentation/comment edits followed
  the final production code tested by this run.

Remaining LED recovery work: worker-wide aggregate reconciliation and exact
cleanup, managed producer cleanup orchestration, final recovered database
publication, and connecting these primitives to the recovered runtime lifecycle.
The ownership reader now accepts removed rows only through validated committed
imports. No ordinary `Open` quarantine gate was removed. Other remaining work:
shared Signal service/account installation/handoff, qualified activation, other
managed config/UID/socket/schema transitions, shared backup report installation/
cutover/rollback, final-outcome/message/reference/admin-audit compaction and producer
decommission orchestration. Live provider/external monitor and actual user-systemd
qualification remain outstanding. Native ARM64/physical LED execution remain
user-deferred. No commit was made.

Publication follow-up constraints from this review:

- Original-source preservation and retained-path reading are now implemented in
  the latest checkpoint. Canonical `state.db` is still checked for drift; final
  publication must explicitly transfer that invariant to its completed authority.
  Renaming the candidate over it without that transition would invalidate evidence.
- Ordinary `Open` refuses any `recovery.json`; inherited ownership currently expects
  its quarantined binding. Publication needs explicit durable completed authority
  and runtime validation of that authority, not deletion of the gate file.
- Subsequent snapshots/restores of a recovered worker must preserve ancestor fault
  ownership and operation/cleanup/import proofs. The current per-recovery singleton
  journals cannot simply be overwritten or reinterpreted under another epoch.
  Include a second-recovery scenario in the publication contract tests.

## Previous checkpoint: inherited LED ownership and runtime guards

Continued on 2026-11-01. Implemented the runtime ownership validation and output
fences needed to retain adopted faults without relabeling their recovery epoch.
All three plans remain incomplete; final recovered-state publication, production
activation and the other remaining work below are not implemented by this change.
No live account, system service, remote host or physical LED was changed.

- Split immutable LED adoption authority from the exact quarantined projection
  check. Both readers verify independent adoption/preservation/inventory files
  against their SQL acknowledgements. Offline recovery still requires its exact
  adopted union and rejects changed rows/operation projections.
- Runtime ownership checks pin canonical and SQL identity, the recovery journal,
  configuration, UID and exact mapping. Every inherited incarnation must retain
  its original ID/binding/condition/identity. Missing rows are refused, never
  recreated. At most 4,096 runtime fault rows are examined, including the full
  2,048-fault inherited union. Losing both SQL markers cannot hide a surviving
  independent adoption intent. No provider or daemon call establishes ownership.
- `Health` exposes `inherited_led_faults` by binding. Inherited critical faults
  remain critical; other inherited conditions remain at least unknown. They grant
  no positive lease. Ordinary fresh status, expiry or reboot cannot discharge them.
- Current runtime fault selection skips inherited incarnations and allocates a
  new current-epoch ID when needed. Normal cleanup selects/marks at most 128
  current rows and excludes inherited rows. Direct Set/Clear attempts on inherited
  IDs remain refused even if an old cleanup tombstone exists. Legacy relabeling
  cannot bypass the immutable proof. Other applications' registry state is kept.
- Runtime passes validate provenance before daemon I/O. Old operation/mapping
  journals must pass the current-epoch checks before any new operation is appended;
  this preserves the remaining archival handoff requirement. No history is
  relabeled, silently reset or replayed. Healthy-lease revocation in tested runtime
  passes does not imply that the final recovered publication path exists.
- Tests borrow an exclusively held, still-quarantined candidate to exercise these
  runtime primitives. Normal `Open` still refuses it. There is no new public
  activation endpoint and no managed recovery bypass.
- Added missing/corrupt proof/identity/mapping/row tests, direct mutation refusal,
  old-operation-history refusal, repeated runtime passes, expiry/reboot behavior,
  maximum-union validation and pinned pi-status-led mock-driver integration. The
  isolated script now includes `TestInheritedLEDOwnershipPinnedDaemon`.

Verification at this checkpoint:

- Initial focused non-race LED/adoption/retirement tests passed (7.354s).
- Affected non-race suites passed: worker 35.332s, backup 22.843s, installation
  4.613s and all other notification packages, in
  `/tmp/deploy-led-inherited-packages.log`.
- Focused inherited-ownership race tests passed (14.610s) in
  `/tmp/deploy-led-inherited-race-corrected.log`. Earlier legacy fixture attempts
  were invalid: preservation deliberately refuses unproven legacy ownership.
  The regression now tests attempted legacy relabeling of an adopted incarnation.
  The superseded full race process group was stopped; its log is not a pass.
- Guard and maximum-union race tests passed (61.744s) in
  `/tmp/deploy-led-inherited-guards.log`. Final pre-I/O guard tests passed without
  race instrumentation (1.252s) in `/tmp/deploy-led-inherited-final-focused.log`.
- Native mock-daemon integration passed with race detection (2.496s) in
  `/tmp/deploy-led-inherited-native.log`. After the final pre-I/O guards, the
  combined inherited-ownership, native daemon, normal fault-clear and verification
  race suite passed (20.655s) in `/tmp/deploy-led-inherited-final-native.log`.
- Final `go vet ./...` and Linux ARM64 cross-build passed in
  `/tmp/deploy-led-inherited-final-vet.log` and
  `/tmp/deploy-led-inherited-final-arm64.log`. Cross-building is not native testing.
- The complete worker regression suite passed after the final production guards
  (36.522s), logged in `/tmp/deploy-led-inherited-final-worker.log`. Additional
  binding-isolation race coverage passed (1.815s) in
  `/tmp/deploy-led-inherited-isolation.log`.
- Full `go test -race ./internal/notification/... ./internal/backup -count=1`
  passed outside the sandbox: worker 478.502s, backup 158.242s, installation 57.142s,
  and all remaining notification packages. Log:
  `/tmp/deploy-led-inherited-full-final.log`. It began before the final
  pre-I/O/history guard refinements, which the final native race/regression runs
  above cover. The last binding-isolation change added only test coverage.
- Formatting, shell syntax and `git diff --check` passed. All tests launched for
  this checkpoint have exited; the explicitly superseded race run was terminated.


Remaining LED recovery work: archive the original operation/mapping history into
an explicitly journaled runtime epoch transition; reconcile/discharge exact old
fault incarnations (including worker aggregate faults); import acknowledged cleanup
without losing provenance; and perform final recovered database publication. The
new ownership reader deliberately refuses missing inherited rows until that cleanup
handoff exists. It accepts ownership only, not domain resolution or clear authority.
Other remaining work: shared Signal service/account installation/handoff, qualified
activation, other managed config/UID/socket/schema transitions, shared backup report
installation/cutover/rollback, final-outcome/message/reference/admin-audit compaction
and producer decommission orchestration. Live provider/external monitor and actual
user-systemd qualification remain outstanding. Native ARM64/physical LED execution
remain user-deferred. No commit was made.

## Previous checkpoint: permanent producer retirement

Continued on 2026-11-01. Implemented explicit local retirement for an enrolled
producer epoch. All three plans remain incomplete and production activation is
still unavailable. No live accounts, services, remote hosts or LEDs were changed.

- `notification-worker producer-retire --local --config … --state-dir …
  --producer … --reason …` previews prerequisites; `--apply` commits a permanent
  retirement. Local CLI actor is derived from the UID. Dry-run opens no worker
  state. Ordinary preview uses normal startup recovery but creates no new decision.
- Every generation must be revoked. Retired subjects, settled message/command/
  policy outcomes, acknowledged receipts/results and no relevant LED fault/cleanup
  rows are required. Retirement cannot cancel work, waive incidents or clear LEDs.
- Schema-4 canonical ownership events retain actor/reason/time/evidence and an
  exact retry returns the original record. Retirement survives recovery ownership
  epoch changes. Older strict decoders reject the new event instead of reactivating
  the producer. Nil retirement fields preserve older ownership hashes.
- New message/status/policy submissions and worker command admission/grants/results
  are fenced within the state transaction. Stage/activate cannot resurrect the
  producer. Historical command queries still use current authorization. Fresh
  worker listings/help explain retirement without exposing unauthorized workers.
  Health exposes retired producers and excludes their liveness requirement while retaining
  unrelated faults. No healthy output is inferred.
- Startup and repeated retirement validate ledger evidence; conflicting restores,
  lost ownership prefixes and damaged decisions fail closed. Stable worker-lock
  inode/identity checks precede publication. Full queue/receipt/command/audit history
  remains; acknowledged intermediate receipt compaction is compatible.
- Coverage includes actual SIGKILL at intent/history/owner publication, interrupted
  replies, corrupted history/state, outstanding-obligation refusal, old encodings,
  receipt compaction, multiple producers, historical chat reads, lock/identity
  replacement and a built native CLI preview/apply/retry/refusal test. The native
  test is included in the isolated validation script.
- Existing multi-instance and recovery-request fixture setup was corrected to
  establish canonical ownership at the appropriate epoch before admitting work.
  The new admission checks do not bypass ownership for synthetic test stores.

Verification at this checkpoint:

- Focused race tests for retirement, multi-instance isolation and request recovery
  seals passed: worker 18.033s, installation 1.329s in
  `/tmp/deploy-retirement-refined.log`.
- Final focused/native race tests passed: worker 4.753s, installation 3.230s in
  `/tmp/deploy-retirement-native.log`, including the built CLI and replaced-lock
  checks. All subprocess crashes are bounded by a timeout.
- Final `go vet ./...` passed (`/tmp/deploy-retirement-vet-final.log`).
- Full `go test -race ./internal/notification/... ./internal/backup -count=1`
  passed outside the sandbox: worker 454.018s, backup 153.208s, installation
  56.794s, and every other notification package passed. Log:
  `/tmp/deploy-retirement-race.log`. The initial non-race run exposed the three
  fixture ownership errors above; it was superseded by this passing run.
- Linux ARM64 cross-build passed (`/tmp/deploy-retirement-arm64-final.log`); this is
  not native qualification. Native ARM64 and physical LED testing remain deferred.
- Additional retirement recovery-fence/LED silence race tests passed (1.321s) in
  `/tmp/deploy-retirement-edges.log`.
- Real pinned pi-status-led mock-driver integration passed with race detection
  (1.400s) in `/tmp/deploy-retirement-led-native.log`: domain retirement clears
  prior faults before producer retirement; restarting retains only baseline
  outputs, grants no healthy lease and preserves another application's state.
  This test is included in the isolated script. No physical hardware was used.
- Final retired-worker discovery/help and historical-query race coverage passed
  (5.009s) in `/tmp/deploy-retirement-gateway.log`. The complete worker suite also
  passed after that last production change (33.635s), logged in
  `/tmp/deploy-retirement-worker-final.log`. The full race suite began before
  the final help/discovery edit; focused race coverage and the final regression
  run cover it. Final vet/cross-build also include that edit.
- Formatting, shell syntax and `git diff --check` passed. All test processes
  launched for this checkpoint have exited.

Recovery follow-up inspection: normal `checkLEDState`, `validateLEDOperation` and
mapping-history validation intentionally bind ownership to the runtime recovery
epoch. Adoption retains original identities in the quarantined candidate. The
next runtime handoff must archive/validate the old operation/mapping history and
retain exact per-fault provenance before accepting those IDs; changing just the
identity comparison would bypass that contract. No recovery gate or identity
validation was relaxed during this checkpoint.

Remaining implementation includes recovered LED runtime reconciliation and final
recovery publication; shared Signal service/account installation and handoff;
qualified activation; other managed configuration/UID/socket/schema migrations;
shared backup report service installation/cutover/rollback; final outcome/message/
reference/admin-audit compaction; and producer-host decommissioning/configuration
removal. Local retirement keeps enrollment for audit. Live provider/external
monitor and actual user-systemd qualification remain outstanding. No commit made.

## Previous checkpoint: managed LED mapping migration and fault-clear guard

Continued on 2026-11-01. Implemented LED-binding-only migration through the
managed verification service lifecycle and closed a normal-runtime fault-clear
registry validation gap. All three plans remain incomplete and production
activation remains unavailable. No live accounts, system services, remote hosts
or physical LEDs were changed.

- New `notification-worker migrate-led --local --spec …` freezes and compares the
  old/new runtimes; only `worker.led_bindings` may differ. Identity, schema, socket,
  providers, enrollment, limits, paths and exclusions stay pinned. Ordinary update
  continues to reject changed configuration. Dry-run validates input files without
  creating an installation, starting a service or contacting the daemon.
- Review caught two ways to strand a prepared migration: representation-only
  mapping edits and preexisting unresolved faults. Apply now compares normalized
  bindings and checks current schema/identity/layout/history, inactive ownership,
  fault rows and capacity through a bounded read-only snapshot before publishing
  lifecycle intent or stopping the service. Tests cover explicit defaults, ordering,
  unresolved faults and inspection while the runtime owns its worker lease. All
  authoritative checks still repeat under the exclusive lease after stop.
- A separate `__managed-led-migration-probe` checks capability version 1 on the new
  runtime/helper before service stop. Existing probe response shapes stay compatible
  with older strict decoders. The new helper is staged in its immutable release;
  original helpers remain retained. Rollback cannot cross the configuration/helper
  transition, and unfinished migration cannot be cancelled through uninstall.
- Under installation/worker leases and inactive producer ownership, the lifecycle
  invokes the existing mapping protocol with its exact operation ID. A different
  operation cannot resume or adopt that completion. Changed bindings with fault or
  cleanup rows remain blocked. Mapping steps only revoke healthy leases and set
  unknown baselines. The new unit/configuration are published after exact mapping
  completion; pending recovery/schema gates cannot be bypassed.
- Completed resume and managed startup verify mapping/configuration/identity and
  journal evidence using a read-only transaction, including while the runtime owns
  the worker lease. Verification never repairs evidence or contacts the daemon.
- Normal runtime fault cleanup now lists the daemon registry before rechecking
  source evidence and issuing Clear. Caller-owned state must match the expected
  LED/style/persistence/message; bounded pause TTLs, duplicate IDs and registry
  capacity are checked. Legacy rows preserve their historical message compatibility.
  Other UIDs remain unowned. Refusal is recorded as `not_sent`, retains the cleanup
  tombstone and can be retried with fresh evidence. An absent fault is cleared
  idempotently; revoking a healthy lease remains independent of registry availability.
- Added failure/resume coverage across service publication and lost daemon replies,
  eight actual installer SIGKILL boundaries, strict worker operation/ownership pins,
  changed-registry/source/cancellation tests and native executable integration with
  the pinned pi-status-led mock driver. The native test rejects a schema-compatible
  executable without LED migration capability before stopping the old service,
  then migrates, verifies its helper/runtime, updates and uninstalls. It is included
  in `scripts/test-notification-worker-isolated.sh`.

Verification at this checkpoint:

- Focused LED/managed race suites passed: worker 29.648s, installation 43.988s in
  `/tmp/deploy-managed-led-race.log`. Additional exact-authority worker tests passed
  (0.074s) and native migration integration passed (3.035s).
- Native integration with race detection passed outside the sandbox: installation
  39.769s, worker 3.451s, LED client 2.287s in
  `/tmp/deploy-managed-led-native-race.log`. This covers managed native lifecycle,
  schema migration, LED migration, recovery cleanup/preservation/adoption and
  verification output using the pinned mock daemon. No actual user-systemd was used.
- Final focused race tests, including registry limits/other UIDs/cancellation/pause
  bounds and CLI dry-run, passed: worker 3.748s, installation 7.152s in
  `/tmp/deploy-managed-led-final.log`.
- After normalized no-op and local-state preflight refinements, all focused managed,
  LED mapping and fault-clear race tests passed again: worker 20.256s, installation
  48.233s in `/tmp/deploy-managed-led-preflight-race.log`. This includes the actual
  installer SIGKILL matrix. Final native migration integration passed (13.405s) in
  `/tmp/deploy-managed-led-native-final.log`.
- An added native fault-clear regression also passed with race detection (1.249s)
  in `/tmp/deploy-led-clear-native.log`: a changed message in the real mock daemon
  registry preserves its fault and revokes healthy output; restoring exact state
  permits acknowledged cleanup and a fresh bounded healthy lease. This test is
  included in the isolated validation script.
- Vet and Linux ARM64 cross-build passed after the final implementation changes in
  `/tmp/deploy-managed-led-vet-final.log` and
  `/tmp/deploy-managed-led-arm64-final.log`. Cross-building is not native qualification.
- The full `go test -race ./internal/notification/... ./internal/backup -count=1`
  suite passed outside the sandbox: worker 460.145s, backup 157.995s, installation
  55.187s, and every remaining notification package passed. Log:
  `/tmp/deploy-managed-led-packages.log`. It began before the final registry capacity
  guard and preflight refinements, which are covered by the subsequent focused and
  native runs above. Formatting, shell syntax and `git diff --check` passed.

All verification processes have finished. No background tests or implementation
remain running. Changes remain in the existing working tree.

Next implementation remains: recovered LED runtime acceptance/reconciliation and
final recovery publication; shared Signal service/account installation/handoff;
qualified activation; other managed configuration/UID/socket/schema migrations;
shared backup report service installation/cutover/rollback; final outcome/message/
reference/admin-audit compaction; producer retirement. Live providers/external
monitor and actual user-systemd qualification remain outstanding. Native ARM64 and
physical LED testing remain user-deferred. No commit was made.

## Previous checkpoint: absent-producer recovery ownership fencing

Continued on 2026-11-01. Added the pending ownership state needed to keep absent
producers isolated while independent producers reconcile. All three plans remain
incomplete; final recovered-state publication and production activation are still
unavailable. No live services, accounts, Pi or LEDs were changed.

- `notification-worker recovery-fence-producer --local --config … --state-dir …
  --producer …` requires an existing quarantine. It revokes all generations and
  moves exactly one enrolled producer to the new recovery epoch. It creates no
  reconciliation summary, receipt, generation or delivery grant.
- Schema-3 ownership journal events and an optional `recovery_pending` field
  preserve old hashes when absent. Pending ownership blocks normal staging and
  activation. A completed challenged summary is required for the subsequent
  `PrepareRecoveryOwnership` transition; existing generation/retry proof paths
  accept that summary-bound successor. Legacy direct recovery remains supported.
  Older executables reject the new journal schema instead of interpreting it as
  ordinary reconciled ownership.
- Exact interrupted retries finish the durable intent and preserve immutable
  history. Re-fencing after reconciliation has advanced is refused. Missing or
  contradictory owner/journal/history/lease/binding/identity evidence fails closed.
  A later recovery epoch can retain an earlier pending fence in its history.
- Tests cover absent reconciliation, independent producer dispatch and unhealthy
  required-producer status, later summary/generation handoff, old pending intents,
  legacy encodings, corrupt authority, lost acknowledgements and six SIGKILL
  boundaries. The backup adapter also completes a real reconciliation after an
  absent-producer fence and resumes a lost completion reply without discharging
  delivery obligations. The CLI is covered with a built native executable.

Current verification:

- Focused non-race worker/installation recovery tests passed.
- Focused race worker (38.319s) and installation (3.592s) tests passed in
  `/tmp/deploy-recovery-fence-race.log`. Its sandboxed backup portion could not
  create Unix sockets (`setsockopt: operation not permitted`); that superseded
  process was stopped after launching the full suite outside the sandbox.
- Final focused worker (15.538s) and installation (1.506s) race tests passed in
  `/tmp/deploy-recovery-fence-final.log`, including replaced-lock rejection and
  exact resume. Native CLI integration passed (0.952s) in
  `/tmp/deploy-recovery-fence-native.log`.
- Dedicated backup pending-fence/lost-response integration passed outside the
  sandbox (1.668s): `/tmp/deploy-recovery-fence-backup-native.log`. Its initial
  sandbox attempt had the same socket restriction, not a successful test result.
- Full `go test -race ./internal/notification/... ./internal/backup -count=1`
  passed outside the sandbox: worker 451.286s, backup 155.189s, installation
  46.108s, and all remaining notification packages passed. Log:
  `/tmp/deploy-recovery-fence-packages.log`. The later backup pending-fence test
  and replaced-lock test are covered by the dedicated runs above.
- Vet, Linux ARM64 cross-build, formatting and `git diff --check` passed. Logs:
  `/tmp/deploy-recovery-fence-vet.log`, `/tmp/deploy-recovery-fence-arm64.log`.
  Cross-building is not native ARM64 qualification. No test used actual user
  systemd, a live messaging account, a remote host or physical LED hardware.

All verification processes have finished. No background implementation or tests
remain running. No commit was made; changes remain in the existing working tree.

Remaining implementation is unchanged except for the absent-producer ownership
prerequisite: runtime acceptance/reconciliation of recovered LED ownership and final
recovery publication; shared Signal service/account installation and handoff;
qualified activation; other managed schema/configuration/UID/socket migrations;
shared backup report service installation/cutover/rollback; final outcome/message/
reference/admin-audit compaction; producer retirement. Native ARM64 and physical LED
execution remain user-deferred. Live provider/external monitor and actual user
systemd qualification remain outstanding.

## Previous checkpoint: explicit managed schema 9 → 10 migration

Continued on 2026-11-01. Added the managed migration path needed to upgrade older
verification installations without bypassing their schema/identity fences. The
three plans remain incomplete overall; this does not publish recovered state,
activate production or implement arbitrary schema/configuration migrations.

- New `migrate-schema --local --spec …` uses the existing managed lifecycle journal
  for an explicit installed verification schema 9 → 10 upgrade. Identity, resolved
  runtime, state/unit paths, exclusions and producer generations must stay pinned.
  Ordinary updates still reject schema changes; unsupported source versions and
  active/draining producer ownership are refused.
- The new executable and recovery helper are staged/probed before service stop.
  A separate `__managed-migration-probe` requires migration capability version 1;
  the original two-field `__managed-probe` response stays compatible with retained
  older helpers. A schema-compatible binary lacking migration support is rejected
  before the old service stops. The initial helper remains at its original path;
  the compatible new helper lives in an immutable release, identified by
  `helper_revision`/`helper_hash`. Later updates retain that helper.
- Under the installation/worker leases, the worker checks ownership and existing
  state, validates SQLite files/sidecars, checkpoints committed WAL and retains
  the exact original DB in `schema-migrations/<operation>/before.db`. Immutable
  intent pins its size/digest, identity, configuration and versions. Temporary
  copies are bounded; publication is atomic and interrupted copies are never
  accepted as completed backups. Existing state is never recreated.
- Schema migration and its metadata acknowledgement commit in one SQL transaction.
  Independent completion plus a state gate allow exact resume before/after commit;
  ordinary opens reject an unfinished gate. Missing/changed backup, intent, SQL
  acknowledgement or identity fails closed. Read-only completion verification can
  run while the verification worker owns its lease, and checks retained evidence
  before a completed migration resume or starting its new managed revision.
- Unfinished migration must resume before uninstall. Completed migration supports
  ordinary update/uninstall, retains both helpers and the old database, and refuses
  executable rollback across schema/helper changes. There is no automatic database
  downgrade. The older helper cannot resume the new migration journal; use the
  staged helper named in `lifecycle.json` (`Intent.After.helper_revision`).
- Added WAL, authority/backup corruption, pending-gate, capability, helper retention,
  post-migration lifecycle and read-only verification tests. Seven actual worker
  SIGKILL boundaries cover intent, copied backup, published backup, SQL transaction,
  committed acknowledgement, independent completion and gate release; eight
  installer SIGKILL boundaries cover migration and service/file publication.
- Updated README, all three plan status sections, the native installation guide,
  CLI/help and isolated integration script. No live service, provider, remote Pi
  or physical LED was changed; the native tests use temporary processes/sockets.

Verification:

- Final complete managed race suite passed in `/tmp/deploy-schema-final-managed.log`:
  worker 5.287s, installation 34.304s. It includes every nested migration refusal,
  corruption, WAL, completion-inspection and SIGKILL test, plus the existing
  setup/update/rollback/uninstall lifecycle regressions.
- The broader affected-package race suite passed in
  `/tmp/deploy-schema-packages-race.log`: worker 442.324s, installation 43.269s,
  backup 155.124s. This run started before the final read-only completion verifier
  and separate capability probe; the final managed/native tests cover those changes.
- Final native migration integration passed in `/tmp/deploy-schema-native-verified.log`
  (10.789s). A built schema-compatible executable with migration capability disabled
  is rejected before service stop; the supported build migrates, starts a real
  temporary verification process, resumes through the staged helper and supports
  later update/uninstall. The ordinary probe remains readable by an older strict
  decoder. The combined native lifecycle/migration run also passed in
  `/tmp/deploy-schema-native-final.log` (27.993s).
- Final `go vet ./...`, Linux ARM64 cross-build, formatting and `git diff --check`
  passed. Logs: `/tmp/deploy-schema-vet.log`, `/tmp/deploy-schema-arm64.log`.
  Native executable tests use temporary processes/Unix sockets, not actual user
  systemd. Cross-building does not constitute native ARM64 qualification.

Remaining implementation: runtime acceptance/reconciliation of recovered LED
ownership and final recovery publication; shared Signal service/account installation
and handoff; qualified activation; other managed schema/configuration/UID/socket
migrations; shared backup report service installation/cutover/rollback; final
outcome/message/reference/admin-audit compaction; producer retirement. Native
ARM64 and physical LED execution remain user-deferred. Live provider/external
monitor and real user-systemd qualification remain outstanding; the recorded
218/CAPABILITIES host sandbox failure has not been changed or requalified.

## Previous checkpoint: quarantined recovery LED ledger adoption

Continued on 2026-11-01. The preservation record can now be applied to the actual
`led_states` table in `reconciled.db`. This completes the quarantined merge step;
normal-runtime acceptance, runtime-wide reconciliation and final recovery publication
remain unfinished. The three plans are still incomplete and production activation
remains unavailable.

- Added `recovery-led-adopt --local --config … --state-dir … --inventory …
  --inventory-hash … --preservation-hash …`. New adoption requires the exact
  committed preservation, a current sealed inventory, matching pinned ownership/
  layout/UID/source evidence, and a fresh registry check. Dry-run validates inputs
  without opening state or contacting a daemon. Review reports an `adoption`
  summary with exact retry arguments.
- The quarantined ledger keeps the complete snapshot/original union with original
  IDs, bindings, conditions and owner epochs. All executable `obsolete` flags become
  zero; both historical cleanup flags remain in the immutable preservation record.
  Foreign/unrecognized entries remain unowned. A separately cleared fault remains
  retained as ownership history and is never recreated by this operation.
- Independent `led-adoption.json` intent pins preservation/inventory/source hashes
  and the exact before/after ledger digests. Ledger replacement and the singleton
  acknowledgement commit in one SQLite transaction. Reopening rejects missing,
  changed or extra records, lost ledger rows, restored cleanup flags and forged
  checkpoints. It does not silently reconstruct a damaged merge.
- Later reviews and domain evidence use the original immutable source view only
  after checking every current ledger row against the adoption checkpoint. This
  keeps authorized producer cleanup valid across adoption while exposing drift in
  the preserved original database. Cleanup works both before and after adoption;
  adoption hashes cannot replace fresh domain evidence.
- Exact interrupted retries retain the same intent/time after expiry or reboot,
  with no daemon requests. Four actual SIGKILL boundaries cover durable intent,
  row replacement, transaction acknowledgement and committed result. Worker lease,
  quarantine and all command/intake/delivery activation fences remain intact.
- The bounded local adoption operation permits up to 30 seconds; registry I/O
  remains capped at three seconds and the 30-second observation is rechecked before
  intent. A 2,048-fault race test exposed the former ten-second operation budget;
  prepared inserts and the larger local budget allow the complete union to commit
  and reopen without truncation. This is not native ARM64 performance qualification.
- Updated CLI/help, README, all three plan status sections, and the isolated test
  script. No runtime schema version change: adoption metadata belongs only to the
  quarantined reconciliation projection. No original live database, provider,
  deployed service, remote Pi or physical LED was changed.

Verification:

- Final LED recovery race suite passed in
  `/tmp/deploy-led-adoption-recovery-final.log`: worker 148.922s, installation
  1.105s. This includes all four actual adoption SIGKILL boundaries, exact replay,
  cancellation, source/registry drift, stale/rebooted evidence, storage exhaustion,
  ledger/journal corruption, cleanup before/after adoption, and the complete
  2,048-fault merge/reopen/retry/review. The first maximum-union run exposed the
  ten-second budget limit; the final run covers the corrected budget and insert.
- Broader affected-package race suite passed in
  `/tmp/deploy-led-adoption-packages-race.log`: worker 399.467s, installation
  38.362s, backup 154.810s. It started before the final local-deadline/prepared-insert
  adjustment; the complete final LED recovery suite above covers those changes.
- Final pinned mock-daemon/built-CLI race integration passed in
  `/tmp/deploy-led-adoption-daemon-native-final.log`: worker 2.120s,
  installation 2.054s. The native executable test starts with an actual owned
  unknown fault created by the normal LED reconciler against the mock driver.
  Preservation/adoption/replay leave the daemon registry unchanged and normal
  worker startup remains blocked by quarantine. No physical hardware was used.
- `go vet ./...`, Linux ARM64 cross-build, formatting and `git diff --check` passed.
  Logs: `/tmp/deploy-led-adoption-vet.log`, `/tmp/deploy-led-adoption-arm64.log`.
  Cross-building does not constitute native ARM64 qualification.

Next implementation: normal-runtime acceptance of retained old LED identities and
operation history, runtime-wide recovery reconciliation, and the final recovered
state publication coordinator. Other outstanding plan work remains shared Signal
relay/account installation and handoff; qualified activation; managed schema/config/
UID/socket migrations; shared backup report service installation/cutover/rollback;
final-outcome/message/reference/admin-audit compaction; producer retirement. Live
provider/external-monitor and real user-systemd qualification remain outstanding;
physical LEDs and native ARM64 execution remain user-deferred. The recorded host
218/CAPABILITIES sandbox failure has not been changed or requalified.

## Previous checkpoint: durable recovery LED fault preservation

Continued on 2026-11-01. Added the ownership-preservation step needed before a
restored notification worker can be published. The three plans remain incomplete;
this step deliberately does not publish recovered state or grant activation.

- New `recovery-led-preserve --local --config … --state-dir … --inventory …
  --inventory-hash …` freezes the exact proven union of the restored and preserved
  original LED fault ledgers. Original IDs, owner epochs, bindings, conditions and
  both cleanup flags remain intact. Post-snapshot and snapshot-only faults are
  retained. Foreign/unrecognized registry entries remain unowned, and free-form
  daemon messages are represented only by their existing hashes.
- Preparation requires the current sealed inventory, matching pinned layouts and
  runtime UID, unchanged sources and a fresh registry check for each known fault
  instance. It rechecks observation age after schema preparation and immediately
  before publishing the independent intent. Network/socket I/O holds no DB mutex.
  Changed fault instances, stale/rebooted observations, storage exhaustion and
  legacy/unproven ownership fail before intent publication.
- The independent `led-preservation.json` intent and a singleton SQLite
  `reconciliation_led_preservation` acknowledgement pin the entire union to the
  recovery operation, config, source hashes, layout and reviewed inventory. The
  original `led_states` and both source ledgers remain unchanged. One record per
  recovery is bounded to 2,048 proven fault identities and a 4 MiB encoded limit,
  plus normal database/free-space limits. No worker schema version was changed;
  these tables belong only to the separate reconciliation projection.
- Exact retries finish an interrupted acknowledgement without contacting the
  daemon or renewing evidence, including after expiry/reboot. A different inventory
  cannot replace a prepared record. Reopening validates the proof against its
  retained inventory and both journal copies; missing/corrupt/forged or additional
  singleton rows fail closed. `recovery-led-review` now includes a preservation
  summary and exposes source drift even for a committed record.
- Preservation is not domain resolution, a cleanup grant or permission to recreate
  daemon states. Tests attempt cleanup using its digest and confirm rejection.
  The stopped-worker/quarantine boundary stays in place after the process closes.
  Runtime ownership adoption, runtime-wide fault reconciliation and the final
  publication coordinator must still consume and validate this evidence.
- Updated CLI dispatch/help, README, all three plan status sections, and the
  isolated integration script to include the new pinned-daemon and executable tests.

Verification:

- Focused race tests passed in `/tmp/deploy-led-preservation-final-focused.log`:
  worker 15.958s, installation 1.016s. Coverage includes union completeness and
  original epochs, cleanup/activation refusal, source/registry drift, expiry and
  monotonic time, cancellation, storage failure, malformed/forged journals,
  refresh/retry identity, and three actual SIGKILL boundaries (intent, SQL
  transaction and committed acknowledgement).
- Final pinned pi-status-led mock-daemon and built-executable tests passed in
  `/tmp/deploy-led-preservation-daemon-native-final.log`: worker 1.675s,
  installation 1.930s. The real daemon at the pinned offline checkout used its mock
  driver, temporary sockets/state and no physical hardware. Registry contents
  stayed unchanged across preparation and replay. The CLI reports the retained
  record and cannot open the quarantined state as a normal worker.
- The broader worker/installation/backup race run is recorded in
  `/tmp/deploy-led-preservation-packages-race.log` and passed: worker 363.123s,
  installation 37.104s, backup 151.628s. Final focused tests above cover the later
  intent-age and unexpected-row checks. The earlier LED recovery-focused race run passed in
  `/tmp/deploy-led-preservation-focused.log` (worker 72.150s, installation 1.097s).
- `go vet ./...`, Linux ARM64 cross-build, formatting and `git diff --check` passed.
  Logs: `/tmp/deploy-led-preservation-vet.log`,
  `/tmp/deploy-led-preservation-arm64.log`. Cross-building is not native ARM64
  qualification. No live service/provider, remote Pi or physical LED was changed.

Remaining: runtime LED ownership adoption/reconciliation and final recovered-state
publication; shared Signal relay/account installation/handoff; qualified activation;
managed schema/configuration/UID/socket migrations; shared backup report service
installation/cutover/rollback; final-outcome/message/reference/admin-audit
compaction; producer retirement. Live provider/external-monitor qualification and
real user-systemd qualification remain outstanding. Physical LED and native ARM64
execution remain user-deferred. The previously recorded development-host
218/CAPABILITIES sandbox failure has not been changed or requalified.

## Previous checkpoint: resumable Signal service maintenance

Continued on 2026-11-01. The common Signal service coordinator and legacy backup
adapter are now implemented. The three plans remain incomplete overall; this does
not install the shared relay, perform account handoff or enable production activation.

- Added a separate coordinator lease and short account-control lease. A durable
  gate precedes stopping the relay; both relays refuse automatic startup while
  maintenance is prepared/stopped/settled or its publication is pending. The link
  child runs outside the control lease and retains the independent account lease.
- Five immutable phases (`prepared`, `stopped`, `settled`, `released`, `complete`)
  retain the operation/name, exact service binding, prior activity and completed or
  explicitly reviewed link-record hash. The bounded hash-linked journal has its
  own head and at most 64 operations plus 32 JSON temporaries. A one-record pending
  acknowledgement is recoverable only by the exact operation. Missing/corrupt
  outcomes, backward time, reused IDs and exhausted link history fail closed.
  Capacity/ID preflight happens before stopping the service.
- `backup-worker signal-link` probes the selected pinned image for maintenance
  protocol version 2 with no network/account mounts before changing activity. It
  uses the installed configuration/state paths, holds the notifier lease against
  sending/handoff, checks the exact rendered Quadlet and binds current generated
  fragment, image enrollment, configuration and credential fingerprints. Aliases,
  drop-ins, pending manager work, stale definitions and changed inputs are rejected.
  The existing setup coordinator lease excludes concurrent installation changes;
  setup/recovery also reject unfinished maintenance after the coordinator exits.
  Returned status is reread from durable state even after a failed acknowledgement.
  No timer or service enablement is changed. Runtime-added qualification data is
  excluded from comparison with the raw installed configuration.
- Repeating the backup command resumes its unfinished operation and recorded prior
  activity. Lost stop/start/child replies do not replay linking. Uncertain linking
  still requires explicit account review, followed by the same backup command to
  restore activity; review alone cannot bypass the service gate. Default invocations
  also retain the completed operation, preventing a lost final reply from causing a
  second link. A deliberate `--new-link` or fresh explicit `--operation` starts a
  new attempt; retaining an explicit ID makes automation retries exact. These
  options cannot supersede unfinished/pending maintenance. The common coordinator
  is reusable, but its current service adapter manages only the legacy backup relay.
- Added `notification-worker signal-service-status --local --state-dir <account>`;
  it can inspect the maintenance journal while a relay holds its lifetime lease.
  Link status/review results now report the combined account/service startup gate.
  Updated README, both example guides and all three plan status sections.

Verification:

- Full affected-package race suite passed in
  `/tmp/deploy-signal-maintenance-packages-race.log`: signalaccount 4.872s,
  installation 36.628s, relay 41.260s, backup 145.952s. Tests cover active/inactive
  restoration, eleven injected journal boundaries and eleven actual coordinator
  SIGKILL cases, lost replies, changed service authority, malformed manager state,
  incompatible image probes, runtime gating and damaged retained outcomes.
- Native executable integration passed in
  `/tmp/deploy-signal-maintenance-native-final.log` (0.669s). The built command probes
  capabilities, invokes a synthetic failed CLI, exposes both journals, records
  explicit review and resumes coordinator restoration with exactly one child
  invocation. Service-manager activity is a controlled fixture, not live systemd.
- Actual cached pinned AMD64 Signal CLI 0.14.9 Podman/JVM checks passed in
  `/tmp/deploy-signal-maintenance-pinned-container.log`: descriptor inheritance
  passed and the real CLI timeout/uncertainty test passed (5.44s). Containers used
  `--pull=never --network=none`, read-only rootfs and disposable tmpfs with no account.
- Additional account race tests after the exhaustion/reused-ID fix passed in
  `/tmp/deploy-signal-maintenance-final-account.log` (7.356s). Final focused race
  results are in `/tmp/deploy-signal-maintenance-final-focused.log` (account 4.789s,
  installation 1.016s, backup 1.067s). After adding installer serialization and
  recovery guards, the full backup race suite passed again in
  `/tmp/deploy-signal-maintenance-backup-final.log` (145.413s). The final CLI
  completed-retry/new-operation and dry-run flag tests passed under the race
  detector in `/tmp/deploy-signal-maintenance-cli-retry.log` (1.073s).
- Vet and Linux ARM64 cross-build passed; final outputs are recorded in
  `/tmp/deploy-signal-maintenance-final-vet.log` and
  `/tmp/deploy-signal-maintenance-final-arm64.log`. Cross-building is not native
  ARM64 runtime qualification. No live service, provider, remote Pi or LED changed.

Remaining implementation: shared Signal relay/account installation and handoff;
restored LED ownership adoption/runtime reconciliation and final recovery
publication; production activation; managed schema/configuration/UID/socket
migrations; shared backup report-service installation/cutover/rollback; full
outcome/message/reference and administrative-audit compaction; producer retirement.
The recorded user-systemd 218/CAPABILITIES failure remains unresolved. Live
messenger/external-monitor qualification is outstanding; physical LED and native
ARM64 execution remain user-deferred.

## Previous checkpoint: common Signal account linking lease and journal

Continued on 2026-11-01. Fixed a concrete compatibility gap: both relays acquired
`deploy-account.lock`, but the legacy backup linking command bypassed it by
invoking the CLI directly. The full three plans still remain incomplete.

- Added `internal/notification/signalaccount` for the shared account lease,
  bounded link history and native/container linking entry point. Both the shared
  and legacy backup relay now use its startup gate. Account directories must be
  owned/private and accessible without symlinks; the stable lock inode is retained.
- Backup `signal-link` now invokes `/usr/local/bin/deploy notification-worker
  __signal-link` inside the existing digest-pinned image, with an explicit operation
  ID. Older images lacking that entry point fail rather than using an unfenced
  fallback. The actual CLI child inherits the account lease independently of the
  Podman/SSH coordinator. No real account or provider was contacted.
- Intent/head and launch marker precede CLI execution. Exact operation retries
  cannot repeat a possibly executed link. Failures, timeout, missing completion
  and crashes leave the account fenced. Both relays refuse restart until an
  explicit review; a surviving child also prevents inspection/review by retaining
  the lease. Successful CLI exit is recorded as `linked`, not live qualification.
- Added `signal-link-status` and `signal-link-resolve --operation …
  --accept-account-state --reason …`. Review is immutable/idempotent and permits
  use of the inspected account without claiming linking succeeded. It can cancel
  an unlaunched prepared attempt. A different operation/reason is rejected.
  These commands do not stop/start services or send messages; dry-run writes nothing.
- Histories contain at most 64 operations and 32 abandoned JSON temporaries,
  with record digests, prior-operation hashes and a separate intent head. Linking
  URIs/CLI output are never copied into journal/errors. Explicit linking streams
  at most 32 KiB to stdout; the subprocess group gets five minutes plus five seconds
  shutdown grace. Backward time and damaged/missing evidence preserve the fence.
- Added the Signal maintenance runbook, updated CLI help/README/backup examples and
  all three plan status sections. The dedicated notification integration target
  now includes the actual linking/status/review executable test. The pinned
  Signal offline target additionally exercises the real link wrapper with no
  network and disposable tmpfs account state.

Verification:

- Full affected-package race suite passed in
  `/tmp/deploy-signal-link-packages-race.log`: relay 41.176s, signalaccount 2.976s,
  installation 36.441s, backup 146.636s. Final focused race tests passed in
  `/tmp/deploy-signal-link-final-focused.log`: signalaccount 2.947s, installation
  1.017s and backup 1.026s. Tests cover busy leases, exact/conflicting retries,
  review semantics, history capacity, corruption, path/privacy checks, clock
  rollback, output redaction/bounds and six actual SIGKILL scenarios, including
  a CLI child surviving its parent and blocking every competing account owner.
- Native executable integration passed in `/tmp/deploy-signal-link-native-final.log`
  (0.590s). The test uses a synthetic CLI and temporary state, verifies one actual
  child attempt across failure/retry/review, and checks linking output stays out of
  errors. Its initial harness incorrectly used a helper that intentionally discards
  stdout on nonzero exit; the harness was fixed without changing production code.
- Actual pinned Signal CLI 0.14.9 AMD64 Podman/JVM checks passed in
  `/tmp/deploy-signal-link-pinned-container.log`. Image digest remains the lockfile
  digest. Both runs use `--pull=never --network=none`, read-only rootfs and tmpfs,
  with no real account. JVM descriptor inheritance passed; the new real-CLI
  linking test passed (5.43s), preserving uncertainty after bounded shutdown.
  This is not successful live linking or an ARM64 runtime test.
- `go vet ./...` and Linux ARM64 cross-build passed:
  `/tmp/deploy-signal-link-final-vet.log`, `/tmp/deploy-signal-link-arm64.log`.
  No physical LED, live service, provider account or remote host was changed.

Still unfinished: managed Signal relay service/account installation and automatic
stop/restore/handoff around linking (a running account currently fails busy);
restored LED ownership/runtime reconciliation and final recovery publication;
production activation; managed schema/configuration/UID/socket migrations; shared
backup report-service installation/cutover/rollback; final-outcome/message/reference
and administrative-audit compaction; producer retirement. Actual user-systemd
qualification still has the recorded 218/CAPABILITIES failure. Live messenger/
external-monitor tests and user-deferred physical LED/native ARM64 checks remain.

## Latest checkpoint: installation qualification evidence

Continued on 2026-11-01. The three plans remain incomplete; this is an evidence
recording workflow, not production activation or proof of live qualification.

- Added `qualification-template`, `qualify --report …` and `qualification-status`
  for completed local managed verification installations. The current manifest,
  actual unit, retained artifacts and worker identity are checked. Reports pin the
  exact revision, executable/helper/unit and normalized configuration hashes,
  architecture, UID and credential/recipient file digests. No credential contents
  are stored. No worker database, service manager, provider or LED API is called.
- Deterministic checks cover installation/native/systemd/reboot/power-loss/external
  monitoring; each provider, route/fallback and producer; Signal account/relay
  maintenance; LED observation/outage/cleanup; and each configured interactive
  destination/worker/mutation capability. Signal image and LED daemon version/
  configuration fields are explicitly operator attestations, not automatic probes.
- Partial/failed reports remain visible. Passed/failed observations require dated
  external evidence digests, but the CLI cannot establish the truth of that
  separately retained evidence. Reports expire after 30 days, and a changed local
  binding invalidates them. No waiver or skipped-check status can complete a report.
  Both `qualified` and `activation_allowed` stay false, even for complete evidence.
- Added 64-record immutable hash-linked audit with a separate committed head.
  Missing/changed committed evidence fails closed; one unacknowledged tail needs
  an exact report retry. Neither status nor startup auto-acknowledges it. Original
  timestamps/bytes survive retries, including after update/expiry; a historical
  acknowledgement cannot qualify the new revision. Later incomplete/failed/pending
  reports supersede prior successful evidence in status. Dry-run writes nothing.
- Added a detailed `examples/notification-worker/QUALIFICATION.md` runbook and
  updated CLI help, README, installation guide and all three plan status sections.
  Implementation is split into `install/qualification.go`,
  `qualification_journal.go` and `qualification_cli.go`.

Verification in this continuation:

- Focused qualification race tests passed in
  `/tmp/deploy-qualification-race-final.log` (5.519s), including exact retries,
  partial/failed evidence, missing/duplicate checks, timestamps, token/revision/
  UID/architecture drift, pending records across updates, audit corruption and
  capacity, dry-run/lease behavior, configured capability coverage, and two actual
  SIGKILL boundaries. Final clock/unit/uninstall guards also passed
  in `/tmp/deploy-qualification-final-guards.log` (5.630s).
- Full notification installation race suite and CLI package check passed in
  `/tmp/deploy-qualification-install-race.log` (installation 34.032s).
- Native executable integration passed in `/tmp/deploy-qualification-native.log`
  (2.772s), including the actual template/import/status commands alongside the
  running verification child and the existing update/resume/rollback lifecycle.
  It uses temporary credentials/state, an injected manager and offline systemd
  syntax checking. All imported checks are pending fixture evidence.
- `go vet ./...` and Linux ARM64 cross-build passed. Logs:
  `/tmp/deploy-qualification-vet.log`, `/tmp/deploy-qualification-arm64-build.log`.
  ARM64 was cross-built only. The earlier full project race suite and final
  compaction refinements are recorded in the next checkpoint.

Remaining implementation still includes restored LED ownership/runtime domain
reconciliation, final restored-state publication, production activation, managed
schema/configuration/UID/socket migrations, Signal relay/account installation and
handoff/linking, shared backup report-service installation/cutover/rollback,
final-outcome/message/reference and administrative-audit compaction, and producer
retirement. The actual user-systemd sandbox failure, live messenger/external
monitor qualification, deferred physical LED checks and native ARM64 execution
remain unverified. No real notification, live service or hardware was changed.

## Latest checkpoint: receipt compaction and retained executable rollback

Continued on 2026-11-01. The full plans remain incomplete and production activation
remains fenced. No live service, account or LED hardware was changed.

- Added offline `receipts-compact --local --config … --state-dir … --producer …`
  with preview by default, explicit `--apply`, and batches of 1–1,024 rows.
  `worker.receipt_audit_days` defaults to 90 and accepts 90–3,650 days. The omitted
  field preserves existing normalized configuration hashes; storage schema remains
  10 because messages, final outcomes and cursor/replay semantics are unchanged.
- Only intermediate receipts of terminal, fully acknowledged events with no
  in-flight attempt qualify. Both the receipt and latest outcome must exceed the
  retention window. Latest outcome/acceptance evidence, admin audit receipts,
  acknowledgement endpoints, complete immutable messages, sequence/event-ID fences,
  report/incident revisions and chat/command records are retained. SQL date filtering
  is conservative at subsecond boundaries; Go rechecks identities, message hashes,
  sequence, known outcome states and exact timestamps before deletion.
- Deletion and cumulative per-epoch summary commit atomically. `through` is the
  largest discarded position, not a contiguous floor or consumer cursor. Inspection
  exposes holes without advancing listed/acked floors. Startup/snapshot checks reject
  contradictory summaries. Old summaries survive recovery as old-epoch metadata.
  No final-outcome/message/admin-audit compaction or automatic VACUUM is provided.
  Logical freed bytes do not promise filesystem shrinkage, and journal/outcome
  headroom is still required on a full disk. CLI preview performs normal startup
  validation/attempt recovery; global dry-run opens no state.
- Added `rollback --local --root … --from-revision …`. It validates retained
  manifests/artifacts, stages the chosen executable with current runtime JSON and
  publishes a new immutable revision through the existing update journal. State,
  identity, schema, credentials, ownership and recovery helper stay pinned. A full
  expected-manifest hash rejects a concurrent update after review. Same-byte retries
  in verification mode return the current revision without another service restart.
- Rollback requires a completed lifecycle operation. If an update cannot become
  healthy, uninstall cancels it first; rollback can then reinstall a retained
  executable in verification mode without starting the failed candidate first.
  Normal resume/retained helper works without original source inputs. It does not
  cross configuration/schema/helper boundaries or restore database snapshots.
  Private interrupted input staging can remain beside the installation root, but
  has no service authority. Cleanup only unlinks known files, never an unknown tree.
- Updated CLI help, README, administration/installation guides and all three plans.
  Principal implementation files are `worker/receipt_compaction*.go`,
  `install/receipt_compaction*.go`, `install/managed_rollback*.go`; shared lifecycle
  preparation now accepts an internal expected-manifest pin.

Verification in this continuation:

- Final focused race tests passed in
  `/tmp/deploy-compaction-rollback-final-focused.log`: worker 3.920s, install 8.824s.
  Covers retained outcomes, receipt/admin audit provenance, cursor/replay guards,
  partial/unacknowledged/in-flight history, 1,024-row bounds, policy/expiry/clock and
  capacity guards, corrupt evidence, producer isolation, preview/dry-run behavior,
  and fresh/expired native chat contexts. Compaction has three actual SIGKILL
  cases; deletion/audit either both survive or both roll back.
- Rollback has nine injected review/staging/lifecycle interruption cases and seven
  additional actual SIGKILL cases in the managed lifecycle suite (42 total there).
  Tests cover original-input disappearance, current/retained file drift, concurrent
  update, history capacity, repeat requests, pending queue preservation and rollback
  after uninstall cancels an unhealthy update. Earlier targeted run:
  `/tmp/deploy-rollback-final-race.log` (install 8.235s).
- Native executable integration passed in `/tmp/deploy-compaction-rollback-native.log`
  (install 2.943s): actual child verification runtime, distinct executable hashes,
  update/lost acknowledgement/resume/rollback, exact running revision, retained
  message, uninstall, admin CLI and compaction CLI. Manager is injected and systemd
  syntax is checked offline; no real service installation or provider contact.
- Full race suite passed in `/tmp/deploy-compaction-rollback-project-race.log`
  (worker 349.894s, backup 154.141s, installation 32.649s). Final focused worker
  race tests also passed after the conservative cutoff/corruption/WAL-headroom
  refinements (`/tmp/deploy-compaction-headroom-final.log`, 3.746s).
  Vet and Linux ARM64 cross-build passed; `/tmp/deploy-notification-arm64` is a
  cross-build artifact, not evidence of execution on ARM hardware.

Remaining: recovered LED ownership/runtime reconciliation and final recovery
publication; qualified activation; schema/config/UID/socket migrations; Signal
relay/account installation/handoff; shared backup report installation/cutover/
rollback; final-outcome/message/reference and administrative-audit compaction;
producer retirement; live-provider/external-monitor and real user-systemd
qualification. The development host's previous 218/CAPABILITIES sandbox failure
remains unresolved. Physical LED and native ARM64 execution remain user-deferred.

## Latest checkpoint: audited message administration and schema 10

Continued on 2026-11-01. The three plans remain incomplete; production activation
is still fenced. No service was installed, no provider contacted and no physical
LED changed in this continuation.

- Added offline `message-review`, `message-decide`, `message-decision-status`
  commands. They require stopped, existing state under the exclusive worker lease
  and an exact private runtime configuration. Reviews contain the frozen event
  and pin its job/config/ownership hashes, receipt head, epoch, UID, boot, wall and
  monotonic clocks for five minutes. The CLI requires an owned `0600` review file.
  Dry-run validates inputs only and writes nothing. No administrative mutation is
  exposed through the producer Unix API.
- Retry applies only to blocked/ambiguous events, with explicit duplicate-risk
  acknowledgement for ambiguous results. Payload, destination revision, expiry,
  generation and tries remain unchanged. Revoked/draining ownership, changed
  routes and expired/superseded events cannot be revived. Normal dispatch still
  checks current evidence, cooldowns and incident serialization. Cancellation is
  unsuccessful terminal evidence, never proof of delivery or domain resolution.
- Independent file intent precedes one SQL transaction for the queue mutation,
  receipt and audit. A result file records that exact completion. Startup verifies
  all evidence and can repair only a SQL-proven missing result. Pending intents
  never apply on startup. Repeating a committed decision returns its original
  result without reopening an event that has since progressed. History is bounded
  to 1,024 decisions plus 32 atomic-write temporaries; no evidence is evicted.
- Snapshot verification validates administrative receipt provenance. Quarantine
  archives completed decisions under their original epoch before deleting the
  receipt stream. Reconciliation preserves decisions newer than the snapshot from
  the original DB as audit only; it never replays actions or creates receipt
  authority. Original source identity and independent files are checked, including
  when the snapshot predates all decisions. Source fingerprints are per recovery
  operation; rollback/loss/conflicting evidence fails closed. Final recovery
  publication remains unfinished.
- Storage schema is now **10**, fencing older binaries/helpers that ignore audit
  semantics. Compatible unmanaged staging upgrades are transactional. Managed
  runtime/administration rejects implicit schema migration; old managed installs
  require the still-unfinished explicit schema/helper migration. Snapshot schemas
  6–9 remain readable in quarantine. Core version helpers now live in migrations.go.
- Added `examples/notification-worker/ADMINISTRATION.md`, updated README/CLI help
  and notification/interactive plan implementation status. Backup's real producer
  receipt-import test proves administrative cancellation leaves the obligation
  open, generates a replacement, and satisfies it only on replacement acceptance.

Verification in this continuation:

- Focused administration/backup race tests passed in
  `/tmp/deploy-administration-final-race.log`: worker 5.805s, install 1.192s,
  backup 1.313s. Additional recovery crash tests passed in
  `/tmp/deploy-administration-crash-final.log` (worker 6.265s): eight action
  SIGKILL cases plus two audit-reconciliation SIGKILL cases.
- After schema 10, affected administration/migration/snapshot/managed lifecycle/
  backup race tests passed in `/tmp/deploy-administration-final-gates.log`:
  worker 29.717s, install 22.416s, backup 2.943s. Includes fresh-review ownership/
  route/expiry guards, the 1,024-intent boundary, missing latest audit-directory
  refusal, explicit managed schema refusal and unmanaged migration.
- Built executable lifecycle integration passed after schema 10 in
  `/tmp/deploy-administration-native-final.log` (install 2.180s). Extended the same
  integration with real executable message-review/decide/status routing and
  idempotent cancellation; this passed in
  `/tmp/deploy-administration-native-cli-final.log` (install 1.934s). Both use a
  fixture manager, temporary Unix sockets and offline systemd validation; no live
  service. Final foreign-pending-intent startup regression passed with race in
  `/tmp/deploy-administration-startup-final.log` (worker 1.718s).
- Final vet and Linux ARM64 cross-build passed after schema 10:
  `/tmp/deploy-administration-final-vet.log`, `/tmp/deploy-notification-arm64`.
  An ARM64 build is not native ARM64 qualification.
- Full `go test -race ./... -count=1` passed outside the socket-restricted
  sandbox in `/tmp/deploy-administration-project-final.log`: worker 343.715s,
  backup 151.325s, install 25.916s. The final foreign-intent startup refinement
  and subsequently added backup retry test passed separately with race; the latter
  includes both administrative cancellation/replacement and ambiguous retry/import
  in `/tmp/deploy-administration-backup-final.log` (backup 1.517s). Tagged install
  vet and final formatting/whitespace checks passed as well. An earlier run
  `/tmp/deploy-administration-project-race.log` (22555) failed the now-corrected
  superseded-review test assertion; its other packages passed. It predates final
  test/schema fixes and is not final qualification. The sandbox recovery regression
  `/tmp/deploy-administration-recovery-regression.log` passed worker/install;
  backup failed because the sandbox denied temporary Unix sockets (`setsockopt:
  operation not permitted`). The successful final full race run above includes
  those backup cases outside that restriction.

Remaining implementation: restored LED ownership adoption/runtime-wide domain
reconciliation; managed recovery publication/activation; schema/config/UID/socket
migration; Signal relay/account installation/handoff; shared backup report-service
installation/cutover/rollback; receipt/audit compaction and producer retirement.
Live provider/external-monitor qualification and the real user-systemd capability
failure remain separate release gates. Physical LED and native ARM64 execution
remain deferred by the user. Preserve every recovery and activation fence.

Principal new files: `internal/notification/worker/administration*.go`,
`internal/notification/install/administration*.go`,
`internal/backup/notification_administration_test.go`.

## Latest checkpoint: native verification lifecycle and administrative inspection

Continued on 2026-11-01. User requested continued implementation until completion.
The overall three plans remain incomplete; do not call this production-ready.

- Added `internal/notification/install/managed_{types,files,service,lifecycle,cli}.go`:
  local `setup`/`update --spec`, `resume`, `uninstall`, `installation-status` and an
  internal managed runtime/probe. Setup atomically publishes a fully prepared root
  from a private sibling; adopts explicitly initialized inactive worker state;
  pins binary SHA256, raw runtime, normalized config, schema, identity and unit;
  and retains an independent initial recovery helper. Updates currently require
  unchanged configuration, exclusions and schema. Both helper/new binary must
  pass a bounded native capability probe through an already-open executable FD.
- Immutable operation intents/releases plus a current lifecycle journal resume
  through stop, unit/pointer publication, manager reload and start/health checks.
  Missing state/locks, identity drift, unowned units/drop-ins/aliases and changed
  artifacts fail closed. Operation lease spans manager I/O; installation lease is
  exclusive during changes and shared for the entire managed runtime. Exact
  installation revision is now part of managed runtime health. A stale/generic
  verification process with the same worker identity cannot qualify a new release.
- Uninstall can supersede an unfinished setup/update with a bounded, pinned
  cancellation intent. It does not require an otherwise impossible healthy start.
  Interruption of cancellation resumes removal and never starts the superseded
  installation. When an earlier disable/unlink already completed, recovery does
  not reissue disable against an absent unit; an unexpectedly enabled missing unit
  still fails closed.
- Added `worker.OpenManagedVerification`/`OpenManagedRuntime`: expected identity
  checked before recovery writes, and older state is rejected before any implicit
  schema upgrade. Active/draining producers prevent adoption/uninstall. Uninstall
  preserves state, pending messages, secrets, releases, helper and LED fault history;
  it removes only the pinned native unit and makes no LED clear calls. No production
  activation mode exists in this manifest. Rootless user-manager sandbox behavior
  is still unqualified.
- Added queue/receipt inspection APIs and CLI commands, with bounded records/bytes,
  producer/epoch checks and separate inspection pagination. They never update
  consumer listed/acked floors or issue acknowledgement authority. Message text
  is omitted from the queue view. Tests prove inspection cannot authorize an ack
  and normal receipt import still works afterward.
- Added `examples/notification-worker/installation.json` and `INSTALLATION.md`;
  README/help and all three plan status sections now describe this limited mode.
  Extended the isolated notification test script with the native executable test.

Verification in this continuation:

- Lifecycle unit tests pass, including all 36 base injected boundaries, ten
  double-interruption cancellation cases and 35 real SIGKILL cases across
  setup/update/uninstall/cancellation, lost start acknowledgement, failed
  health, source disappearance, changed artifacts, missing DB/locks, active producer
  refusal and lease contention. Focused race run including inspection and dry-run/
  manager parsing: `/tmp/deploy-managed-inspection-race.log`. Final affected-suite
  race run (including cancellation, identity-before-recovery, schema refusal,
  unmanaged-health rejection and verification tests) passed:
  `/tmp/deploy-managed-final-gates.log` (install 19.031s, worker 1.891s).
- Built native executable integration passed after inspection additions and exact
  revision health checks and cancellation of a running interrupted update:
  `/tmp/deploy-managed-native-final.log` (10.646s). Uses an
  injected manager with real worker child processes, temporary Unix sockets and
  offline `systemd-analyze --user verify`. No real service installed/enabled;
  no provider messages or physical hardware touched. This is a tagged test under
  `notificationintegration`, not proof of live manager restrictions.
- Full `go test -race ./... -count=1` passed in
  `/tmp/deploy-managed-project-race.log` (worker 327.072s, backup 140.766s).
  It preceded the final exact-revision/cancellation refinements, which passed the
  affected-suite and native integration runs above. Final `go vet ./...`, tagged
  install vet, Linux ARM64 cross-build, shell syntax and whitespace checks passed.
  Vet logs: `/tmp/deploy-managed-vet.log`, `/tmp/deploy-managed-tagged-vet.log`.
- Added opt-in `TestNotificationRealUserSandbox` with a uniquely named transient
  user-systemd unit, disposable files, a runtime deadline and automatic collection.
  It checks actual state writes, readonly/excluded paths, Unix socket availability
  and denied IP socket creation, without network connections or hardware access.
  The real development-host probe FAILED before execution: `218/CAPABILITIES`,
  `Failed to drop capabilities: Operation not permitted` (confirmed in its exact
  unit journal). Log: `/tmp/deploy-managed-user-sandbox.log`. Explicit private
  user namespaces and a diagnostic variant without the explicit bounding directive
  did not resolve it. No host security policy was changed; the final unit retains
  all restrictions and includes `PrivateUsers=true`. This host is NOT qualified.
  The ordinary integration script does not silently opt into real manager tests;
  the installation guide documents the explicit probe command.

Remaining: final restored LED ownership adoption/runtime-wide reconciliation;
managed recovery publication and qualified activation; schema/config/UID/socket
migrations; Signal relay/account installation/handoff; shared backup report-service
installation/cutover/rollback; administrative retry/cancel/retirement; live provider,
external-monitor and real user-systemd qualification. Physical LED/native ARM64
execution remains deferred by the user. Retain all existing recovery fences.

## Latest checkpoint: guarded LED cleanup, verification runtime and fixtures

Continued implementation on 2026-11-01. The user asked to keep working until done.
The three plans remain incomplete; do not interpret the checks below as production
activation or release qualification. The source tree remains uncommitted.

Implemented in this continuation:

- Added process-local `worker.OpenVerification` and
  `notification-worker serve --local --verification`. Durable producer admission
  and status remain available, but message/reply/command grants, direct chat
  admission and provider intake are blocked. LED output only revokes healthy leases
  and maintains unknown baselines; it does not rewrite fault/tombstone authority.
  Status exposes `verification_only`. Canonical producer ownership is unchanged;
  this is not a durable managed activation fence.
- Added pure `install.RenderVerificationService` with path/argument escaping,
  resource limits, Unix-only networking, private devices, readonly configuration/
  executable, worker-state writes and source-path/container-socket exclusions.
  No unit was installed or enabled. The installer must supply canonical exclusions
  from its inventory and qualify the real user-manager restrictions.

- `protocol.RecoveryLEDCleanupSession` and worker
  `OpenRecoveryLEDCleanupSession` perform one exact named-binding clear while
  recovery remains quarantined. A reviewed inventory/evidence hash, original
  UID/nonce ownership, unchanged source ledgers and producer domain guard are
  mandatory. Registry observations before and after intent persistence reject
  changed state; the final domain/worker checks bound dispatch to current evidence.
  Calls have a ten-second outer deadline and a two-second clear budget. Worker
  aggregate, foreign, unknown and legacy fault states are not eligible.
- Cleanup intents have a linked file head, immutable intent/result files and a
  matching SQLite journal in the reconciliation database. Interrupted intents
  are uncertain and never replayed. Retries return their exact historical result;
  a new attempt needs an explicitly refreshed inventory and a new producer report.
  Acknowledgement loss/cancellation cannot become proof of no send. Restart checks
  history and can finish persistence without dispatch. History is bounded by
  eight inventories times the two 1024-fault source ledgers plus storage budgets.
- Review now exposes cleanup attempt/unconfirmed counts and its last 64 outcomes.
  Acknowledged means daemon acceptance only. Source LED ledgers, producer cursors,
  delivery gates, command grants and activation fences are unchanged.
- Backup's `clearBackupRecoveryLEDFault` retains both stable source leases while
  the worker performs cleanup. Its guard checks the exact locally acknowledged
  report and current ownership, then re-evaluates archive/incident/probe/run/pause/
  qualification evidence in a transaction that ALWAYS rolls back. A changed basis,
  expired lease, reboot or clock regression blocks dispatch. Worker/daemon calls
  never run inside a backup transaction.
- Added synthetic uptime producer evidence and reports under
  `internal/testutil/uptimefixture`. It performs no network probes. Tests exercise
  fresh/expired multi-target evidence, monotonic revisions, explicit retirement,
  independent reports and LED arbitration with a scanner producer. This test-only
  fixture has in-memory counters; recreating it requires a new producer epoch.
- Extracted the pinned offline LED mock-daemon test harness into
  `internal/testutil/ledfixture` under the `notificationintegration` build tag.
  Added worker cleanup integration against the real pinned daemon in mock mode.
  The notification integration script now includes backup LED recovery tests and
  this worker/daemon test. README and all three plans describe the new scope.

Verification completed so far:

- Focused cleanup/backup race tests passed, including seven real SIGKILL boundaries
  for intent, SQL transaction/commit, RPC return and outcome file/transaction/commit.
  `/tmp/deploy-led-cleanup-final-race.log`: worker 24.166s, backup 14.233s.
- Backup integration covers fresh-domain drift, missing receipts/host probes,
  qualification, pause, owner/clock changes, both leases, rollback-only evaluation
  and cancellation/uncertainty. A concurrent session Close now waits for in-flight
  RPC/outcome completion before releasing its worker lease. Worker tests cover registry replacement, source
  drift, short evidence leases, policy evidence, historical replay, corruption,
  explicit refresh and unchanged quarantine.
- Pinned pi-status-led mock-driver integration passed with race instrumentation:
  `/tmp/deploy-led-cleanup-pinned-daemon.log`. Source checkout is the clean local
  `/home/sal/code/pi-status-led` at
  `fbe71f9f6edd108beceb01d568a0b17b5a6eb85c`. The tests used temporary state and
  Unix sockets; they did not contact `rsp4`, a live daemon or a provider.
- The sandbox still blocks Unix `setsockopt`. Automatic review approved scoped
  unsandboxed test runs for local notification/backup tests, the temporary LED mock
  integration and the remaining project tests. These approvals are not authority
  to change live deployments or contact providers.
- The full ordinary project test run passed:
  `/tmp/deploy-project-tests.log`. The full notification/backup race suite passed
  with no socket exclusions (`/tmp/deploy-notification-backup-full-race.log`), as
  did all remaining project packages (`/tmp/deploy-remaining-project-race.log`).
  These runs preceded verification-mode additions; that affected-suite rerun is
  recorded separately below when complete.
- Final cleanup/backup race tests passed after the typed request contract and
  shutdown fix: `/tmp/deploy-led-cleanup-complete-race.log` (worker 24.562s,
  backup 14.011s). Uptime fixture/gateway race tests passed in
  `/tmp/deploy-uptime-fixture-final-2.log`.
- Verification-mode worker, temporary Unix API and native-unit tests passed in
  `/tmp/deploy-verification-final-race.log`; additional direct-intake guard tests
  passed in `/tmp/deploy-verification-final-gates.log`, including producer-activation
  rejection. Final unit exclusions/runtime tests passed in
  `/tmp/deploy-verification-units-final.log`. The pinned real daemon's mock-driver
  suite, including both cleanup and verification mode, passed in
  `/tmp/deploy-pinned-daemon-final.log` (LED 2.284s, worker 2.072s).
  The complete notification race suite also passed with no exclusions:
  `/tmp/deploy-notification-verification-full-race.log`. This run includes the
  verification runtime and existing notification regressions; the final direct
  admission/activation guards and unit-exclusion refinements passed their focused
  runs listed above.
- Final `go vet ./...`, Linux ARM64 cross-build (`/tmp/deploy-notification-arm64`),
  shell syntax and whitespace checks passed after the verification-mode additions.
  Vet log: `/tmp/deploy-final-verification-vet.log`. Native ARM64 execution and
  actual user-systemd sandbox qualification were not performed.

Still outstanding: restored LED ownership adoption/runtime-wide reconciliation;
managed recovery publication and activation; native worker/relay installation and
lifecycle orchestration; managed migration including UID/socket changes; backup
shared report-service installation/cutover/rollback; remaining administrative
lifecycle/receipt/retry paths and live provider/external-monitor qualification.
Physical LED and native ARM64 qualification remain deferred as agreed. No completed
release or production-ready claim is warranted.

## Previous checkpoint: backup adapter for LED recovery evidence

Continued implementation on 2026-11-01. The three plans remain incomplete overall;
production activation is still gated. This checkpoint supersedes the previous
checkpoint's backup-adapter next step.

Completed:

- Added `reconcileBackupLEDRecovery` and its durable source journal. It connects
  backup's actual domain evaluator to `protocol.RecoveryLEDEvidenceSession`, using
  completed producer reconciliation as its authority. Backup ownership must remain
  fenced; both original and already-committed recovery delivery bindings are checked.
- The coordinator holds the stable recovery/notifier leases throughout the exchange
  and rechecks their file identities. Worker API calls run outside SQLite
  transactions. Configuration, epoch, producer, inventory, challenge digest, clock
  and returned proof mismatches fail closed.
- A new challenge evaluates archive evidence, open incidents, qualification,
  independent host-probe validity, run/pause state and retirement. The evaluator's
  status-sequence allocation and incident changes commit atomically with the frozen
  report. It does not freeze/export notification messages or advance delivery
  sequences/cursors. Backup supplies ordinary evidence with no invented policy
  fault revisions or risk-acceptance transitions.
- The worker acknowledgement is recorded separately after exact proof validation.
  Lost replies, cancellation after acceptance and restarts reuse the frozen report;
  importing a historical acknowledgement cannot renew its timestamp or reevaluate
  changed domain state. A fresh inventory requests a fresh evaluation and sees
  newer failures. Missing acknowledged gateway evidence or a remote proof without
  a local frozen report blocks progress.
- Local reports form a bounded hash-linked history with a pinned head and retained
  evaluator snapshots. The limit is eight per recovery, matching inventory rounds;
  there is no history compaction. Damaged rows, missing schema/head/snapshots and
  conflicting receipts fail closed. Ledger schema initialization and its first
  report are in the same transaction. SQLite failures roll back the evaluator too.
- The adapter remains an internal coordinator, not a new production CLI/service.
  It cannot mutate the LED registry, assert physical output, start delivery or
  grant interactive execution. All three plans and README describe this scope.

Principal files:

- `internal/backup/notification_recovery_led.go`
- `internal/backup/notification_recovery_led_test.go`
- `internal/backup/notification_recovery_led_crash_test.go`

Verification:

- Local integration uses a real archived SQLite backup, backup evaluator/producer
  ledger and quarantined notification worker with a fake LED registry. It binds no
  Unix socket and contacts no provider, Pi or live daemon.
- Focused race tests passed, including six real SIGKILL cases at challenge return,
  report transaction/commit, worker acceptance and receipt transaction/commit:
  `/tmp/deploy-backup-led-focused-2.log` (28.771 seconds).
- Broader backup race checks passed for the new adapter plus archive, control,
  bootstrap and retirement regressions:
  `GOCACHE=/tmp/deploy-notification-go-cache go test -race ./internal/backup -run '^TestBackup(LEDRecovery|Archive|AcceptsSmallSQLitePages|ControlRejects|Bootstrap|Retirement)' -count=1`.
  Log: `/tmp/deploy-backup-led-regression.log` (35.578 seconds). This includes
  report-budget exhaustion and cancellation coverage.
- Additional integration after delivery-epoch commit and both-lease/transaction
  checks passed: `/tmp/deploy-backup-led-commit.log` (3.115 seconds).
- Related notification-worker recovery race tests passed (160.381 seconds):
  `GOCACHE=/tmp/deploy-notification-go-cache go test -race ./internal/notification/worker ./internal/notification/protocol -run 'TestRecoveryLED|Test.*Recovery' -skip '^(TestInteractiveUnixHandoffAndLedger|TestUnixRoundTripAndIdentityFence)$' -count=1`.
  Log: `/tmp/deploy-backup-led-worker-regression.log`. The protocol package compiled
  but had no tests matching that selection. This was not the full notification suite.
- Final `go vet ./...`, Linux ARM64 cross-build, formatting and `git diff --check`
  passed. Full backup socket/VM acceptance and native ARM64 runtime qualification
  were not attempted. Physical LED testing stays deferred.

Next work: journaled exact LED ownership adoption/cleanup with fresh registry and
domain revalidation; runtime-wide fault reconciliation; managed recovery command
orchestration and activation. Other specialist adapters, managed migration services,
UID/socket changes, backup report-service installation/ownership migration,
lifecycle/receipt/retry administration and live qualification remain outstanding.
No production deployment, provider or physical LED was changed.

## Previous checkpoint: challenged LED domain evidence and cleanup proposals

Continued implementation on 2026-11-01. The plans remain incomplete overall;
production activation is still gated. This checkpoint supersedes the earlier
inventory checkpoint's immediate next steps.

Completed:

- Added the shared `protocol.RecoveryLEDEvidenceSession` contract and internal
  `worker.OpenRecoveryLEDEvidenceSession` implementation. A producer challenge pins
  the recovery operation/epochs, sealed inventory, both ownership ledgers, producer
  domain histories and previous LED reports. It requires an enrolled producer and
  a fresh inventory, and expires after ten minutes without renewal on retry.
- Fresh full snapshots must advance sequence beyond both preserved/restored status,
  completed producer reconciliation and previous LED reports. Basis, fault and
  acceptance revisions cannot regress; configuration changes and inconsistent
  support identities fail closed. Reports are bounded to a full protocol snapshot,
  128 transitions and a combined payload limit. No provider/network endpoint for
  arbitrary proof submission was introduced.
- Read-only domain collection retains subject latches, policy fault incarnations,
  support/dependencies and completed recovery status. Changes invalidate a challenge
  or its proposal. Exact policy transitions are tied to the challenged snapshot's
  sequence/basis and must carry current acceptance support when applicable.
- `recovery-led-review` now includes `resolutions`. Named bindings need complete
  fresh selected-subject coverage, resolution/retirement and exact policy transitions
  for retained fault revisions. Omission, pause, ongoing faults, unknown ownership,
  expired evidence and changed mappings cannot produce cleanup targets. Successful
  proposals identify exact fault IDs, evidence hash and a capped validity deadline.
  The worker aggregate stays blocked on runtime reconciliation.
- Prior acceptance dependencies survive inventory refresh. A different acceptance
  cannot erase an older dependency; explicit domain resolution can discharge it.
  Same-boot monotonic observations impose a lower bound on evaluation time across
  refreshes, so a wall-clock stall/rollback cannot extend short evidence leases.
  Old challenges/reports after reboot remain historical evidence only.
- Separate immutable challenge/report file intents and matching SQLite rows provide
  idempotent crash recovery. Pending records finish with their original timestamp;
  conflicting retries, missing files/schema and altered history fail closed.
  Startup validates the history. The eight inventory rounds, enrollment and storage
  budgets bound retention; no compaction is implemented.
- This is preparation only. No LED mutation, healthy lease, ownership adoption,
  normal status publication, producer activation or command grant occurs. Physical
  testing remains deferred. Producer-specific adapters and managed orchestration
  have not yet been connected to this exchange.

Principal files: `internal/notification/protocol/recovery_led.go`;
`internal/notification/worker/recovery_led_{domain,evidence,resolution}.go`;
`recovery_led_evidence_test.go` and `recovery_led_evidence_crash_test.go`.
The existing inventory review and recovery initialization now validate and expose
this evidence. README and both notification/interactive plans were updated.

Verification:

- The notification-package race suite passed (worker 297.366 seconds):
  `GOCACHE=/tmp/deploy-notification-go-cache go test -race ./internal/notification/... -skip '^(TestInteractiveUnixHandoffAndLedger|TestUnixRoundTripAndIdentityFence)$' -count=1`.
  Log: `/tmp/deploy-led-evidence-full-race.log`. Exactly those two known
  sandbox-blocked Unix socket tests were excluded.
- Final focused race tests passed after the monotonic-expiry and completed-producer
  reconciliation checks were added (25.557 seconds):
  `/tmp/deploy-led-evidence-final-focused-2.log`.
- Six real SIGKILL cases cover challenge and report filesystem intent, database
  transaction and committed boundaries. Restart preserves timestamps and identity,
  can finish historical persistence after reboot/expiry, and grants no LED action.
- Tests also cover exact policy transitions, risk acceptance/support expiry,
  dependency preservation across refreshes, incomplete subject coverage, explicit
  retirement, sequence/basis/configuration/fault regression, changed domains,
  corruption, conflicting replay, payload bounds and quarantine preservation.
- Final `go vet ./...`, Linux ARM64 cross-build, formatting and `git diff --check`
  passed. These are local fixture tests, not real daemon/hardware, native ARM64
  runtime or full-project deployment qualification.

Next work: connect specialist/backup producer evaluation to the new exchange;
implement journaled exact LED ownership adoption/cleanup with fresh registry and
domain revalidation; reconcile runtime-wide faults; finish managed publication and
activation. The older pending work (managed migration services, UID/socket changes,
backup report-service installation/ownership migration, lifecycle/receipt/retry
administration and live qualification) remains outstanding. No Pi, actual daemon,
provider or deployment was contacted or changed.

## Previous checkpoint: durable LED recovery inventory and review

Continued implementation on 2026-11-01. The plans remain incomplete overall;
production activation remains gated. This checkpoint supersedes the next-step
list in the earlier binding-migration checkpoint below.

Completed:

- Added `notification-worker recovery-led-review --local --config <runtime>
  --state-dir <state> [--refresh]`. It requires an existing quarantine, an explicit
  LED socket and the exclusive worker lease. It cannot initiate restore or stop a
  service. Dry-run validates configuration without inspecting state or contacting
  the daemon.
- The inventory combines the restored reconciliation projection, the preserved
  original worker database and read-only daemon status/list observations. It retains
  post-snapshot and snapshot-only faults, exact old identity/epoch evidence, cleanup
  intent differences, operation/mapping history digests and separate mapping
  revisions. Foreign/unrecognized daemon states are preserved. Legacy/schema-6
  fault records never acquire exact epoch ownership. Invalid ownership histories,
  incompatible source configurations and missing source databases fail closed.
- A narrow observer interface has no LED mutation methods. Free-form daemon
  messages are hashed rather than stored or displayed. Reviews report target
  conflicts, source drift and UID/mapping agreement. They are diagnostic evidence:
  `clear_allowed`, `activation_allowed` and `visual_verified` always remain false.
- Inventories live only in the quarantined reconciliation projection, with an
  immutable file per capture, linked filesystem head and matching SQLite history.
  A pending capture resumes with the same identity, source evidence and original
  observation age. Reopening recovery validates the retained history. No worker
  schema bump or automatic source-ledger mutation was introduced.
- Default reads reuse the sealed observation without another daemon call. Explicit
  refresh permits up to eight inventories, within existing disk/database budgets.
  Pending captures are sealed first even when refresh is requested. Evidence is
  current only while sources match, the boot matches and wall/monotonic elapsed
  time stays below 30 seconds. Reboot, clock rollback, source drift or expiry cannot
  renew it. Daemon calls hold no database mutex; sources and time are checked again
  before writing capture intent.

Principal files:

- `internal/notification/worker/recovery_led_source.go`
- `internal/notification/worker/recovery_led_view.go`
- `internal/notification/worker/recovery_led_inventory.go`
- `internal/notification/worker/recovery_led_test.go`
- `internal/notification/worker/recovery_led_crash_test.go`
- `internal/notification/install/recovery_led.go` and its CLI tests

Existing fault-identity validation is shared with the inventory reader. Recovery
initialization validates any inventory history before admitting another session.
README and both notification/interactive plans describe the command and its limits.

Verification:

- Focused worker/install race tests passed (worker 13.255 seconds):
  `/tmp/deploy-led-inventory-focused.log`.
- Five real SIGKILL boundaries passed: schema publication, filesystem intent,
  immutable archive, before database commit and after commit. Restart seals the
  same capture exactly once without renewing it or relabeling fault ownership.
  These use a fake daemon; they do not qualify pi-status-led or physical hardware.
- Additional cases cover foreign/unknown states, post-snapshot faults, cleanup
  differences, schema-6 provenance, migration A→B→A revisions, source changes during
  RPC, expiry, reboot, clock failure/rollback, corrupt rows/files/epochs, capacity,
  history limits, explicit refresh/socket pinning, dry-run, quarantine and lease
  refusal. The normal worker remains fenced throughout.
- The notification-package race suite passed (worker 277.925 seconds), excluding
  exactly the two known sandbox-blocked Unix socket tests:
  `GOCACHE=/tmp/deploy-notification-go-cache go test -race ./internal/notification/... -skip '^(TestInteractiveUnixHandoffAndLedger|TestUnixRoundTripAndIdentityFence)$' -count=1`.
  Log: `/tmp/deploy-led-inventory-full-race.log`.
- After strengthening the crash test to request refresh during pending capture
  recovery, all five SIGKILL cases passed again (3.007 seconds):
  `/tmp/deploy-led-inventory-crash.log`.
- `go vet ./...`, Linux ARM64 CLI cross-build, formatting and `git diff --check`
  passed. The two excluded socket tests, actual daemon/hardware behavior, native
  ARM64 runtime and full-project deployment acceptance were not qualified here.

Next work: use fresh producer reconciliation/domain-resolution proof to prepare
exact restored LED ownership adoption/cleanup, preserving all newer and unknown
faults; add journaled LED recovery operations and managed publication/activation.
The review does not supply any of that authority. Managed service orchestration
for binding migration, UID/socket migration, backup report-service installation/
ownership migration, remaining lifecycle/receipt/retry administration and live
qualification remain pending. No actual daemon, provider, Pi or deployment was
changed. User manual physical LED testing remains deferred.

## Previous checkpoint: explicit local LED binding migration

Continued implementation on 2026-11-01. The notification/interactive/backup plans
remain incomplete overall; production activation is still gated.

Completed:

- Schema 9 and `notification-worker led-migrate --local --config <old-runtime>
  --next-config <new-runtime> --state-dir <state>` support explicit local binding
  changes. The command takes the existing worker lease and refuses a live worker.
  Only LED bindings may differ; runtime UID, socket and all other settings stay fixed.
- Changed or removed bindings must first have no fault or cleanup ledger rows.
  Operators resolve/retire them through fresh producer evidence under the old
  runtime. Migration cannot clear fault IDs or use a config edit as resolution.
  Faults on unchanged bindings and unfamiliar daemon states are preserved.
- Targets, patterns, namespace collisions, registry quotas and storage reserve are
  checked before fencing/mutation and rechecked during progress. The journal
  revokes old healthy leases, removes changed old baselines, and installs new
  unknown baselines. It never sets health or enables/configures hardware.
- Independent filesystem/database fences prevent normal startup during migration.
  Resume requires the exact old/new configurations and socket, and uses the same
  operation identity. RPC intent/outcome survives interruption; a received ACK is
  committed even if the caller is cancelled. No DB mutex is held during daemon I/O.
- If the daemon loses acknowledged baselines before publication, resume rewinds
  only baseline sets. It never replays an old baseline clear after that ID was
  moved to a new target. A completed command retry does not repeat RPCs.
- Mapping publication atomically archives the previous mapping and retained LED
  operation history, then publishes a new mapping revision and fresh active journal.
  Returning to an earlier config gets a distinct revision. `led_operations` status
  includes `layout_hash` to disambiguate counters across mapping changes.
- Startup, snapshot and offline recovery validate mapping chains and archived
  acknowledgements. Missing archives/links, tampering and premature publication
  fail closed. Schema-6/7/8 snapshots remain readable, with original recovery-epoch
  provenance preserved in quarantine. Schema-8 upgrades validate the old mapping
  and journal before committing transactional schema-9 DDL.
- Migration history is bounded to 16 transitions plus normal disk/database limits.
  Further transitions are rejected before LED mutations; archive compaction is not
  implemented. Dry-run validates only configs and says that state/daemon inspection
  has not occurred. The command does not stop/restart services or change runtime files.
- README and both notification/interactive plans include the command, prerequisites,
  resume procedure and limits.

Principal files:

- `internal/notification/worker/led_migration.go`
- `internal/notification/worker/led_migration_daemon.go`
- `internal/notification/worker/led_migration_history.go`
- `internal/notification/worker/led_migration_test.go`
- `internal/notification/worker/led_migration_crash_test.go`
- `internal/notification/install/led_migration.go` and its CLI tests

LED layout/operation validation, schema migration, normal startup, snapshot and
recovery validation were updated alongside them.

Verification:

- Focused race tests covering LED behavior, migration, snapshots and CLI checks
  passed: `/tmp/deploy-led-migration-all-focused.log`.
- Eleven real SIGKILL cases passed: filesystem intent, database preparation,
  pre-commit/post-commit/unfence, and intent/RPC/ACK boundaries for both clear and
  set operations. The test daemon is a durable file-backed fixture; these are not
  tests of the real pi-status-led daemon or physical hardware.
- Tests also cover lost replies, cancellation, daemon state loss, unchanged-fault
  preservation, unresolved-fault refusal, completed-command retries, corrupt archives,
  schema rollback, config/socket drift, foreign namespaces, storage/registry limits,
  the 16-transition cap, dry-run and live-worker lease refusal.
- The complete notification-package race suite passed with the two known
  sandbox-blocked Unix socket tests excluded:
  `GOCACHE=/tmp/deploy-notification-go-cache go test -race ./internal/notification/... -skip '^(TestInteractiveUnixHandoffAndLedger|TestUnixRoundTripAndIdentityFence)$' -count=1`.
  Log: `/tmp/deploy-led-migration-full-race.log` (worker: 269.343 seconds).
- After the final live-publication check was added, focused LED/migration/snapshot
  and CLI race tests passed again: `/tmp/deploy-led-migration-final-focused.log`
  (worker: 29.600 seconds). Live reconciliation rejects unpublished layout revisions
  before daemon RPCs; startup/snapshot validation is not its only check.
- Final `go vet ./...`, Linux ARM64 CLI cross-build, formatting and `git diff --check`
  passed. No full-project deployment acceptance or native ARM64 runtime qualification
  was performed. The two excluded socket tests remain unverified in this sandbox.

Next work: restored LED ownership reconciliation and managed recovery publication/
activation; managed service orchestration for LED migration; UID/socket migration;
backup report-service installation and ownership migration; remaining lifecycle,
receipt/retry administration and live qualification. User manual physical LED tests
remain deferred. No real daemon, provider, Pi or deployment was changed this turn.

## Previous checkpoint: durable LED operation journal

Continued local implementation on 2026-11-01. Overall plan implementation remains
incomplete; managed production activation is still gated.

Completed:

- Schema 8 adds an independent LED operation journal. Every set/clear commits an
  intent before RPC, including the worker/recovery identity, pinned mapping digest,
  binding, exact state ID/ownership and supporting evidence digest where required.
  Completion records acknowledged, uncertain or not-sent outcome. A crash before
  completion leaves a pending attempt, which may already have reached the daemon.
- Healthy and pause leases are recomputed after durable intent commit and fresh
  evidence checks. The receipt records the actual dispatched state/TTL; its TTL can
  only shorten the intent's upper bound. Cancelled or changed evidence does not
  dispatch a queued intent. A concurrent source change remains a conservative
  deferral; failure to clear the old healthy lease still propagates as an error.
- RPCs execute outside database transactions. Store-level serialization prevents
  two reconciler instances from interleaving mutations. Received acknowledgements
  have a separate bounded completion context so cancellation does not discard them.
  Journal write failure prevents dispatch, or leaves an already-sent operation
  pending if its acknowledgement cannot be committed.
- Each binding retains 64 attempts and a separate copy of its latest acknowledged
  operation. Explicit pruned-through and pruned-unconfirmed counters report missing
  history. This is bounded audit history, never a replay queue. Exact fault
  tombstones and fresh evidence remain necessary for destructive cleanup.
- `system.status` exposes `led_operations`: issued/pruned revisions, retained pending
  and uncertain counts, and the latest historical acknowledgement. These diagnostics
  never renew source health, registry observations or physical qualification.
- Startup, snapshots and offline recovery validate journal structure, digests,
  ownership, mapping, epoch, state constraints and acknowledgement consistency.
  Schema-7 upgrades atomically start with empty history; no earlier acknowledgements
  are invented. Schema-6/7 snapshots remain readable. Restored schema-8 history keeps
  its original epoch in quarantine, and normal startup remains fenced.
- README and both notification/interactive plan status sections describe the new
  implementation and its remaining limits.

Principal files: `internal/notification/worker/led_operations.go`,
`led_operations_history.go`, `led_operations_test.go`; LED reconciliation, schema
migration, status and snapshot/recovery validation were updated alongside them.

Verification in this continuation:

- Focused worker race tests for LED behavior, migration and snapshots passed.
- Tests cover lost set/clear acknowledgements, source changes and expiry after
  intent commit, shortened TTLs, cancellation, failed journal writes, bounded
  retention, history corruption, transactional upgrade rollback, schema-6/7
  snapshot compatibility and old-epoch recovery provenance.
- Six real SIGKILL cases cover set and fault-clear operations at intent commit,
  RPC return and acknowledgement commit. Restart does not replay old healthy
  assertions or fault destruction; fresh evidence permits exact tombstone cleanup.
- The complete notification-package race suite passed with only the two known
  sandbox-blocked Unix socket tests excluded:
  `GOCACHE=/tmp/deploy-notification-go-cache go test -race ./internal/notification/... -skip '^(TestInteractiveUnixHandoffAndLedger|TestUnixRoundTripAndIdentityFence)$' -count=1`.
  Log: `/tmp/deploy-led-operations-final-race.log` (worker: 249.854 seconds).
- After the final cleanup-error propagation refinement, focused LED/runtime/health/
  status and install-supervision race tests passed again. Log:
  `/tmp/deploy-led-operations-deferral-final.log`. Status tests confirm that an old
  healthy-set acknowledgement cannot establish current health after restart.
- Final `git diff --check` passed. The two excluded socket tests remain unverified
  in this sandbox; this was not a whole-project or deployment acceptance run.
- `go vet ./...` and Linux ARM64 CLI cross-build passed. Cross-compilation is not
  native runtime qualification. No real Pi/daemon, live provider or VM qualification
  was performed; physical LED testing remains deferred to the user.

At that checkpoint, next work included explicit binding migration (now implemented
for local staging above), restored ownership reconciliation and managed recovery
publication/activation; backup report-service installation and
ownership migration; remaining lifecycle, receipt/retry administration and live
qualification. The operation journal is not proof that restored LED ownership is
safe to activate, and its pruned history is not a complete lifetime audit trail.

## Previous checkpoint: configurable LED bindings

The staged notification worker now supports `worker.led_bindings`. Implementation
is still incomplete overall; production activation remains gated.

Completed in this continuation:

- Up to 32 bindings select one enrolled producer and exact subject keys or a sole
  `*`; configuration allows at most 512 selector entries. Each binding has separate
  baseline/healthy IDs and independently owned fault incarnations. Source conditions
  and per-binding output observations are exposed in status.
- Named patterns and priorities are configurable within severity bands. The daemon
  must advertise each configured pattern. Red/PWR requires explicit `allow_power`
  and an already enabled daemon target. No daemon or hardware configuration is changed.
- Schema 7 adds binding and full fault identity to the LED ledger, plus a stored
  normalized mapping/UID pin. Mapping changes, removed pins and runtime UID changes
  fail closed. Older binaries refuse schema 7. Configuration edits cannot silently
  reuse old state ownership; explicit managed binding migration is still pending.
- Existing fault IDs migrate into an explicit legacy inventory. Migration DDL,
  inventory, pin and schema version commit together. Failed migration rolls back.
- Resolution and retirement clear only that binding's exact old fault IDs. New
  episodes receive new IDs. Lost clear acknowledgements preserve tombstones, and
  restart cannot retry their destruction while source evidence is unknown/paused.
- Pause indications expire with supporting evidence. Fresh retirement clears old
  faults without a healthy lease. Successful runtime-loop observations do not turn
  retirement into health. Recorded retirement survives later fresh producer reports.
- Reconciliation has a ten-second pass budget and resumes at the next binding if
  the budget expires. A missing pattern on one binding does not stop other outputs.
- Wildcard selection is bounded to 128 active subjects for healthy coverage;
  overflow is explicit and known critical faults remain visible. Status repeats
  only bounded subject-key summaries with omission counts, rather than duplicating
  complete subject details for every binding.
- Schema-6 snapshots remain readable and recover into quarantine. Schema-7 snapshots
  preserve mapping pins and old fault identities; restore does not relabel them as
  fresh recovery-epoch authority. No recovery activation was enabled.

Principal new files:

- `internal/notification/worker/led_binding.go`
- `internal/notification/worker/led_ledger.go`
- `internal/notification/worker/led_runtime.go`
- `internal/notification/worker/led_binding_test.go`
- `internal/notification/worker/led_binding_recovery_test.go`
- `examples/notification-worker/led-runtime.json`

Store/status, schema migration, snapshot/recovery validation, the reconciler,
runtime configuration validation and related fixtures were updated. Both plans
and README describe the new staged configuration and its remaining limits.

Verification:

- Focused race tests for bindings, runtime health, migration, snapshot, policy and
  status passed. Coverage includes independent resolution, retirement, restart,
  lost clear acknowledgement, corrupted fault identities, UID/mapping drift,
  unavailable patterns, 32 bindings, wildcard bounds and legacy snapshot recovery.
- The first full notification-package race run passed with only the two previously
  sandbox-blocked Unix socket tests excluded. Its log is
  `/tmp/deploy-led-bindings-full-race.log`.
- After the final status/retirement refinements, the complete notification-package
  race run passed again:
  `go test -race ./internal/notification/... -skip '^(TestInteractiveUnixHandoffAndLedger|TestUnixRoundTripAndIdentityFence)$' -count=1`.
  Log: `/tmp/deploy-led-bindings-final-race.log`. The two excluded Unix socket tests
  remain unverified in this sandbox; this is not a complete project acceptance run.
- Final `go vet ./...`, Linux ARM64 CLI cross-build, example JSON parsing and
  `git diff --check` passed. No live provider, Pi, native ARM64 or real mock-daemon
  qualification was attempted again in this continuation.

At that checkpoint, next work included explicit binding migration, LED operation
history, restored ownership reconciliation and managed activation. Operation history
is now implemented as recorded above; the remaining gates still apply.

## Previous checkpoint: runtime supervision and registry observation

Completed:

- Fixed LED reconciliation starvation during successful interactive polling.
  Runtime health now carries a revision that changes on failures, recovery and
  stalled-loop renewal. LED checks permit deadline-only renewal, preserve their
  original lease deadline, and reject changed producer evidence or loop revisions.
  Successful polling cannot conceal clock rollback.
- Moved staged service orchestration out of `install/cli.go` into
  `internal/notification/install/runtime.go`. The runtime cancels and joins all
  services on fatal exit; an unexpected clean service exit is also an error.
- Added a concurrent test using the real worker database and dispatcher: a report
  is durably accepted while intake is in backoff, and health retains the outage.
  Added shutdown/join and cancelled-start tests.
- Added LED registry capacity checks before new fault/healthy states, verification
  of the baseline and exact acknowledged state, and a cross-check between listed
  arbitration and `status.get`. Missing/changed states, daemon restart, degraded
  status and inconsistent observations stop healthy renewal and attempt to clear
  the lease (unconfirmed clears still rely on bounded expiry). Other applications'
  states are preserved, including winning states and quota usage.
- Added optional `health.led` output diagnostics for registry acceptance,
  suppression and observation expiry. They expire after at most 30 seconds,
  remain separate from producer health, and never claim physical rendering or
  qualification. Required-output activation policy remains unfinished.
- Bounded an entire paginated LED listing by one latency budget; incomplete,
  duplicated, reordered and invalid pages are rejected. Tests exercise actual
  production JSON framing/deadlines over an in-memory transport. This avoids
  requiring sandbox-forbidden socket setup for protocol unit tests.
- Updated both notification/interactive plan status and README. The stale claim
  that ordinary interactive polling errors stop the staged runtime is removed.

New files in this session:

- `internal/notification/install/runtime.go`, `runtime_test.go`
- `internal/notification/worker/led_health.go`, `led_runtime_test.go`
- `internal/notification/worker/led_observation.go`, `led_observation_test.go`
- `internal/notification/led/list_test.go`

Also changed runtime health, LED reconciliation/client, store/status and related
tests. No configurable binding or managed activation feature was enabled.

Verification in this resumed session:

- Focused race tests covering LED, runtime supervision/group, registry pagination,
  reply ambiguity and lease clamping passed.
- `go vet ./...` and Linux ARM64 CLI cross-build passed against the new code.
- `git diff --check` passed (untracked files still require separate inspection).
- A broad race run with the two previously known socket tests excluded failed:
  many backup/gateway and vulnerability-fixture tests also require Unix sockets;
  systemd verification was denied socket options. Deployment tests could not
  write locks under `/home/sal/.cache/deploy`; a concurrency test then timed out.
  Full output is `/tmp/deploy-resume-race.log`. This is not a full-suite pass.
- The clean pinned LED checkout is present at `/home/sal/code/pi-status-led`,
  commit `fbe71f9f6edd108beceb01d568a0b17b5a6eb85c`. Its API/security/model sources
  were inspected. `TestPinnedMockDaemon` was attempted and failed at daemon startup
  with `Operation not permitted` inside the sandbox. No physical LEDs or services
  were changed. Earlier authorization was not used to run new unsandboxed tests.
- The four affected deployment tests passed when rerun with the race detector and
  `XDG_CACHE_HOME=/tmp/deploy-resume-test-cache`, including the previously timed-out
  concurrency test. This rerun did not exercise the entire deployment package.
- The final notification-package race run passed against the new code:
  `go test -race ./internal/notification/... -skip '^(TestInteractiveUnixHandoffAndLedger|TestUnixRoundTripAndIdentityFence)$' -count=1`.
  The two explicitly excluded Unix socket tests remain unverified in this sandbox.
  Output is `/tmp/deploy-resume-notification-race.log`.

Next implementation work remains configurable LED bindings, recovery-safe LED
ownership reconciliation, managed recovery publication/activation and the backup
report-service/ownership migration. Consult all three detailed plans for the
remaining integration gates; the overall implementation is still incomplete.

## Original interrupted-session handoff

## Overall status

The notification and interactive worker plans are **partially implemented, not
production-ready**. Significant local contracts, durable recovery machinery,
staged CLI support and regression tests exist. Managed installation and recovery
activation still intentionally refuse incomplete candidates. Do not remove those
guards just to make setup succeed.

The governing plans are `NOTIFICATION_WORKER_PLAN.md`,
`INTERACTIVE_WORKER_PLAN.md` and `BACKUP_WORKER_PLAN.md`. This handoff records the
latest implementation session; it is not a claim that every plan item is done.
The vulnerability scanner and uptime worker remain future projects; a synthetic
vulnerability fixture is not an implemented scanner.

## Completed in the latest session

### Signal recovery across retained ranges and repeated restores

- Added canonical retained-history ranges to Signal recovery manifests, with a
  maximum of 32 ranges. Legacy manifests keep their previous serialized shape.
- Recovery pagination now uses retained-event ordinals while actual event
  positions retain their original high-water marks and explicit missing gaps.
- Updated relay export, restored-source exchange, gateway import, handoff and
  cursor staging to support an already recovered relay and subsequent recoveries.
- Added snapshot Version 3 for gapped historical prefixes; Version 2 remains
  supported for the original contiguous historical prefix.
- Updated digest verification to distinguish retained history from the fresh
  contiguous tail. Historical records do not become executable input.
- Runtime schema 5 still requires the external `signal-runtime.json` pin and
  matching database proof. Schema 4 cursor candidates and schema 6 restored
  quarantine remain inert until managed publication is implemented.
- Added five successive recovery cycles, retained/restored-source exchange,
  lost-response replay, corruption checks, maximum-range proof-budget coverage
  and ten additional baseline cursor SIGKILL cases.

Principal files: `internal/notification/interaction/signal_history.go`,
`signal_recovery.go`, `signal_recovery_exchange.go`;
`internal/notification/relay/recovery_history.go`, `recovery*.go`,
`restore_exchange.go`, `snapshot*.go`, `restore_baseline.go`;
`internal/notification/worker/recovery_signal*.go` and their tests.

### Gateway evidence when relay history is missing

- Recovery request sealing can use a preserved gateway inbox row only when its
  Signal position belongs to an explicitly declared missing range or restored
  suffix. A missing retained event cannot be excused using this fallback.
- The fallback verifies the immutable `candidate.db` hash, rejects SQLite
  sidecars and checks exact original row identity, body, hash and quarantine state.
- Opaque/redacted missing rows contribute evidence and a `MissingSignalRows`
  count without inventing request identities or granting execution authority.
- Added tests for valid full/opaque evidence, changed bodies and hashes, injected
  identifiers, invalid gaps, changed snapshots and unexpected sidecars.

Principal files: `internal/notification/worker/recovery_request_snapshot.go`,
`recovery_request_inventory.go`, `recovery_request_seal.go`,
`recovery_signal_baseline_test.go`.

### Independent interactive loop supervision and health

- Telegram intake, Signal intake, command conversion and command replies now
  run under separate supervisors in the staged CLI runtime.
- Ordinary loop errors retry with exponential backoff of 1, 2, 4, 8, 16 and
  30 seconds, capped at 30 seconds. A successful step resets the backoff.
- Added registered loop health with a 90-second success deadline. Errors,
  cancellation, stalls and uncertain clock state remove positive health.
- Raw provider errors are neither exposed in loop health nor logged by the
  supervisor; logs report generic degraded/recovered transitions.
- Ambiguous or blocked command replies now affect worker health.
- Added `Store.PullTelegram` for one persisted intake attempt. The older
  `RunTelegramIntake` remains, but the staged CLI uses the supervised single step.
- API/server or main dispatcher termination still stops the runtime. This work
  does not enable production activation.

Principal files: `internal/notification/install/supervision.go`,
`supervision_test.go`, `cli.go`;
`internal/notification/worker/runtime_health.go`, `runtime_health_test.go`,
`store.go`, `status.go`, `command_intake.go`.

## Verification recorded before interruption

Use `GOCACHE=/tmp/deploy-notification-go-cache` for the commands below.

- Compilation and focused non-race Signal range/baseline/restore tests passed.
- Focused race tests passed for interaction, relay and worker packages with
  `-run 'Test(Signal|Restored|Baseline|InboxBaseline|Relay|RequestRecovery)' -count=1`.
- Further worker/relay race tests passed with
  `-run 'TestSignal(Baseline|Missing|HistoryProof|Repeated|CursorRelaySIGKILL)' -count=1`.
- The newest worker/install supervision and health race tests passed with
  `-run 'Test(RuntimeLoop|AmbiguousCommandReply|InteractiveSupervision|LED|Health|Status)' -count=1`.
- The maximum-range proof test passed using positions near MaxInt64 within the
  existing 16,384-byte control budget.
- `go vet ./...` and
  `GOOS=linux GOARCH=arm64 go build -o /tmp/deploy-notification-arm64 ./cmd/deploy`
  passed **before the newest supervision changes**. Repeat after review.
- `git diff --check` passed during implementation. This does not by itself check
  the contents of the many untracked files.
- A broad suite was launched with
  `go test -race ./... -skip '^(TestInteractiveUnixHandoffAndLedger|TestUnixRoundTripAndIdentityFence)$'`.
  Its final result was not received. The original process session was `85968`;
  polling it while writing this handoff returned **Unknown process id**. Treat
  the result as unknown and rerun it. It also started before the latest changes.

The two skipped Unix socket tests previously failed inside the sandbox with
`setsockopt: operation not permitted`. The user granted a one-time unsandboxed
exception for those tests, but automatic approval review timed out and the
unsandboxed attempts did not start. No successful result is available for them.

No new live Signal/Telegram delivery, native ARM64 runtime qualification, VM
power-loss test or physical LED qualification was completed in this session.
An ARM64 cross-build is not native runtime qualification.

## Resume here

1. Review the newest supervision and health changes, including concurrent CLI
   behavior. Focused supervisor/store tests pass; actual concurrent CLI loop
   integration still deserves coverage. Telegram now performs its read-only
   preflight on each supervised attempt.
2. Check whether frequently renewed loop deadlines cause excessive conservative
   deferrals in LED reconciliation, which compares health before and after I/O.
   This is a review concern, not a confirmed failure.
3. Update plan status for supervision. `INTERACTIVE_WORKER_PLAN.md` still says
   polling/dispatch errors stop the staged runtime and independent supervision
   is unfinished; those sentences predate the latest code. Recovery-range,
   snapshot and missing-evidence documentation was already updated in both
   plans and `README.md`.
4. Rerun vet, ARM64 cross-build and the broad race suite against the final files;
   distinguish sandbox restrictions from genuine failures.
5. Continue the unfinished plan requirements: managed candidate publication and
   activation; receiver shutdown and fresh provider cutoffs; repeated gateway
   lifecycle recovery; configurable LED bindings and recovery-safe ownership
   reconciliation; inventory/systemd/Quadlet installation, update and rollback;
   account-linking maintenance; backup shared-notification migration and report
   service installation/scheduling; receipt compaction and retry administration;
   remaining producer integrations and qualification. Reconcile this list with
   the detailed plan checklists before claiming completion.

No partial LED-binding implementation was written in the latest session; that
area was inspected only.

## User decisions and workspace precautions

- The user will manually test physical LED behavior after implementation.
  Raspberry Pi 4 is available as `sal@rsp4`, with `pi-status-led` already installed.
  The pending read-only SSH attempt was cancelled; no successful connection,
  remote change or qualification was made. Do not resume hardware testing as a
  prerequisite to completing local implementation.
- Native ARM64/live-service qualification remains deferred and must be reported
  separately from local fixture coverage. No Signal image was supplied by the user.
- The repository contains extensive earlier modularization changes: many tracked
  root files appear deleted, while `internal/`, `cmd/`, plans and other files are
  untracked. Preserve all of it. Do not reset, clean, stage or commit unrelated work.
- No commit was created during this implementation session. Current progress is
  in the working files, not a committed checkpoint.
- Work is paused for tomorrow at the user's request. This handoff does not start
  a background continuation or scheduled task.
