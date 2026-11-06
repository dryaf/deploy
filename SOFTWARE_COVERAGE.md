# Software implementation coverage

This review separates implementation from the full acceptance result in
[ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md). The user explicitly confirmed “Use software implementation coverage” on
2026-11-06, with manual qualification and unsupported workflows listed separately.
The implementation count therefore
asks whether the software and executable checks for each original criterion are
present, with no known missing software workflow in that criterion. It does not
count an unperformed provider, hardware, VM or target-load observation as passed.

The denominator remains the original 84 criteria. Each criterion contributes
at most one implementation entry; partial implementations contribute zero. Gate
criteria D-A10 and N-A15 are counted here for their implemented test harnesses,
opt-ins and qualification recording, **not** for a successful external release
qualification. Their acceptance results remain open in the acceptance matrix.
The plan text, evidence families in that matrix, and specific evidence below
remain authoritative; counting entries does not imply equal engineering effort.

The requested `make test` passed on 2026-11-06, including regular tests and vet.
The user explicitly stopped the broad race run and deferred race qualification
until after implementation. `make test-all` retains that separate opt-in gate; the interrupted
race run is not counted as passed. Prior completed focused race evidence remains
recorded. The completed implementation count is **84/84 (100% aggregate software coverage)**:

| Plan | Implemented software portions | Partial software portions |
| --- | ---: | --- |
| Deployment | 10/10 | None; registry, manager and VM observations remain unqualified |
| Backup | 43/43 | None; target qualification remains separate |
| Shared notification | 16/16 | None; live/provider/hardware observations remain separate |
| Interactive | 15/15 | None; real provider qualification remains separate |
| Total | 84/84 | No partial software criteria |

All original software criteria are implemented. This does not meet the separate
**full acceptance** score while external qualification remains unperformed; no
production-readiness claim follows from this count.

The active target is **84/84 software criteria**, requested on 2026-11-06.
The final regular `make test` and vet passed on the completed source. The authorized
`make test-all` race gate is now running. The current race result remains pending;
see progress.md for the source fingerprint and exact logs.

## Operator lifecycle scope

N-A16 includes executable `replacement-target`, `installation-retire` and
`replacement-init` procedures for UID/socket/general configuration and bounded
history changes. This is an explicit retirement and fresh-enrollment transition,
with a planned outage and retained old audit state. It does not preserve pending
work or transfer an interactive native account by changing its local label.
Obligations must settle first; continuing an existing producer/account uses the
implemented narrower migration and recovery workflows. Fresh replacement uses new
producer epochs and, for interaction, a fresh native account. See
[the complete runbook](examples/notification-worker/RETIREMENT_REPLACEMENT.md).

## Newly closed implementation and evidence gaps

- N-A16 now includes terminal installation retirement with SQL/file evidence,
  exact UID-owned LED baseline/lease cleanup, fresh replacement identity and target
  pins, full-history shutdown reserves, recovered Signal retirement, native CLI,
  private-input/dry-run checks and nine actual SIGKILL boundaries. The operator
  runbooks connect installation, recovery, configuration and history procedures.
  Backup aggregate retirement now requires explicit retirement of every configured
  source, no active instances and no open incidents; empty discovery is insufficient.


- N-A12 now includes legacy Signal migration and repeated fenced migration cycles,
  including three interleaved recovery epochs. Independent archived payloads,
  acceptance receipts, ownership history and held-source decisions remain required.
  Interrupted capture fences new reports; actual SIGKILL resumes retain that fence.
  See the current evidence in progress.md.

- B-A29 now includes shared-owner setup/update and SIGKILL recovery, restricted
  source key rotation, qualified report activation, and a versioned continuation
  for revocation/rollback after a configuration or helper update. Original cutover
  proofs remain immutable; lifecycle operations retain the legacy sender fence.

