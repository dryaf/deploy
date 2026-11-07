# Roadmap

Updated 2026-11-07. This is the current list of unfinished work. Implementation
is paused at the user’s request. [Progress checkpoint](../progress.md) records
the integrated source, isolated work in progress, test evidence and resume steps.
Lasting behavior belongs in user guides and design references.

The original worker implementation recorded **84/84 software criteria**. The newer
YAML usability milestone is **still in progress** and has its own acceptance
scenarios. The historical 100% score does not measure this milestone or establish
production readiness. No current usability completion percentage is asserted.

## What works today

- Version-3 `server.yaml`, home-directory defaults, private credential-file
  references, guided missing-input prompts and unattended configuration.
- Short install, plan, status and resume commands for fresh Telegram and fresh
  direct-alert Signal setups, standalone backups and local shared backup setup.
- Explicit provider diagnostics, recorded qualification, manual backup requests
  and schedule activation after the required checks.
- Supported source additions, retained-identity endpoint moves and explicit
  source retirement; supported standalone backup policy and Telegram credential
  updates; unbound notification policy and activity updates.
- Initial LED setup and verification-mode mapping/output changes; qualified
  production output toggles preserve ownership, incidents and reporting.
- Initial Telegram backup read commands and explicitly authorized incident
  acknowledgement/snooze/unsnooze for new local shared installations,
  with explicit actors, conversations and separate command admission.
- Read-only import/review of existing installations and selected adoption paths.
- YAML disable/re-enable of already admitted Telegram backup commands, retaining
  reporting and command results; re-enable requires valid delivery qualification.
- YAML permission and subject-scope reductions while commands are disabled,
  preserving those restrictions when intake is re-enabled.
- YAML command-alias renaming and conversation retirement while intake is disabled,
  retaining reporting, saved results and original reply destinations.
- Source replacement under new aliases through separate enrollment/retirement
  applies or one combined apply with explicit retirement reasons.

The [worker guide](workers/README.md) and
[configuration reference](reference/worker-configuration.md) define the supported
scope. A configurable field is not a promise that every live change is supported.

## Ordinary YAML work: completed steps

1. **Initial incident command mode:** new Telegram/shared-backup declarations can
   use `commands.mode: mutation` with explicit `incident.manage` permissions.
   Versioned receiver records preserve read-only compatibility; helper checks,
   qualification, actor/subject permissions and separate admission still apply.
   This does not complete later mode or permission migrations.
2. **Existing command activity:** toggle `commands.enabled` and apply the
   notification worker. Gateway and backup declaration changes resume together;
   both workers' ordinary apply/resume/status/plan remain usable. The original
   receiver, saved results and reporting handoff are preserved. Other settings
   must stay unchanged; expired delivery qualification is not renewed implicitly.

3. **Command permission reductions:** while commands are disabled, remove actors,
   rights or subject access and apply the notification worker. Review rejects
   expansion before retaining an operation; gateway and backup receipts resume
   together. Re-enabling retains the narrowed policy. Adding rights and changing
   audiences remain unfinished.

4. **Command enrollment reductions:** rename the backup alias or retire a
   conversation with its permissions while intake is disabled. Subsequent
   permission changes retain the migrated policy; old provider input cannot be
   reinterpreted under the new alias. New or redirected audiences still need
   their admission workflow.
5. **Mode changes within receiver capabilities:** installations originally
   admitted for incident commands can select read mode while disabled and later
   restore mutation mode. Rights stay reduced; re-enable checks qualification.
   An originally read-only receiver still needs its upgrade admission workflow.

6. **Source replacement under a new alias:** add the replacement first, then
   retire the old source with an explicit reason in a separate apply. The actual
   CLI passed both standalone and shared fixtures, including lost revocation
   replies. Worker/archive/producer identities remain; application backup and
   restore qualification are still operator checks.

7. **Production LED output:** change only `status_leds.enabled` on an unrecovered
   gateway with a local shared backup. The coordinator retains producer and
   delivery ownership, commands, bindings and incidents; both workers' ordinary
   commands continue to work. Both directions require valid delivery qualification,
   and re-enable requires originally qualified output. Production mapping,
   daemon/device replacement and recovered installations remain unfinished.

