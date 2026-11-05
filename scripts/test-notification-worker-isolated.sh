#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${DEPLOY_LED_FIXTURE:?Set DEPLOY_LED_FIXTURE to a clean offline pi-status-led checkout at the pinned commit}"
# Retained SQLite proof audits and real crash/resume fixtures are substantially
# slower under the race detector than ordinary tests. Bound the complete suite
# explicitly instead of relying on Go's ten-minute package default.
go test -race -timeout=90m ./internal/notification/...
go test -race ./internal/backup -run '^TestBackup(Shared|LEDRecovery|ReconciliationCLI|RecoveryHandoff)'
go test -race -tags=notificationintegration -count=1 -timeout=90s \
  -run '^TestBackup(LEDRecovery|Reconciliation|RecoveryHandoff)NativeWorkflow$' ./internal/backup
go test -race -tags=notificationintegration -count=1 -timeout=60s \
  -run '^Test(PinnedMockDaemon|InheritedLEDOwnershipPinnedDaemon|ProducerRetirementPinnedDaemon|LEDFaultClearPinnedDaemon|RecoveryLEDCleanupPinnedDaemon|RecoveryLEDPreservationAndAdoptionPinnedDaemon|VerificationModePinnedDaemon)$' \
  ./internal/notification/led ./internal/notification/worker
go test -race -tags=notificationintegration -count=1 -timeout=120s \
  -run '^Test(ManagedNativeExecutableLifecycle|ManagedDeliveryNativeExecutable|ActivationReviewNativeExecutable|ManagedRecoveryNativeExecutable|ManagedSignalNativeExecutable|SignalServiceNativeExecutable|ManagedTelegramNativeExecutable|ManagedSchemaNativeExecutable|ManagedLEDMappingNativeExecutable|SignalLinkNativeExecutable|SignalRuntimeInstallNativeExecutable|SignalRecoveryFenceNativeExecutable|SignalVerificationNativeExecutable|NativeSignalHandoffNativeExecutable|NativeSignalExecPrivateBus|PodmanSignalQuadletContract|TelegramHandoffCheckNativeExecutable|TelegramRenewalCheckNativeExecutable|ProducerRetirementNativeExecutable|RecoveryFenceNativeExecutable|RecoveryQuarantineNativeExecutable|RecoveryStatusNativeExecutable|RecoveryCheckpointNativeExecutable|RecoveryCheckpointVerifyNativeExecutable|RecoveryPublicationNativeExecutable|RecoveryPublicationVerifyNativeExecutable|RecoveryPublishNativeExecutable|RecoveryLEDPreservationAndAdoptionNativeExecutable)$' ./internal/notification/install

# The opt-in test owns a separate VFS store and builds only a local scratch
# fixture; no existing images/containers, live accounts or user units are used.
if [[ ${DEPLOY_PODMAN_FIXTURE:-0} == 1 ]]; then
  go test -race -tags=notificationintegration -count=1 -timeout=8m \
    -run '^TestPodmanSignalHandoffIsolatedContainer$' ./internal/notification/install
fi
