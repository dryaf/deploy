package workerctl

import (
	"context"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandEnrollmentRetainsAliasAcrossLostReplies(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		for _, boundary := range []string{"intent", "gateway", "scope", "scope-reply", "continuation"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				f := newCommandActivityFixture(t, mode)
				ctx := context.Background()
				f.toggle(false)
				_, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				testutil.Must(t, err)
				reviews := 0
				f.lifecycle.scopeEnrollment = f.lifecycle.scope
				f.lifecycle.reviewEnrollment = func(_ context.Context, root, manifest, policy string) (install.CommandPolicyRequest, error) {
					reviews++
					if f.manifest.Commands.Mode != "drain" || manifest != protocol.Hash(f.manifest) {
						t.Fatal("review used live or changed gateway")
					}
					var compiled interaction.Config
					if err := protocol.Decode([]byte(policy), &compiled, 16384); err != nil || compiled.Validate() != nil || compiled.Workers[0].Alias != "archive" {
						t.Fatal("review lost compiled alias", err)
					}
					return install.CommandPolicyRequest{Root: root, ManifestHash: manifest, Operation: "migrate-commands", Policy: policy}, nil
				}
				n := *f.r.Desired.Notification
				n.Commands.Workers = map[string]string{"archive": "backup-worker"}
				n.Commands.Permissions = []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "archive", Permissions: []string{"read"}, Scope: config.PermissionScope{Subjects: []string{"database", "cache"}}}}
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
				if !commandEnrollmentJournal(j) {
					t.Fatal("missing policy transition")
				}
				testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, j, f.manifest, f.lifecycle))
				f.boundary = ""
				n = *f.r.Desired.Notification
				n.Commands.Permissions = []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "archive", Permissions: []string{"read"}, Scope: config.PermissionScope{Subjects: []string{"database"}}}}
				f.r.Desired.Notification = &n
				f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
				result, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				if err != nil || result.State != "commands_disabled" || !commandEnrollmentJournal(f.journal(t)) {
					t.Fatal("permission edit lost overlay", result, err)
				}
				testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, f.journal(t), f.manifest, f.lifecycle))
				f.toggle(true)
				result, _, err = reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				if err != nil || result.State != "commands_admitted" || f.effects != 4 {
					t.Fatal("could not enable reduced policy", result, err)
				}
				testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, f.journal(t), f.manifest, f.lifecycle))
			})
		}
	}
}

func TestCommandEnrollmentRejectedReviewLeavesNoPendingOperation(t *testing.T) {
	f := newCommandActivityFixture(t, "read")
	ctx := context.Background()
	f.toggle(false)
	_, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
	testutil.Must(t, err)
	prior := f.journal(t)
	n := *f.r.Desired.Notification
	n.Commands.Workers = map[string]string{"archive": "backup-worker"}
	n.Commands.Permissions = []config.PermissionDeclaration{{Actor: "new-actor", Conversation: "ops", Worker: "archive", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}
	f.r.Desired.Notification = &n
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	rejected := errors.New("permission expansion requires separate admission")
	f.lifecycle.reviewEnrollment = func(context.Context, string, string, string) (install.CommandPolicyRequest, error) {
		return install.CommandPolicyRequest{}, rejected
	}
	_, handled, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
	if !handled || err == nil || f.journal(t).Digest() != prior.Digest() || f.effects != 1 {
		t.Fatal("rejected review retained effects", err)
	}
}

func TestCommandModeChangesRetainOriginalIncidentContract(t *testing.T) {
	f := newCommandActivityFixture(t, "mutation")
	ctx := context.Background()
	f.lifecycle.scopeEnrollment = f.lifecycle.scope
	f.lifecycle.scopePermissions = f.lifecycle.scope
	f.lifecycle.reviewPermissions = func(_ context.Context, root, manifest, policy string) (install.CommandPolicyRequest, error) {
		return install.CommandPolicyRequest{Root: root, ManifestHash: manifest, Operation: "narrow-commands", Policy: policy}, nil
	}
	f.lifecycle.reviewEnrollment = func(_ context.Context, root, manifest, policy string) (install.CommandPolicyRequest, error) {
		var c interaction.Config
		testutil.Must(t, protocol.Decode([]byte(policy), &c, 16384))
		if protocol.Hash(c.Workers[0].Commands) != protocol.Hash(f.receiver.Policy.Workers[0].Commands) {
			t.Fatal("mode change replaced immutable receiver contract")
		}
		if len(c.Permissions) != 1 || len(c.Permissions[0].Permissions) != 1 || c.Permissions[0].Permissions[0] != "read" {
			t.Fatal("mode change restored mutation rights")
		}
		return install.CommandPolicyRequest{Root: root, ManifestHash: manifest, Operation: "migrate-commands", Policy: policy}, nil
	}
	f.lifecycle.review = func(_ context.Context, root, manifest, mode string) (install.CommandPolicyRequest, bool, error) {
		expected := "read"
		if f.r.Desired.Notification.Commands.Mode == "mutation" {
			expected = "all"
		}
		if mode != expected {
			t.Fatal("admission used previous mode")
		}
		return install.CommandPolicyRequest{Root: root, ManifestHash: manifest, Operation: "enable-commands", Mode: mode, QualificationHash: protocol.Hash("current qualification")}, true, nil
	}
	apply := func(want string) {
		t.Helper()
		f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
		for range 2 {
			result, handled, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
			if err != nil || !handled || result.State != want {
				t.Fatal(result, handled, err)
			}
		}
		testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, f.journal(t), f.manifest, f.lifecycle))
	}
	f.toggle(false)
	apply("commands_disabled")
	n := *f.r.Desired.Notification
	n.Commands.Permissions = []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}
	f.r.Desired.Notification = &n
	apply("commands_disabled")
	for _, mode := range []string{"read", "mutation", "read"} {
		n = *f.r.Desired.Notification
		n.Commands.Mode = mode
		f.r.Desired.Notification = &n
		apply("commands_disabled")
		f.toggle(true)
		apply("commands_admitted")
		expected := "read"
		if mode == "mutation" {
			expected = "all"
		}
		if f.manifest.Commands.Mode != expected {
			t.Fatal("wrong admitted mode", f.manifest.Commands.Mode)
		}
		f.toggle(false)
		apply("commands_disabled")
	}
}

func TestCommandModeUpgradeRequiresOriginalReceiverCapability(t *testing.T) {
	f := newCommandActivityFixture(t, "read")
	f.toggle(false)
	_, _, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
	testutil.Must(t, err)
	prior := f.journal(t)
	n := *f.r.Desired.Notification
	n.Commands.Mode = "mutation"
	f.r.Desired.Notification = &n
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	_, handled, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
	if err == nil || !handled || f.journal(t).Digest() != prior.Digest() || f.effects != 1 {
		t.Fatal("unsupported receiver gained mode or pending journal", err)
	}
}
