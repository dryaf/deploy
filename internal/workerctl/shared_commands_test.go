package workerctl

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestSharedCommandsEnrollExistingReservation(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, mode := range []string{"read", "mutation"} {
		for _, boundary := range []string{"none", "shared-limits-intent", "shared-limits-input-published", "shared-limits-domain-complete", "scope-reply"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				f := newMappingFixture(t, false, true, false, false, true)
				original, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
				testutil.Must(t, err)
				raw, err := readLimitsRuntime(filepath.Join(f.paths.Config, "consumers/runtime.json"))
				testutil.Must(t, err)
				p := f.r.Desired.Notification.Providers["telegram"]
				p.Account, p.BotName = "123", "fixture_bot"
				f.r.Desired.Notification.Providers["telegram"] = p
				rights := []string{"read"}
				if mode == "mutation" {
					rights = append(rights, "incident.manage")
				}
				f.r.Desired.Notification.Commands = config.CommandDeclaration{Enabled: true, Mode: mode, Workers: map[string]string{"backup": "backup-worker"}, Conversations: map[string]config.ConversationDeclaration{"ops": {Provider: "telegram", Account: "123", NativeID: "456"}}, Permissions: []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "backup", Permissions: rights, Scope: config.PermissionScope{WholeWorker: true}}}}
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				updating := false
				activity := false
				providers := false
				calls := 0
				fired := false
				stop := errors.New("interrupted")
				receipts := map[string]string{}
				l := notificationRemappingLifecycle{notificationMappingLifecycle: notificationMappingLifecycle{inspect: f.inspect}}
				l.delivery.review = func(context.Context, string, string) (install.DeliveryAdmissionRequest, bool, error) {
					return install.DeliveryAdmissionRequest{}, false, nil
				}
				l.boundary = func(at string) error {
					if !fired && at == boundary {
						fired = true
						return stop
					}
					return nil
				}
				l.migrate = func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
					if expected != protocol.Hash(f.o.Manifest) {
						t.Fatal("manifest changed")
					}
					var before, after install.RuntimeInput
					testutil.Must(t, safefs.DecodeJSON(raw, &before, maxControlFrame))
					next, e := readLimitsRuntime(spec.RuntimeFile)
					testutil.Must(t, e)
					testutil.Must(t, safefs.DecodeJSON(next, &after, maxControlFrame))
					b, e := before.Resolve()
					testutil.Must(t, e)
					a, e := after.Resolve()
					testutil.Must(t, e)
					validate, migrate := install.ValidateManagedEnrolledCommands, worker.MigrateEnrolledCommands
					if updating {
						validate, migrate = install.ValidateManagedEnrolledCommandPolicy, worker.MigrateEnrolledCommandPolicy
					}
					if activity {
						validate, migrate = install.ValidateManagedEnrolledCommandActivity, worker.MigrateEnrolledCommandActivity
					}
					if providers {
						validate, migrate = install.ValidateManagedEnrolledProviders, worker.MigrateEnrolledProviders
					}
					testutil.Must(t, validate(f.o.Manifest, b, a))
					_, e = migrate(ctx, f.paths.State, f.o.Manifest.Identity, b.Worker, a.Worker, testutil.ID(t, "n"))
					testutil.Must(t, e)
					calls++
					raw = next
					f.o.Manifest.RuntimeHash, f.o.Manifest.ConfigHash = inputDigest(next), protocol.Hash(a)
					f.o.Manifest.Revision = testutil.ID(t, "n")
					f.o.Manifest.HelperRevision = f.o.Manifest.Revision
					return f.o.Manifest, nil
				}
				l.scope = func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, change func(backup.ProducerReservation) (string, error)) (string, error) {
					if pin := receipts[r.Operation]; pin != "" {
						return pin, nil
					}
					receipt, e := change(f.reservation)
					if e != nil {
						return "", e
					}
					if !fired && boundary == "scope-reply" {
						fired = true
						return "", stop
					}
					receipts[r.Operation] = receipt
					return receipt, nil
				}
				l.scopeInspect = func(_, op, _, _ string) (string, bool, error) { pin := receipts[op]; return pin, pin != "", nil }
				l.delivery.scopeInspect = l.scopeInspect
				result, handled, err := reconcileSharedCommands(ctx, f.r, f.paths, "/helper", l)
				if boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("boundary missed", result, err)
					}
					result, handled, err = reconcileSharedCommands(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || result.State != "verification" || calls != 1 {
					t.Fatal(result, handled, err, calls)
				}
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				testutil.Must(t, verifySharedLimitsReceipt(f.d, f.journal(t), f.o.Manifest, l.scopeInspect))
				saved, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
				testutil.Must(t, err)
				if string(saved) != string(original) {
					t.Fatal("original producer enrollment changed")
				}
				_, _, err = reconcileSharedCommands(ctx, f.r, f.paths, "/helper", l)
				testutil.Must(t, err)
				if calls != 1 {
					t.Fatal("replayed completed enrollment")
				}
				updating, fired = true, false
				f.r.Desired.Notification.Commands.Workers = map[string]string{"renamed": "backup-worker"}
				f.r.Desired.Notification.Commands.Conversations = map[string]config.ConversationDeclaration{"new-ops": {Provider: "telegram", Account: "123", NativeID: "654"}}
				f.r.Desired.Notification.Commands.Permissions = []config.PermissionDeclaration{{Actor: "987", Conversation: "new-ops", Worker: "renamed", Permissions: rights, Scope: config.PermissionScope{WholeWorker: true}}}
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				result, handled, err = reconcileSharedCommandPolicy(ctx, f.r, f.paths, "/helper", l)
				if boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("policy boundary missed", result, err)
					}
					result, handled, err = reconcileSharedCommandPolicy(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || result.State != "verification" || calls != 2 {
					t.Fatal(result, handled, err, calls)
				}
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				testutil.Must(t, verifySharedLimitsReceipt(f.d, f.journal(t), f.o.Manifest, l.scopeInspect))
				_, _, err = reconcileSharedCommandPolicy(ctx, f.r, f.paths, "/helper", l)
				testutil.Must(t, err)
				if calls != 2 {
					t.Fatal("replayed completed policy")
				}
				activity = true
				for n, enabled := range []bool{false, true} {
					fired = false
					f.r.Desired.Notification.Commands.Enabled = enabled
					f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
					testutil.Must(t, err)
					result, handled, err = reconcileSharedCommandActivity(ctx, f.r, f.paths, "/helper", l)
					if boundary != "none" {
						if !errors.Is(err, stop) || !handled {
							t.Fatal("activity boundary missed", enabled, result, err)
						}
						result, handled, err = reconcileSharedCommandActivity(ctx, f.r, f.paths, "/helper", l)
					}
					if err != nil || !handled || result.State != "verification" || calls != 3+n {
						t.Fatal(result, handled, err, calls)
					}
					testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
					testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
					testutil.Must(t, verifySharedLimitsReceipt(f.d, f.journal(t), f.o.Manifest, l.scopeInspect))
					_, _, err = reconcileSharedCommandActivity(ctx, f.r, f.paths, "/helper", l)
					testutil.Must(t, err)
					if calls != 3+n {
						t.Fatal("replayed completed activity")
					}
				}
				providers, activity, fired = true, false, false
				p = f.r.Desired.Notification.Providers["telegram"]
				p.TokenInput, p.ChatIDInput = "", ""
				p.TokenFile, p.ChatIDFile = filepath.Join(f.paths.Config, "command-token"), filepath.Join(f.paths.Config, "command-chat")
				testutil.Must(t, os.WriteFile(p.TokenFile, []byte("123:updated-command-token"), 0600))
				testutil.Must(t, os.WriteFile(p.ChatIDFile, []byte("654"), 0600))
				f.r.Desired.Notification.Providers = config.ProviderDeclarations{"telegram": p}
				f.r.Inputs = nil
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				result, handled, err = reconcileSharedProviders(ctx, f.r, f.paths, "/helper", l)
				if boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("provider boundary missed", result, err)
					}
					result, handled, err = reconcileSharedProviders(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || result.State != "verification" || calls != 5 {
					t.Fatal(result, handled, err, calls)
				}
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				testutil.Must(t, verifySharedLimitsReceipt(f.d, f.journal(t), f.o.Manifest, l.scopeInspect))

			})
		}
	}
}
