package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Budget changes preserve the exact disabled command authority and all queued
// work. Fresh delivery qualification cannot replace that authority.
func PreflightCommandDeliveryLimits(ctx context.Context, state string, id protocol.Identity, before, after Config, commandHistory string) error {
	if !safefs.DigestPattern.MatchString(commandHistory) {
		return fmt.Errorf("command budgets require an exact command history pin")
	}
	if err := validateEnrolledPolicyChange(31, before, after); err != nil {
		return err
	}
	return inspectEnrolledPolicyState(ctx, state, id, before, func(s *Store) error {
		if err := checkDeliveryCommandBudgetHead(s, commandHistory); err != nil {
			return err
		}
		return checkEnrolledPolicyCapacity(ctx, s, after, 31)
	})
}
func MigrateCommandDeliveryLimits(ctx context.Context, state string, id protocol.Identity, before, after Config, operation, commandHistory string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, operation, 31, nil, commandHistory)
}
func checkDeliveryCommandBudgetHead(s *Store, expected string) error {
	if s.config.Interaction == nil {
		return fmt.Errorf("command budget migration lost enrollment")
	}
	head, err := s.commandSQLHead()
	if err != nil {
		return err
	}
	if head == nil || head.Mode != "drain" || head.Identity != s.identity || expected != "" && head.Hash != expected {
		return fmt.Errorf("disable commands and retain their exact authority before changing budgets")
	}
	return s.checkManagedCommandHistory(head.Hash)
}
