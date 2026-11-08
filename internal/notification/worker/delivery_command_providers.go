package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Provider changes preserve the exact disabled command authority and all queued
// work. Fresh delivery qualification cannot replace that authority.
func PreflightCommandDeliveryProviders(ctx context.Context, state string, id protocol.Identity, before, after Config, commandHistory string) error {
	if !safefs.DigestPattern.MatchString(commandHistory) {
		return fmt.Errorf("command policy changes require an exact command history pin")
	}
	if err := validateEnrolledPolicyChange(35, before, after); err != nil {
		return err
	}
	return inspectEnrolledPolicyState(ctx, state, id, before, func(s *Store) error {
		if err := checkDisabledDeliveryCommandHead(s, commandHistory); err != nil {
			return err
		}
		return checkEnrolledPolicyCapacity(ctx, s, after, 35)
	})
}
func MigrateCommandDeliveryProviders(ctx context.Context, state string, id protocol.Identity, before, after Config, operation, commandHistory string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, operation, 35, nil, commandHistory)
}