8. **Combined source replacement:** add new aliases and retire old ones in one
   YAML edit with explicit `retire_sources` reasons. Enrollment completes before
   replacement publication and revocation. Both intents and new keys survive
   rollback; lost replies resume through ordinary commands. Worker/archive and
   shared producer identities remain. Retired alias/identity reuse is unfinished.

9. **Guided LED qualification:** the short qualification wizard records the
   observed daemon version and configuration-file digest alongside manual checks.
   Deferred details remain incomplete; interrupted prompts resume, recorded facts
   stay immutable, and the original observation window remains unchanged.
   Physical/provider qualification and production requalification remain separate.

10. **Shared limits before activation:** change notification queue/storage budgets
    after local backup enrollment, before delivery
    activation; step 15 adds unchanged LED installations. Both workers retain the revised declaration and original producer;
    pending work is preserved, undersized budgets refused, and configured commands
    remain unadmitted. Exact resume handles interruption; qualify the revised
    configuration before delivery. Production and recovered updates remain unfinished.

11. **Shared producer timing before activation:** change the local backup
    producer's heartbeat/evidence freshness thresholds without changing identity,
    routes, quota or command policy. Original observation times remain; explicit
    declarations of unchanged effective defaults do not restart the gateway.
    Timing and budget changes can follow each other. Recovered and
    production changes remain unfinished; step 23 adds derived backup freshness
    before activation.

12. **Shared routing before activation:** add/remove unused routes or change
    primary/fallback order among existing providers on an unrecovered gateway
    with unchanged LED policy. Pending messages block the change. Producer subscriptions,
    enrollment and terminal message history remain; both workers retain the new
    scope before qualification and admission. Production routing remains unfinished.

13. **Existing producer policy before activation:** change the local backup
    producer's explicit quota, requiredness, allowed routes, policy-health setting
    or context capabilities. Identity and original observations remain. Quota
    reductions must fit retained work; access-policy changes require a settled
    queue. Explicit unchanged defaults cause no restart. This does not admit
    commands or interactive alert destinations; production/recovery changes
    and new producer identities remain unfinished.

14. **Shared Telegram changes before activation:** command-free unrecovered
    gateways can select new private token/chat files, preserving
    the original bootstrap and every reviewed credential set. The queue must be
    settled; an unchanged owned Signal service stays retained. Both workers'
    ordinary commands and later policy edits use the selected provider. Changed
    runtime/destination qualification remains explicit; production and command
    profiles remain unfinished.

15. **Shared policy with LEDs before activation:** budgets, producer timing,
    routes and existing producer policy retain unchanged LED bindings, endpoint,
    completed output permission and unresolved faults. Interrupted changes pin
    the same history; missing records cannot silently reset output. Direct
    Telegram profile changes follow the same LED preservation contract on
    command-free gateways. Production/recovery changes remain separate.

16. **Alternating LED mappings and policy:** before activation, completed LED
    mapping edits can follow policy edits and vice versa. Each change proves the
    exact intervening history; retries keep output permission and unresolved
    faults. Mapping edits still require enabled output. Unapplied backup-only
    changes are rejected before a mapping/output operation is retained.

17. **Restore originally admitted command permissions:** while intake is disabled,
    restore an actor, right or subject scope that the existing receiver originally
    admitted. Apply permissions separately from aliases, conversations and mode.
    Explicit history preserves reporting and results, rejects old pending input,
    and survives interruption. Re-enabling alone does not restore rights. New
    authority beyond the original receiver and retired audiences remain pending.

18. **Add commands to an existing staged gateway:** command-free Telegram gateways
    with a local backup reservation can enroll read or mutation policy before
    delivery activation. Retain the original archive, source keys, provider files,
    gateway identity and LED disposition. Pending command history or foreign aliases
    block enrollment. Qualification, receiver setup and admission stay separate.
    Already activated or recovered gateways still need a later migration.

19. **Revise command policy before activation:** an existing, unadmitted Telegram
    declaration can change actors, rights, scopes, aliases, conversations/topics
    and read/mutation mode. Producer identity, old alias reservations, backup
    scope and LED output history remain. Interrupted changes resume exactly;
    qualification, receiver review and intake admission follow the revised policy.
    Already activated installations still need their production migrations.

