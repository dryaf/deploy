package workerctl

import (
	"context"
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
)

func defaultSharedCommands() notificationRemappingLifecycle {
	l := defaultSharedLimits()
	l.migrate = install.MigrateEnrolledCommands
	l.scope = backup.ReconcileVerificationCommandsScope
	return l
}
func reconcileSharedCommands(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
	return reconcileSharedPolicy(ctx, r, paths, helper, l, "commands")
}

func defaultSharedCommandPolicy() notificationRemappingLifecycle {
	l := defaultSharedLimits()
	l.migrate = install.MigrateEnrolledCommandPolicy
	l.scope = backup.ReconcileVerificationCommandPolicyScope
	return l
}
func reconcileSharedCommandPolicy(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
	return reconcileSharedPolicy(ctx, r, paths, helper, l, "command-policy")
}

func defaultSharedCommandActivity() notificationRemappingLifecycle {
	l := defaultSharedLimits()
	l.migrate = install.MigrateEnrolledCommandActivity
	l.scope = backup.ReconcileVerificationCommandActivityScope
	return l
}
func reconcileSharedCommandActivity(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
	return reconcileSharedPolicy(ctx, r, paths, helper, l, "command-activity")
}
