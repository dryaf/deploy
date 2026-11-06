#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${DEPLOY_LED_FIXTURE:?Set DEPLOY_LED_FIXTURE to a clean offline pi-status-led checkout at the pinned commit}"
# Retained SQLite proof audits and real crash/resume fixtures are substantially
# slower under the race detector than ordinary tests. Bound the complete suite
# explicitly instead of relying on Go's ten-minute package default.
go test -race -timeout=90m ./internal/notification/...
go test -race -timeout=30m ./internal/backup -run '^Test(Backup(Shared|LEDRecovery|ReconciliationCLI|RecoveryHandoff|RecoveryActivation|Repeated|NextRecovery|MigrationCycle|SignalMigration|Cutover|LegacyResolution|LegacyRollback|Remigration)|Legacy(BackupSignal|SignalAdoption)|SharedReport)'
go test -race -tags=notificationintegration -count=1 -timeout=5m \
  -run '^TestBackup((LEDRecovery|Reconciliation|RecoveryHandoff)NativeWorkflow|RecoveryActivationNativeHelper)$' ./internal/backup
go test -race -tags=notificationintegration -count=1 -timeout=5m \
  -run '^Test(PinnedMockDaemon|InheritedLEDOwnershipPinnedDaemon|ProducerRetirementPinnedDaemon|LEDFaultClearPinnedDaemon|RecoveryLEDCleanupPinnedDaemon|RecoveryLEDPreservationAndAdoptionPinnedDaemon|VerificationModePinnedDaemon|RuntimeLEDRecovery(SIGKILL|Socket)PinnedDaemon)$' \
  ./internal/notification/led ./internal/notification/worker
# This group builds/runs native helpers and audits retained recovery proofs.
# The aggregate exceeds two minutes under -race even without live providers.
go test -race -tags=notificationintegration -count=1 -timeout=15m \
  -run '^Test(InstallationRetirementNativeReplacement|ManagedNativeExecutableLifecycle|ManagedDeliveryNativeExecutable|ActivationReviewNativeExecutable|ManagedRecoveryNativeExecutable|ManagedSignalNativeExecutable|SignalServiceNativeExecutable|ManagedTelegramNativeExecutable|ManagedSchemaNativeExecutable|ManagedLEDMappingNativeExecutable|SignalLinkNativeExecutable|SignalRuntimeInstallNativeExecutable|SignalRecoveryFenceNativeExecutable|SignalVerificationNativeExecutable|NativeSignalHandoffNativeExecutable|NativeSignalExecPrivateBus|PodmanSignalQuadletContract|TelegramHandoffCheckNativeExecutable|TelegramRenewalCheckNativeExecutable|ProducerRetirementNativeExecutable|RecoveryFenceNativeExecutable|RecoveryQuarantineNativeExecutable|RecoveryStatusNativeExecutable|RecoveryCheckpointNativeExecutable|RecoveryCheckpointVerifyNativeExecutable|RecoveryPublicationNativeExecutable|RecoveryPublicationVerifyNativeExecutable|RecoveryPublishNativeExecutable|RecoveryLEDPreservationAndAdoptionNativeExecutable)$' ./internal/notification/install

# Production admission and recovered intake use offline native executables and
# synthetic provider transports. Include these independently of the original
# verification-only lifecycle group so new production paths remain release gates.
go test -race -tags=notificationintegration -count=1 -timeout=30m \
  -run '^Test(ManagedRecoveredTelegramDeliveryNative|NativeManagedSignalDeliveryAndCommands|NativeManagedSignalRepeatedRecovery|NativeLegacySignalConversationDelivery|NativeSignalProductionService|NativeSignalCommandSupervisorJoinsDescendants|NativeManagedLEDDelivery)$' \
  ./internal/notification/install

# The opt-in test owns a separate VFS store and builds only a local scratch
# fixture; no existing images/containers, live accounts or user units are used.
if [[ ${DEPLOY_PODMAN_FIXTURE:-0} == 1 ]]; then
  go test -race -tags=notificationintegration -count=1 -timeout=8m \
    -run '^TestPodmanSignalHandoffIsolatedContainer$' ./internal/notification/install
fi