20. **Command activity before activation:** disable an unadmitted command
    declaration and later restore its exact policy through YAML. Alias
    reservations, backup scope and LED output history remain. Other verification
    policy can change while commands are disabled; re-enable and command-policy
    edits remain separate. Neither toggle admits a receiver or provider intake.

21. **Notification-first command declarations:** while no backup producer is
    reserved, edit future commands and fill missing bot metadata through the
    same apply/resume flow. The gateway runtime, credentials and service remain
    unchanged. Later unbound policy changes retain the declaration; reserved
    dependencies require setup completion before enrolled policy updates.

22. **Telegram files with unadmitted commands:** before activation, rotate token
    references or the fixed alert destination while keeping the same command bot,
    actors, conversations and permissions. Alerts and future intake share the
    selected token. Disabled verification commands retain their account too;
    old/new files, alias history and LED state remain. Pending messages and any
    admitted command history block this workflow. Production/bot replacement
    remains separate.

23. **Derived backup freshness before activation:** change only
    `backup_worker.freshness` when the local shared producer uses default policy.
    Either worker's apply coordinates the backup runtimes, archive checkpoint and
    gateway evidence deadline. Original observations, source keys and producer
    identity remain. Exact retries finish interrupted publication; other backup
    readers block publication and qualification must match the revised runtime.
    Apply schedule edits separately. Step 39 adds production changes; recovered
    variants remain pending.

24. **Shared backup report route before activation:** select an existing
    notification route through either worker's apply. Default producer permissions
    change with a settled queue; an explicitly allowed route records the selection
    without restarting the gateway. Backup runtime, source keys, original producer
    enrollment and LED history remain. Both declaration scopes resume together.
    Production and recovered route changes remain pending.

25. **Standalone Telegram alert destination:** select a new private numeric
    chat-ID file for the same bot. Apply requires accepted retained outbox rows
    and no unresolved provider reservation, then rechecks under the installation
    lease. Source/archive identities and delivery history remain; old/new inputs
    stay pinned through rollback. Timers return to staged qualification. Shared
    production destination and bot replacement workflows remain pending.

26. **Explicit backup policy at fresh Signal setup:** notification-first Signal
    or combined Signal/Telegram installations can retain the local backup
    producer's declared settings before reservation. The gateway stays unbound;
    later enrollment uses the real reserved identity and exact declared limits.
    Unsupported producer identities and interactive intake remain refused.

27. **Telegram profiles beside owned Signal:** before activation, add an unused
    direct Telegram profile, or remove it after routes stop referencing it. Both
    unbound and local shared gateways preserve Signal account/service authority,
    old credential files and producer enrollment. Interrupted applies resume;
    shared queues must be settled and qualification follows the final runtime.
    Signal replacement, command intake and production changes remain pending.

28. **Renew qualification after Telegram delivery admission:** the short qualify
    command pauses a command-free local shared gateway with unchanged policy,
    retaining queued work and delivery ownership. Lost pause replies resume and
    ordinary apply requires a newly recorded qualification before reactivation.
    Prior evidence cannot silently renew a second pause. Command, Signal,
    recovered and later production LED-output continuations remain unfinished.

29. **Renew owned Signal delivery qualification:** the same short qualify/apply
    sequence supports an unchanged owned native Signal service, alone or beside
    Telegram. Interrupted pauses and explicit service resume preserve account,
    inbox and reporting ownership. Every renewal cycle requires new observations;
    artifact replacement, relinking, commands and recovered gateways remain separate.

30. **Renew after production LED output changes:** alternate qualified output
    toggles and short qualification renewals without replacing reporting ownership,
    LED bindings or backup configuration receipts. A renewal while disabled retains
    previously admitted output permission; originally disabled installations cannot
    gain it by renewing. Commands, mapping/device replacement and recovery remain
    separate workflows.

31. **Production Telegram credential references:** command-free, unrecovered
    Telegram-only gateways can select new private files for the same bot and chat.
    Apply coordinates a delivery pause, expanded backup report-service exclusions
    and both declarations; the original owner, queued messages and source/archive
    identities remain. Exact retries resume interrupted changes. Fresh qualification
    and another apply resume delivery. Retained credential history also survives
    deactivation and a new specialist admission. Signal, command, destination and
    recovered variants remain unfinished.

