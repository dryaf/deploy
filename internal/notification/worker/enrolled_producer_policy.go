package worker

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Producer policy changes retain every enrolled identity. New identities and
// retirement require their own admission/disposition, even in verification.
func ValidateEnrolledProducerPolicyChange(before, after Config) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if len(before.Producers) != len(after.Producers) {
		return fmt.Errorf("producer policy cannot add or retire identities")
	}
	for i, p := range before.Producers {
		if p.Producer != after.Producers[i].Producer {
			return fmt.Errorf("producer policy cannot replace enrolled identities")
		}
	}
	next := before
	next.Producers = after.Producers
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("change producer policy separately from global budgets, routes and commands")
	}
	return nil
}
func producerPolicyNeedsSettledQueue(before, after Config) bool {
	projected := after
	projected.Producers = append([]Enrollment(nil), after.Producers...)
	for i, p := range before.Producers {
		projected.Producers[i].MaxMessages = p.MaxMessages
		projected.Producers[i].MaxSilenceSeconds = p.MaxSilenceSeconds
		projected.Producers[i].MaxEvidenceSeconds = p.MaxEvidenceSeconds
	}
	return protocol.Hash(projected) != protocol.Hash(before)
}
func PreflightEnrolledProducerPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 4)
}
func MigrateEnrolledProducerPolicy(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 4, nil)
}
