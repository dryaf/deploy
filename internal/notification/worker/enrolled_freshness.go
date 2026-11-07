package worker

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Timing policy changes retain the last observations and their timestamps. They
// alter evaluation thresholds, never fabricate a current heartbeat or evidence.
func ValidateEnrolledFreshnessChange(before, after Config) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if len(before.Producers) != len(after.Producers) {
		return fmt.Errorf("freshness updates require unchanged enrollment")
	}
	next := before
	next.Producers = append([]Enrollment(nil), before.Producers...)
	for i := range next.Producers {
		next.Producers[i].MaxSilenceSeconds, next.Producers[i].MaxEvidenceSeconds = after.Producers[i].MaxSilenceSeconds, after.Producers[i].MaxEvidenceSeconds
	}
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("change heartbeat and evidence freshness separately from quotas, routes, identities and command policy")
	}
	return nil
}
func validateEnrolledPolicyChange(version int, before, after Config) error {
	if version == 31 {
		if before.Interaction == nil || after.Interaction == nil {
			return fmt.Errorf("command budgets require retained enrollment")
		}
		return ValidateEnrolledLimitsChange(before, after)
	}

	if version == 21 || version == 24 || version == 23 || version == 25 {
		if before.Interaction != nil || after.Interaction != nil {
			return fmt.Errorf("production limit migration excludes command policy")
		}
		if version == 23 || version == 25 {
			return ValidateEnrolledRoutesChange(before, after)
		}
		if version == 24 {
			return ValidateEnrolledProducerPolicyChange(before, after)
		}
		return ValidateEnrolledLimitsChange(before, after)
	}
	if version == 13 {
		return ValidateEnrolledCommandActivityChange(before, after)
	}
	if version == 12 {
		return ValidateEnrolledCommandPolicyChange(before, after)
	}
	if version == 11 {
		return ValidateEnrolledCommandsChange(before, after)
	}
	if version == 4 {
		return ValidateEnrolledProducerPolicyChange(before, after)
	}
	if version == 3 || version == 5 {
		return ValidateEnrolledRoutesChange(before, after)
	}
	if version == 2 {
		return ValidateEnrolledFreshnessChange(before, after)
	}
	if version != 1 {
		return fmt.Errorf("unknown enrolled policy version")
	}
	return ValidateEnrolledLimitsChange(before, after)
}
func PreflightEnrolledFreshness(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 2)
}
func MigrateEnrolledFreshness(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 2, nil)
}
