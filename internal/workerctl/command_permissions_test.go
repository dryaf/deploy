package workerctl

import (
	"context"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandPermissionsRetainReducedPolicyAcrossLostReplies(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		for _, boundary := range []string{"intent", "gateway", "scope", "scope-reply", "continuation"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				f := newCommandActivityFixture(t, mode)
				ctx := context.Background()
				f.toggle(false)
				_, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				testutil.Must(t, err)
				reviews := 0
				f.lifecycle.scopePermissions = f.lifecycle.scope
				f.lifecycle.reviewPermissions = func(_ context.Context, root, manifest, policy string) (install.CommandPolicyRequest, error) {
					reviews++
					if f.manifest.Commands.Mode != "drain" || manifest != protocol.Hash(f.manifest) {
						t.Fatal("review used live or changed gateway")
					}
					return install.CommandPolicyRequest{Root: root, ManifestHash: manifest, Operation: "narrow-commands", Policy: policy}, nil
				}
				n := *f.r.Desired.Notification
				n.Commands.Permissions = []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{Subjects: []string{"database"}}}}
				f.r.Desired.Notification = &n
				f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
				f.boundary, f.fired = boundary, false
				_, handled, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				if !handled || err == nil || !f.fired {
					t.Fatal("missed policy boundary", handled, err)
				}
				for range 2 {
					result, handled, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
					if err != nil || !handled || result.State != "commands_disabled" {
						t.Fatal(result, handled, err)
					}
				}
				if reviews != 1 || f.effects != 2 {
					t.Fatal("repeated policy review or effect", reviews, f.effects)
				}
				j := f.journal(t)
				if !commandPermissionsJournal(j) {
					t.Fatal("missing policy transition")
				}
				testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, j, f.manifest, f.lifecycle))
				f.boundary = ""
				f.toggle(true)
				result, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				if err != nil || result.State != "commands_admitted" || f.effects != 3 {
					t.Fatal("could not enable reduced policy", result, err)
				}
				testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, f.journal(t), f.manifest, f.lifecycle))
			})
		}
	}
}

func TestCommandPermissionsRejectedReviewLeavesNoPendingOperation(t *testing.T) {
	f := newCommandActivityFixture(t, "read")
	ctx := context.Background()
	f.toggle(false)
	_, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
	testutil.Must(t, err)
	prior := f.journal(t)
	n := *f.r.Desired.Notification
	n.Commands.Permissions = []config.PermissionDeclaration{{Actor: "new-actor", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}
	f.r.Desired.Notification = &n
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	rejected := errors.New("permission expansion requires separate admission")
	f.lifecycle.reviewPermissions = func(context.Context, string, string, string) (install.CommandPolicyRequest, error) {
		return install.CommandPolicyRequest{}, rejected
	}
	_, handled, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
	if !handled || err == nil || f.journal(t).Digest() != prior.Digest() || f.effects != 1 {
		t.Fatal("rejected review retained effects", err)
	}
}
