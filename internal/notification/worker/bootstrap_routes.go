package worker

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Routes share the bounded bootstrap policy chain with limits. Version 1/2
// receipts remain immutable; version 3/4 explicitly identify route-only changes.
// Neither route changes nor retries can cross real producer/message history.
func ValidateBootstrapRoutesChange(before, after Config) error {
	if err := before.ValidateBootstrap(); err != nil {
		return err
	}
	if err := after.ValidateBootstrap(); err != nil {
		return err
	}
	next := before
	next.Routes = after.Routes
	if protocol.Hash(next) != protocol.Hash(after) {
		return fmt.Errorf("route migration must retain limits, enrollment and other policy")
	}
	return nil
}
func validateBootstrapPolicyChange(kind string, before, after Config) error {
	switch kind {
	case "":
		return ValidateBootstrapLimitsChange(before, after)
	case "routes":
		return ValidateBootstrapRoutesChange(before, after)
	default:
		return fmt.Errorf("unknown bootstrap policy transition")
	}
}
func PreflightBootstrapRoutes(ctx context.Context, state string, id protocol.Identity, before, after Config) error {
	return preflightBootstrapPolicy(ctx, state, id, before, after, "routes")
}
func MigrateBootstrapRoutes(ctx context.Context, state string, id protocol.Identity, before, after Config, operation string) (BootstrapLimitsReceipt, error) {
	return migrateBootstrapPolicy(ctx, state, id, before, after, operation, "routes", nil)
}
