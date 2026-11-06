package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/safefs"
)

func openPinnedSignalReconciler(ctx context.Context, path string, c Config, operation string) (*recoveryReconciler, error) {
	if !safefs.ValidID(operation, "n") {
		return nil, fmt.Errorf("Signal reconciliation requires exact recovery operation")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	if r.journal.Operation != operation {
		r.Close()
		return nil, fmt.Errorf("Signal recovery operation changed")
	}
	if err = r.initializeSignalRecovery(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func OpenPinnedSignalRecoverySession(ctx context.Context, path string, c Config, operation string) (interaction.SignalRecoverySession, error) {
	return openPinnedSignalReconciler(ctx, path, c, operation)
}

func OpenPinnedSignalCursorSession(ctx context.Context, path string, c Config, operation string) (interaction.SignalCursorSession, error) {
	r, err := openPinnedSignalReconciler(ctx, path, c, operation)
	if err != nil {
		return nil, err
	}
	if err = r.initializeSignalHandoff(ctx); err == nil {
		err = r.initializeSignalCursor(ctx)
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// SealPinnedRecoveryRequests freezes the imported mappings. It never enables
// intake; the caller must finish available provider and command reconciliation.
func SealPinnedRecoveryRequests(ctx context.Context, path string, c Config, operation string) (RecoveryRequestSummary, error) {
	if !safefs.ValidID(operation, "n") {
		return RecoveryRequestSummary{}, fmt.Errorf("request sealing requires exact recovery operation")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return RecoveryRequestSummary{}, err
	}
	defer r.Close()
	if r.journal.Operation != operation {
		return RecoveryRequestSummary{}, fmt.Errorf("request sealing recovery operation changed")
	}
	if err = r.initializeRequestRecovery(ctx); err != nil {
		return RecoveryRequestSummary{}, err
	}
	return r.SealRecoveryRequests(ctx)
}

// CommitPinnedTelegramRecoveryCursor retains the completed drain and request
// seal. The operation check and cursor commit share the same offline lease.
func CommitPinnedTelegramRecoveryCursor(ctx context.Context, path string, c Config, operation, account string) (TelegramRecoveryCursor, error) {
	if !safefs.ValidID(operation, "n") {
		return TelegramRecoveryCursor{}, fmt.Errorf("Telegram cursor requires exact recovery operation")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return TelegramRecoveryCursor{}, err
	}
	defer r.Close()
	if r.journal.Operation != operation {
		return TelegramRecoveryCursor{}, fmt.Errorf("Telegram cursor recovery operation changed")
	}
	if err = r.initializeTelegramCursorRecovery(ctx); err != nil {
		return TelegramRecoveryCursor{}, err
	}
	return r.CommitTelegramRecoveryCursor(ctx, account)
}