32. **Production Telegram rotation beside owned Signal:** the same short
    apply/qualify/apply sequence retains an unchanged native Signal account,
    service, inbox and artifact while rotating same-bot/same-chat Telegram files.
    Explicit resume repairs a stopped native service only after checking the
    retained coordinator and private inputs. Fresh qualification still covers
    the combined runtime. Signal account replacement, command profiles and
    recovered installations remain separate.

33. **Production notification limits:** effective budget changes on an unrecovered
    command-free local shared gateway retain the original delivery owner, active
    producer generations, queued work, credential exclusions and LED disposition.
    Both workers record the policy while reporting is fenced; fresh qualification
    and a final apply resume delivery. Undersized budgets and lost history are
    refused. Unchanged owned Signal is retained. Command-bearing and recovered
    changes remain separate work. Explicit budget defaults already normalize to
    the same declaration.

34. **Production producer policy:** existing local backup producer timing, quotas,
    requiredness, allowed routes, policy health and context capabilities can
    change through apply/qualify/apply on an unrecovered command-free gateway.
    Timing and quotas retain queued work; access changes require a settled queue.
    The active backup reporting route stays allowed. Original observations,
    producer/delivery ownership, reporting, owned Signal and LED disposition are
    retained. Recovered and command-bearing variants and additional identities remain
    unfinished. Explicit default producer declarations are completed in step 38.

35. **Production notification routes:** change route names or primary/fallback
    ordering among existing providers on an unrecovered command-free gateway.
    Pending messages must settle first; producer permissions and the selected
    backup reporting route stay unchanged. Apply pauses delivery, records both
    declarations and retains original ownership, terminal history, Signal and
    LED disposition; qualify and apply to resume. Recovered and command-bearing
    variants, provider replacement and backup route selection remain separate.
    Explicit single-provider route defaults already normalize to the same declaration.

36. **Production Telegram destinations:** select new target-side token/chat files
    for a different chat or bot on an unrecovered command-free local shared
    gateway. The queue must settle before the coordinated delivery pause; terminal
    history, original ownership, reporting, old credential exclusions, native
    Signal and LED disposition remain. Qualify the new destination before the
    final apply. Same-destination rotations continue to retain pending work.
    Explicit account/name declaration changes, provider addition/removal,
    recovered and command-bearing variants remain separate.

37. **Production unused Telegram enrollment:** add or remove an unused direct
    Telegram provider beside unchanged owned Signal on an unrecovered command-free
    gateway. Keep routes unchanged; settle pending work before each change, then
    apply/qualify/apply. Removing a selected provider is refused, so complete its
    route change first. Both declarations, original ownership, reporting, old
    credential exclusions and LED disposition remain. Explicit account/name,
    recovered and command-bearing variants remain separate.

38. **Unchanged effective declarations:** explicit producer settings that match
    the current derived policy, and a return to matching derived defaults, record
    both declarations without pausing delivery, restarting services, renewing
    qualification or changing queued work. Interrupted recording resumes the same
    operation. Later effective changes still require their normal migration.
    Budget and single-provider route defaults already normalize to their omitted
    forms; they do not require a migration.

39. **Production derived backup freshness:** change only `backup_worker.freshness`
    through either worker on an unrecovered command-free local shared gateway.
    Capture and report units are checked and stopped before publishing both backup
    runtimes, the archive checkpoint and the gateway evidence deadline. Original
    observations, source/archive/producer ownership, Signal and LED settings remain.
    Exact retries finish interrupted publication. Qualify and apply the gateway;
    qualify and activate backup capture separately. Recovered and command-bearing
    variants remain unfinished.

40. **Backup policy after shared notification changes:** schedule, budget and other
    supported backup-only policy updates retain the original reporting handoff
    after completed production notification changes. Durable update receipts
    preserve both histories through lost completion replies. Later notification
    changes use the completed backup declaration and reject unapplied edits.
    Explicit producer timing remains independent of backup freshness. Source
    continuations are covered by steps 41–44; image updates by step 45 and
    backup helper upgrades by steps 46–47.

41. **Source endpoint moves after shared policy changes:** change an existing
    source's host, user or SSH port while retaining its identity, keys and helper.
    A typed completion receipt preserves the original reporting runtime even
    though the worker policy fingerprint is unchanged. Lost inspection replies
    resume without enrolling another source. Both workers' ordinary commands and
    later notification updates retain the completed move. Source additions, retirement and combined replacement follow in steps 42–44.

