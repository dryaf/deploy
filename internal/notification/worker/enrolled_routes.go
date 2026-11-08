package worker

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// A routing edit never retargets pending work. The caller must remain in
// verification, with all producer generations inactive and the queue settled.
func ValidateEnrolledRoutesChange(before, after Config) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	next := before
	next.Routes = after.Routes
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("change routes separately from providers, enrollment, budgets and timing")
	}
	return nil
}
func checkEnrolledPolicyCapacity(ctx context.Context, s *Store, after Config, version int) error {
	if version == 31 || version == 32 || version == 33 || version == 34 || version == 35 {
		if err := checkDisabledDeliveryCommandHead(s, ""); err != nil {
			return err
		}
		if err := checkLimitsCapacity(ctx, s, after, true, true); err != nil {
			return err
		}
		if version == 33 || version == 35 && protocol.Hash(s.config.Routes) != protocol.Hash(after.Routes) || version == 34 && producerPolicyNeedsSettledQueue(s.config, after) {
			var pending int
			if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE state NOT IN ('provider_accepted','expired','superseded','cancelled')").Scan(&pending); err != nil {
				return err
			}
			if pending != 0 {
				return fmt.Errorf("settle pending messages before changing producer access policy; command requests/results remain retained")
			}
		}
		return nil
	}

	if version == 21 || version == 24 || version == 23 || version == 25 {
		if err := checkLimitsCapacity(ctx, s, after, true); err != nil {
			return err
		}
		if version == 25 || version == 23 || version == 24 && producerPolicyNeedsSettledQueue(s.config, after) {
			var pending int
			if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE state NOT IN ('provider_accepted','expired','superseded','cancelled')").Scan(&pending); err != nil {
				return err
			}
			if pending != 0 {
				return fmt.Errorf("settle pending messages before changing production routes or producer access policy")
			}
		}
		return nil
	}
	if version == 13 {
		if err := checkVerificationCommandActivity(ctx, s, after); err != nil {
			return err
		}
	}
	if version == 12 {
		if err := checkVerificationCommandState(ctx, s, after); err != nil {
			return err
		}
	}
	if version == 11 {
		if err := checkInitialEnrolledCommandState(ctx, s, after); err != nil {
			return err
		}
	}
	if version == 5 && s.config.Interaction != nil {
		if err := requireEmptyCommandHistory(ctx, s); err != nil {
			return err
		}
	}
	if err := checkEnrolledLimitsCapacity(ctx, s, after); err != nil {
		return err
	}
	if version == 3 || version == 5 || version == 4 && producerPolicyNeedsSettledQueue(s.config, after) {
		var pending int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE state NOT IN ('provider_accepted','expired','superseded','cancelled')").Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return fmt.Errorf("settle pending notification messages before changing shared routes or producer access policy")
		}
	}
	return nil
}
func PreflightEnrolledRoutes(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 3)
}
func MigrateEnrolledRoutes(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 3, nil)
}