- I-A07/I-A09 now include `migrate-commands`: retained alias renames,
  conversation/worker removal, conversation generation changes, atomic pending
  input fences and full original-history import after restore. The integrated
  restore-before-conversion/alias-change scenario preserves one domain effect.
  Dual-provider reply adapters preserve accounts/destinations through credential
  rotation and withhold retired replies after enrollment changes. Native capability,
  interrupted lifecycle, lost history and SIGKILL checks pass. General provider
  account/native-audience changes use the N-A16 retirement/replacement procedure.


- N-A11 now includes legacy backup Signal account adoption with an explicitly
  incomplete empty inbox history, inherited child-lease fencing, published gateway
  handoff and retained bootstrap ancestry. The actual CLI review/adopt/handoff
  workflow, native helper probe and interruption/SIGKILL checks pass. See progress.md.


- N-A10 now includes repeated managed Telegram/Signal, command/LED and backup
  recovery. Snapshots retain independently copied publication/owner/runtime
  evidence; account/inbox and producer/command histories retain ancestor epochs.
  Three-epoch native Signal and backup lifecycle tests preserve original owners,
  request fences, receipts and a single executed command effect. Missing ancestor
  proof refuses admission. Next recovery and command-ledger SIGKILL tests cover
  the new durable boundaries. See the current logs in progress.md. Live providers,
  hardware and user managers remain unqualified.


- The final audit found that I-A06/I-A12 previously depended on test-only policy
  changes. `narrow-commands` now applies a complete retained permission array after
  disabling commands, with monotonic scope/permission checks, independent SQL/file
  history, binary capability checks and a canonical-config-preserving runtime
  policy. `TestManagedCommandPolicyNarrowsQueuedAndRunningWork` covers queued
  refusal, retained running/completed results, private reply suppression, fresh
  scoped reads and full actor revocation across reopenings. Interruption, lost
  anchors, old reader/manifest refusal and no-write dry-run checks accompany it.
  `TestManagedCommandPolicySIGKILL` kills a real child at each of the four durable
  boundaries and proves resumed admission keeps the revocation.
  `TestManagedCommandRecoveryPreservesLaterPolicy` imports a later revocation over
  an earlier snapshot and reactivates without restoring the actor. Expansion of
  rights remains a general migration requirement; supported enrollment changes are
  implemented through the version-3 migration above.


- `TestManagedCommandRecoveryCannotRepeatCommittedEffect` restores a gateway
  snapshot taken before an actual SQLite domain mutation, reconciles its retained
  result plus a later pending command, publishes/adopts recovery, freshly qualifies
  and re-enables mutations, and reopens twice. The domain effect remains one;
  historical commands cannot be reimported/granted, and old results cannot mint
  new receipts. Earlier worker recovery tests additionally retain confirmation
  invalidation, missing-source and conflicting-outcome controls.
- `TestManagedConversationIsolationThroughCredentialRotation` imports provider
  events for a private chat, a group and two threads, rejects an unenrolled
  thread, retains pending replies across credential change/deactivation/fresh
  qualification, and checks exact destinations and IDs across reopenings. The
  newer enrollment migration and cross-provider scenarios above close its remaining
  software transition evidence.
- `TestRecoveryIntakePinsRefuseWrongOperationAndSnapshot` and
  `TestRecoveryIntakeCLIDryRunAndExplicitScope` cover the new pinned history/drain/
  seal/cursor CLI. Managed recovery fixtures invoke the actual CLI coordinator
  functions. Wrong operation is refused before Telegram access; dry-run does not
  open state or contact providers. Existing history/drain/seal/cursor crash tests
  remain applicable to their shared implementation.
- `TestBackupRemigrationPreservesDeliveryOutcomes`,
  `TestBackupRemigrationHeldResolution`,
  `TestBackupRemigrationAcceptedBatchCoverage` and `TestBackupRemigrationSIGKILL`
  cover G → legacy → G+1 ownership, scoped decisions, exact member coverage and
  process death. `TestManagedDeliveryNativeExecutable` and
  `TestBackupRecoveryActivationNativeHelper` also exercise native predecessor
  checks and capability admission.