42. **Source additions after shared policy changes:** add new aliases while
    retaining existing source endpoints, keys, artifacts and gateway policy.
    Local publication and rollback preserve the exact shared-history intent and
    every remote enrollment. Completion records both histories; later notification
    updates retain the expanded backup. Missing added-source keys or enrollment
    receipts are refused. Retirement and combined replacement follow in steps 43–44.

43. **Source retirement after shared policy changes:** remove a source with an
    explicit YAML retirement reason while retaining the remaining sources and
    gateway policy. Local publication stages capture and records the archive
    disposition; history completes only after remote revocation is acknowledged.
    Rollback, lost revocation replies and lost completion replies retain the same
    intent. Both workers and later notification changes preserve the original
    handoff. Combined replacement follows in step 44; retired identity reuse remains separate.

44. **Combined source replacement after shared policy changes:** add new aliases
    and retire old ones in one YAML edit. The published remaining-source runtime
    retains both enrollment and retirement intents until remote revocations are
    acknowledged. Rollback and lost replies preserve both sets of keys and the
    original reporting owner; later notification updates use the completed source
    set. Reusing retired aliases or identities remains unfinished.

45. **Pinned backup-image updates with shared reporting:** change only the backup
    container image while retaining its helper, source artifacts, endpoints and
    policy. Read-only producer review accepts that bounded transition; publication
    and rollback retain the original archive/reporting owner. Typed image history
    supports later notification edits, including after source replacement.
    Capture is staged for qualification. Command-free backup helper upgrades
    follow in step 46; relay-image migration remains separate work.

46. **Command-free shared backup helper upgrades:** replace the backup helper and
    compatible image while retaining source helpers, endpoints and other policy.
    Read-only review checks the installed helper and rejects retained command
    authority. Publication records old/new artifacts without rewriting the
    original reporting handoff. Rollback retains the same upgrade intent and
    reports a resumable state; changed retry inputs are refused. Capture is staged
    for qualification. Later notification updates preserve the completed helper
    upgrade. Compatible command-bearing upgrades follow in step 47. Gateway/source helpers
    and receiver capability expansion remain separate.

47. **Compatible backup helper upgrades with command receivers:** retain the
    completed receiver's version, permissions, SQL binding, pending requests and
    saved results while replacing its helper and matching image. The candidate
    must support the existing receiver. Review does not write database sidecars;
    missing records, locks, ledger tables and retained WAL require recovery.
    Original fresh reporting ownership is required. Completed command admission
    and activity inspect the applied backup change without
    replacing its original receipt. Later command activity changes use that
    completed backup policy, including disable/re-enable cycles; unapplied YAML
    edits are refused. New receiver
    capabilities and permissions still require a separate admission workflow.

48. **Delivery requalification with disabled commands:** disable Telegram backup
    commands in YAML, apply, qualify, then apply again. Typed pause and renewal
    records retain receiver ownership, command history, queued requests and
    permission restrictions. Both installed executables must support the new
    pause capability. Fresh delivery evidence does not enable intake; subsequent
    permission reductions and explicit command re-enable keep the original
    receiver. Read and mutation modes use the same workflow. Recovered and legacy
    Signal qualification remain separate.

49. **Notification budgets with disabled commands:** unrecovered Telegram gateways
    with already admitted backup commands can disable intake, apply limits-only
    edits, qualify and apply to resume delivery. Original receiver ownership,
    permission restrictions, queued requests, saved results, reporting and LED
    disposition remain. Both read and mutation modes support repeated changes,
    interruption/resume and later independent qualification renewals. Global
    limits update derived producer message limits; command queue budgets remain
    separate. Re-enable intake explicitly in YAML and apply afterward.

Step 49 is integrated and its regular tests and vet passed. Work is paused at the
user's request after this step. The [progress checkpoint](../progress.md) records
the final source and evidence; race and live qualification remain deferred.

## Next: finish ordinary YAML workflows

