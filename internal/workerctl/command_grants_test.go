package workerctl

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandPermissionRestoreRetainsExplicitReview(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		for _, boundary := range []string{"intent", "gateway", "scope", "scope-reply", "continuation"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				f := newCommandActivityFixture(t, mode)
				ctx := context.Background()
				original := f.r.Desired.Notification.Commands.Permissions
				f.lifecycle.scopePermissions = f.lifecycle.scope
				f.lifecycle.scopeEnrollment = f.lifecycle.scope
				f.lifecycle.reviewPermissions = func(_ context.Context, root, manifest, policy string) (install.CommandPolicyRequest, error) {
					return install.CommandPolicyRequest{Root: root, ManifestHash: manifest, Operation: "narrow-commands", Policy: policy}, nil
				}
				reviews := 0
				f.lifecycle.reviewRestore = func(_ context.Context, root, manifest, policy string) (install.CommandPolicyRequest, error) {
					reviews++
					var c interaction.Config
					testutil.Must(t, protocol.Decode([]byte(policy), &c, 16384))
					if !commandRightsSubset(&f.receiver.Policy, &c) || len(c.Permissions) == 0 || f.manifest.Commands.Mode != "drain" {
						t.Fatal("invalid restoration review")
					}
					return install.CommandPolicyRequest{Root: root, ManifestHash: manifest, Operation: "restore-command-permissions", Policy: policy}, nil
				}
				f.toggle(false)
				_, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				testutil.Must(t, err)
				n := *f.r.Desired.Notification
				n.Commands.Permissions = []config.PermissionDeclaration{}
				f.r.Desired.Notification = &n
				f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
				_, _, err = reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				testutil.Must(t, err)
				n = *f.r.Desired.Notification
				n.Commands.Permissions = original
				f.r.Desired.Notification = &n
				f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
				f.boundary, f.fired = boundary, false
				_, handled, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				if !handled || err == nil || !f.fired {
					t.Fatal("missed restoration boundary", err)
				}
				for range 2 {
					result, handled, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
					if err != nil || !handled || result.State != "commands_disabled" {
						t.Fatal(result, err)
					}
				}
				if reviews != 1 || f.effects != 3 {
					t.Fatal("repeated review/effect", reviews, f.effects)
				}
				testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, f.journal(t), f.manifest, f.lifecycle))
				f.boundary = ""
				f.toggle(true)
				result, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				if err != nil || result.State != "commands_admitted" {
					t.Fatal(result, err)
				}
				testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, f.journal(t), f.manifest, f.lifecycle))
			})
		}
	}
}
