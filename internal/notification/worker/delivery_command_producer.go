package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Producer policy changes preserve the exact disabled command authority and all queued
// work. Fresh delivery qualification cannot replace that authority.
func PreflightCommandDeliveryProducerPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, commandHistory string) error {
	if !safefs.DigestPattern.MatchString(commandHistory) {
		return fmt.Errorf("command policy changes require an exact command history pin")
	}
	if err := validateEnrolledPolicyChange(34, before, after); err != nil {
		return err
	}
	return inspectEnrolledPolicyState(ctx, state, id, before, func(s *Store) error {
		if err := checkDisabledDeliveryCommandHead(s, commandHistory); err != nil {
			return err
		}
		return checkEnrolledPolicyCapacity(ctx, s, after, 34)
	})
}
func MigrateCommandDeliveryProducerPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, operation, commandHistory string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, operation, 34, nil, commandHistory)
}

// Declaration-only changes retain the exact config, owner and disabled command
// head. Reading this proof does not acquire a runtime lease or alter the store.
func PreflightCommandDeliveryDeclaration(ctx context.Context, state string, id protocol.Identity, c Config, commandHistory string) error {
	if c.Interaction == nil || !safefs.DigestPattern.MatchString(commandHistory) {
		return fmt.Errorf("command declaration requires its original enrollment and exact disabled head")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	return inspectEnrolledPolicyState(ctx, state, id, c, func(s *Store) error {
		if err := checkDisabledDeliveryCommandHead(s, commandHistory); err != nil {
			return err
		}
		var owner ManagedDeliveryOwner
		if err := s.dir.ReadJSON(managedDeliveryOwnerFile, &owner); err != nil {
			return err
		}
		return checkManagedDeliveryOwnerRecord(s.dir, s.db, c, owner, protocol.Hash(owner), id)
	})
}