| Work remaining | Completion means |
| --- | --- |
| Shared configuration changes | Policy, provider/destination, freshness and activity changes coordinate the gateway and backup worker while preserving pending work and delivery ownership. |
| Source identity reuse | Reusing retired aliases or source identities has an explicit migration/disposition workflow with archives and history retained. Combined replacement with new aliases and separate add-then-retire are supported. |
| Command changes | Initial command enrollment on an already activated command-free installation, new permission/scope authority and new/redirected audiences on activated installations work through short commands; receiver capability upgrades, recovered/legacy qualification, Signal intake and interactive alert destinations have their own admission checks. |
| Production LED changes | Mapping changes, daemon/device replacement and recovered installations work after activation and preserve unresolved incidents and other clients' output. Qualified output-only toggles are supported. |
| Existing Signal accounts | Legacy takeover/adoption and relinking work beyond the currently supported owned-service paths, with one account owner throughout. |
| Worker lifecycle | Shared gateway/source helpers, disable/removal/reinstallation and existing-directory relocation have complete resumable workflows. |
| Complete field behavior | Every public setting has a verified apply, no-op, migration or explicit required-action path; unsupported changes cannot silently lose state. |

Detailed contracts and dependencies remain in the
[active usability plan](plans/worker-usability.md), work packages U1–U11 and
acceptance scenarios UX01–UX30. Retire that plan only when its implementation and
acceptance work is complete; retain its lasting contracts in the reference docs.

## Then: make first installation straightforward

- [ ] Supply complete matching release bundles for amd64 and ARM64, including
  required images and native Signal artifacts. The current helper bundle alone
  is incomplete.
- [ ] Finish guided host prerequisite checks and actionable administrator steps.
- [ ] Complete the short-command help/status flow so users can follow setup,
  qualification, activation and recovery without learning internal protocols.
- [ ] Verify a fresh Raspberry Pi walkthrough covering install, supported update,
  interruption/resume and uninstall with the packaged artifacts.
- [ ] Finish all software acceptance scenarios in the usability plan and record
  evidence against the final source.

## Verification and release

- [ ] Run the full regular tests and vet on the completed implementation with
  `make test`.
- [ ] After implementation and regular checks pass, run `make test-all`, which
  adds race tests. Investigate any failures before calling that gate complete.
- [ ] Run the applicable isolated host and VM interruption/recovery workflows.
- [ ] Configure release automation and enforcement of the release checks.

Recent recorded checks and their exact scope are in
[validation](development/validation.md). A past test result does not qualify
later source changes.

## Manual qualification — separate from software completion

The operator will perform live checks after implementation. They remain pending:

- Actual Telegram/Signal delivery, reply routing, fallback and outage monitoring.
- Native ARM64 execution, physical Pi LED behavior and host permissions.
- Real user-systemd/rootless behavior, cross-UID boundaries, and VM reboot/power
  interruption where the acceptance contracts require them.
- End-to-end backup and restore observations on the chosen disposable targets.

Record the installation, configuration, artifact versions and observed outcome.
Fixtures, successful builds and entered qualification records do not substitute
for observations that have not been performed.

## Planned application workflow: local image build and SSH transfer

Requested 2026-11-07; not implemented yet. Build the application container image
locally, transfer it directly over SSH and load it into the remote user's Podman
storage. No image registry is required. Configure the workflow in `deploy.yaml`
and run it with the existing `deploy release <environment>` command.

- [ ] Add a documented YAML choice for local image builds, with Dockerfile,
  build context and target architecture settings. Preserve existing remote-build
  and registry-image workflows; finalize the configuration names during design.
- [ ] Check local build prerequisites and remote architecture before deployment.
  Support amd64 and ARM64 targets, with actionable errors when cross-platform
  building is unavailable.
- [ ] Build once, export the image and transfer it through the configured SSH
  connection. Verify archive integrity and loaded image identity and platform
  before changing the running service.
- [ ] Add YAML-configured signing of the exported archive and release metadata,
  binding the archive checksum, image identity, application, release and target
  platform. Keep private signing keys on the workstation or CI system; configure
  key references rather than secret values in YAML. Select and document the
  signing format/tool during implementation, with Cosign file signing as a
  candidate that requires no image registry.
- [ ] Transfer signature evidence alongside the archive and verify it remotely
  against previously trusted public keys before loading the image or changing
  the service. Do not treat a public key supplied with the transfer as trusted
  automatically, or rely on `podman load` alone for signature enforcement.
  When an environment requires signing, reject missing, invalid or untrusted
  signatures without falling back to unsigned deployment.
