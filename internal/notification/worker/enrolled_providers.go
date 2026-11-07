package worker

import (
	"context"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Provider credentials live in the managed runtime. This receipt fences its
// route-policy handoff even when a same-account token rotation leaves routes
// unchanged. No pending delivery may cross the provider change.
func PreflightEnrolledProviders(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightEnrolledPolicy(ctx, state, id, before, after, 5)
}
func MigrateEnrolledProviders(ctx context.Context, state string, id protocol.Identity, before, after Config, op string) (EnrolledLimitsReceipt, error) {
	return migrateEnrolledPolicy(ctx, state, id, before, after, op, 5, nil)
}