- `TestRuntimeLEDRecoverySocketPinnedDaemon` and
  `TestRuntimeLEDRecoverySIGKILLPinnedDaemon` exercise exact inherited worker-fault
  cleanup through a real local socket and pinned mock-hardware daemon, including
  lost results and foreign-state preservation. Physical output remains unqualified.
- `TestNativeManagedSignalDeliveryAndCommands`,
  `TestNativeLegacySignalConversationDelivery`,
  `TestNativeSignalProductionService` and
  `TestNativeSignalCommandSupervisorJoinsDescendants` exercise offline native
  production dispatch/intake, replacement/admission and child lifetime. They use
  a synthetic CLI and simulated manager, not a real messenger or host manager.

The release script now includes these production admission integration families,
recovered Telegram intake, backup recovery activation/remigration and runtime LED
cleanup. It previously selected only the older verification lifecycle families.
Native Signal scenarios have a five-minute whole-test context under race checking;
production RPC, admission and provider timeouts are unchanged.

## Baseline evidence retained

The unchanged areas retain the implementation and evidence mapped to every
criterion in ACCEPTANCE_MATRIX.md: deployment transaction/rollback/retention,
restricted backup capture and archive/pull/restore contracts, status and receipt
semantics, bounded adapters, command authorization and synthetic specialist
contracts. Representative executable proofs include:

- Deployment: `TestImageTransactionInstallAndMutableUpdate`,
  `TestHealthDeadlineBoundsHangingProbe`, `TestFailedFirstInstallAndInactiveRollback`,
  `TestPendingRecoveryBeforeAndAfterCommit`, `TestTerminatedTransactionRollsBack`,
  `TestDryRunNoRunnerOrFiles`, plus the isolated image and VM harnesses.
- Backup: `TestSQLiteWALSnapshotIsStandaloneAndLeavesSourceWAL`,
  `TestSQLiteSnapshotRejectsUninitializedSource`,
  `TestSQLiteValidatorUnsupportedSchemaAndBoundedOutput`,
  `TestBackupSourceEnrollmentRestrictionsAndRevocation`,
  `TestBackupCaptureReleasesLocksAfterSnapshot`,
  `TestBackupArchivePublicationAndIndependentPull`,
  `TestBackupRetentionProtectsCountAgeAndPins`,
  `TestBackupRestoreContractRejectsIneffectiveAndWrongInput`,
  `TestBackupSetupSIGKILLRecovery`, `TestBackupTakeoverFencesOldStateAndPreservesPins`,
  `TestBackupScheduleAcrossDST`, `TestBackupReviewPauseCatchupDurability`,
  and the report/receipt/monitor regression families.
- Notification: `TestUnixRoundTripAndIdentityFence`,
  `TestRestartDoesNotRenewStatusAndPreservesAmbiguity`,
  `TestDelayedReceiptKeepsAcceptanceEvidence`,
  `TestWaitingPrefixCannotStarveLaterIncident`,
  `TestBatchSupersessionPreservesInflightReservations`,
  `TestCriticalSurvivesMissingSubjectAndStaleStatus`,
  `TestLEDRuntimeChangesInvalidatePreparedHealth`, plus pinned daemon and
  generation/recovery/administration tests.
- Interactive: `TestNativeReplyRoutesToOneSyntheticWorkerThroughRealGateway`,
  `TestAcceptanceRevalidatesAssumptionsAndPreservesDetectionAcrossReplay`,
  `TestJobCoalescingCrashReplayAndLateScan`,
  `TestAcceptedFindingCannotClearAnotherFaultAndClockCannotExtendAcceptance`,
  `TestCommandScopeAndDestinationFencing`, `TestCommandGrantsAndOrderedResults`,
  `TestLatePreparationIsAcknowledgedWithoutReopening`,
  `TestTransitionBacklogIndependentOfFullSupport`, and context, result-floor,
  rejection and scoped-pagination tests. Vulnerability fixtures remain synthetic.

See `progress.md` for exact commands, logs, observed outcomes, current source-tree
identity and remaining external gates. A package test pass is evidence for these
fixtures, not proof that every normative external subcase has been observed.