- [ ] Define explicit trusted-key provisioning and rotation, retain verification
  evidence with each release, and preserve verification of retained rollback
  artifacts under the configured trust policy.
- [ ] Pin the loaded image in the Quadlet and integrate with existing release
  locking, health checks and rollback to the exact previous image.
- [ ] Handle interrupted transfers and imports with safe retries and cleanup;
  retain images needed by active releases and rollback.
- [ ] Document prerequisites and examples in the [application guide](applications.md).
  Test successful deployment, architecture mismatch, corrupted/interrupted
  transfer, retry, failed startup and rollback without a registry.
  Include signed deployment, archive/metadata tampering, missing signatures,
  wrong signing keys, key rotation and rollback verification.

## Planned private networking: overlay endpoints and service binding

Requested 2026-11-07; not implemented yet. Add optional overlay endpoint metadata
to the existing version-3 `server.yaml` inventory and explicit policies for using
it. Keep connection selection and listener exposure separate. Final field names,
including whether to use `mesh_ip` or a broader endpoint declaration, remain to be
designed.

For a fresh installation today, `servers.<name>.host` can already contain a
reachable WireGuard/Tailscale address. Explicit IP bindings can also be supplied
through application `quadlet.ports`. The planned feature adds coordinated policy,
validation and diagnostics when public and private endpoints coexist; it is not
a prerequisite for the first Raspberry Pi setup.

### Phase 1: explicit private endpoint selection

- [ ] Add optional overlay addresses with documented IPv4/IPv6 behavior. Adding
  an address alone must not silently change existing connection choices.
- [ ] Define separate operator-to-server and worker-to-source transport choices.
  A required-private policy fails when the selected overlay endpoint is
  unavailable. Any public fallback, including bootstrap access, requires an
  explicit policy and must be visible in plan/status output.
- [ ] Check reachability from the actual initiator: operator computer to worker,
  and worker to each source. Declared overlay addresses do not establish routes
  or prove connectivity. Report missing routes, unavailable endpoints and trust
  failures with actionable next steps.
- [ ] Preserve strict SSH host-key verification and stable server/source identity
  across public and overlay addresses. Evaluate explicit host-key aliases or
  certificates; keep connection caches distinct for endpoint, port, account and
  trust settings. Preserve restricted per-source worker keys and authorization.
- [ ] Carry selected endpoints through CLI SSH, worker SSH and applicable file
  transfers. Retain the existing SQLite snapshot, verification and transfer
  protocols; private routing must not substitute raw copying of a live database.
  Make the same endpoint policy available to future telemetry probes.
- [ ] Treat endpoint and transport-policy changes on installed workers as reviewed
  migrations. Integrate with source endpoint pins, exact retry declarations,
  rollback and renewed qualification; do not silently choose a different server
  or re-enroll source identity after a connection failure.
- [ ] Test public/private selection, tunnel loss, explicitly permitted fallback,
  different operator/worker reachability, host-key mismatch, IPv4/IPv6, endpoint
  migration and interruption/resume.

### Phase 2: managed private listener binding

- [ ] Add explicit application/service binding policy using generated Quadlet
  settings for supported network modes. Validate that the selected overlay bind
  address belongs to the target, and define behavior across reboot, tunnel
  startup, address changes and service restart.
- [ ] Bind selected published ports only to the configured address. Reject
  conflicting wildcard mappings, host-network/raw Podman options or other
  configuration that bypasses required-private exposure. An unavailable address
  must not cause fallback to `0.0.0.0`, `::` or another public listener.
- [ ] Coordinate private application intent with Traefik/router configuration so
  a private port binding does not leave the service publicly exposed through an
  existing proxy route. Retain application authentication and overlay access
  controls as independent requirements.
- [ ] Keep current local worker APIs on Unix sockets. Apply this binding policy
  to explicitly networked services, including future OTLP listeners, rather than
  introducing TCP listeners for existing local-only protocols.
- [ ] Verify generated settings and actual listener addresses in the supported
  rootless Podman environments. Test intended private access, absence of wildcard
  exposure, conflicting proxy routes, tunnel loss/recovery, IPv6 and rollback.

VPN provisioning, keys, kernel interfaces, routing, firewall rules and SSH daemon
listener configuration remain administrator-owned. Selecting an overlay address
does not itself prove encryption or close an existing public SSH listener.
Telegram/Signal provider requests retain their normal internet routing unless the
administrator separately configures an egress route. Document these boundaries
without promising zero public exposure from an address field alone.

## Planned monitoring: uptime, embedded telemetry and anomaly detection

Requested 2026-11-07; not implemented yet and outside the current worker usability
milestone. Design this alongside the planned uptime worker so availability checks
and optional telemetry share one monitoring model. A Raspberry Pi may monitor
local services and remote VPS applications without requiring a cloud service or
a separate observability stack. Final worker and YAML field names remain to be
designed.

### Phase 1: probes, bounded history and explicit thresholds

- [ ] Configure monitoring through the existing version-3 `server.yaml` inventory
  and short worker commands. Use home-directory defaults with optional path
  overrides and private credential-file references.
- [ ] Add HTTP availability and latency probes, plus narrowly authorized remote
  observations for service/container memory, disk space and SQLite DB/WAL file
  sizes. Reuse named SSH hosts and verified host keys, with bounded timeouts and
  concurrency. Define an allowlisted helper protocol for remote observations.
- [ ] Reuse notification-worker producer interfaces, routes, incident/recovery
  handling and LED ownership. Keep provider credentials and LED arbitration in
  the existing gateway; recovering one probe must not clear another fault or
  another client's output.
- [ ] Store samples in an embedded SQLite database with stable host/probe/series
  identity, units, observation time and result status. Batch writes, impose hard
  database/WAL and free-space limits, and prune retention in bounded batches.
  Define acceptable sample loss separately from durable incident/delivery state;
  consider WAL with `synchronous=NORMAL` only for loss-tolerant sample history.
- [ ] Support explicit thresholds with consecutive-breach and recovery rules,
  deduplicated alerts and reminders. Treat probe failure, stale/missing data and
  confirmed recovery as distinct states. Monitor the telemetry worker's own
  freshness and define behavior during network outages and restarts.
- [ ] Treat WAL size as an observation, not proof of failed checkpointing: combine
  size, sustained growth and disk-space thresholds. Do not checkpoint or modify
  application databases as a side effect of monitoring.
- [ ] Benchmark on Raspberry Pi/ARM64 and a small VPS using a declared probe
  count, interval and retention. Treat under 25 MiB RAM as an initial target;
  report worker RSS, SQLite cache, helper-process peaks, CPU and disk writes.
  Document detection delay: three breaches at 60-second intervals take roughly
  2–3 minutes to detect, plus notification delivery time.

### Phase 2: rolling baselines and anomaly rules

- [ ] Add rolling baselines with minimum sample count and baseline age, explicit
  warm-up status, zero-variance handling and numerically stable calculations.
  Score against prior eligible observations before incorporating the current
  sample. Keep absolute thresholds active during warm-up and sustained incidents.
- [ ] Offer Z-score and multiplier rules, and evaluate robust alternatives such
  as median/MAD or percentile thresholds for suitable metrics. Define baseline
  update rules so sustained degradation is not silently learned as healthy.
- [ ] Account for deployment changes, maintenance windows, missing observations
  and periodic workloads. Define separate trigger/recovery thresholds and
  consecutive counts; emit recovery only after confirmed healthy observations.
- [ ] Add bounded aggregates for longer history and explainable alerts showing
  the observed value, units, baseline, applicable rule and observation period.
  Test spikes, gradual degradation, outages, cold starts, zero baselines,
  deployment changes, restart/resume, alert deduplication and recovery.

### Phase 3: scoped OpenTelemetry ingestion

- [ ] Define and document a supported OTLP/HTTP metrics subset before claiming
  compatibility. Handle metric/resource identity, units, timestamps, counter
  resets, cumulative/delta temporality and histogram buckets; do not reduce
  histograms to averages when tail-latency rules need their distribution.
- [ ] Provide explicit listener configuration, authentication/access controls,
  request-size, rate and series-cardinality limits, and backpressure. Include
  malformed payloads, unsupported metric types and resource-exhaustion tests.

## Later product work

Real vulnerability and uptime workers remain outside the current implementation
milestone. Existing synthetic producers exercise their shared interfaces; they
are not production scanners or monitors. The monitoring phases above define the
planned uptime/telemetry work; the vulnerability worker remains separate.
