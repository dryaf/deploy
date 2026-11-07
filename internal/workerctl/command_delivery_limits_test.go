package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type commandBudgetFixture struct {
	*mappingFixture
	provider                                    deliveryProviderLifecycle
	renewal                                     commandRequalificationLifecycle
	commands                                    commandActivityLifecycle
	receiver                                    backup.CommandReceiverRequest
	commandRequests                             map[string]install.ManagedManifest
	paused, rotated, admitted                   map[string]install.ManagedManifest
	scopes                                      map[string]string
	boundary                                    string
	fired, ready, receiverChanged               bool
	pauses, rotations, renewals, receiverChecks int
}

func (f *commandBudgetFixture) fail(at string) error {
	if at == f.boundary && !f.fired {
		f.fired = true
		return errors.New("lost " + at + " reply")
	}
	return nil
}
func newCommandBudgetFixture(t *testing.T, mode string) *commandBudgetFixture {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	m := newMappingFixtureWithOptions(t, mappingFixtureOptions{configure: func(r *ControlRequest) {
		declareFixtureReadCommands(t, r)
		r.Desired.Notification.Commands.Mode = mode
		if mode == "mutation" {
			r.Desired.Notification.Commands.Permissions[0].Permissions = []string{"read", "incident.manage"}
		}
	}}, false, true, false, false, true)
	f := &commandBudgetFixture{mappingFixture: m, commandRequests: map[string]install.ManagedManifest{}, paused: map[string]install.ManagedManifest{}, rotated: map[string]install.ManagedManifest{}, admitted: map[string]install.ManagedManifest{}, scopes: map[string]string{}}
	ctx := context.Background()
	delivery := notificationDeliveryLifecycle{
		review: func(_ context.Context, root, hash string) (install.DeliveryAdmissionRequest, bool, error) {
			return install.DeliveryAdmissionRequest{Root: root, ManifestHash: hash, QualificationHash: protocol.Hash(fmt.Sprint("qualification", f.renewals))}, f.ready, nil
		},
		inspect: func(_ context.Context, r install.DeliveryAdmissionRequest) (install.ManagedManifest, bool, error) {
			m, ok := f.admitted[protocol.Hash(r)]
			return m, ok, nil
		},
		ensure: func(_ context.Context, r install.DeliveryAdmissionRequest) (install.ManagedManifest, error) {
			if r.ManifestHash != protocol.Hash(f.o.Manifest) {
				t.Fatal("unreviewed activation")
			}
			f.o.Manifest.Mode = "delivery"
			f.o.Manifest.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: testutil.ID(t, "n"), Hash: protocol.Hash(r)}
			f.o.Manifest.DeliveryOwnerHash = protocol.Hash("retained owner")
			f.o.Manifest.UnitHash = protocol.Hash("delivery unit")
			f.admitted[protocol.Hash(r)] = f.o.Manifest
			f.renewals++
			return f.o.Manifest, f.fail("delivery-reply")
		},
		connect: func(context.Context, string, string, string) error { return f.fail("connect-reply") },
	}
	prior := f.journal(t)
	pins := prior.Pins
	pins.PreviousOperation, pins.Previous = prior.ID, prior.Digest()
	pins.Artifacts = map[string]string{"helper": f.r.HelperSHA256, "qualification": protocol.Hash("initial qualification")}
	step := &notificationDeliveryStep{request: install.DeliveryAdmissionRequest{Root: f.paths.Installation, ManifestHash: protocol.Hash(f.o.Manifest), QualificationHash: pins.Artifacts["qualification"]}, lifecycle: delivery}
	_, err := (JournalCoordinator{Directory: f.d}).Apply(ctx, pins, []LifecycleStep{step})
	testutil.Must(t, err)
	runtime, err := install.ReadRuntime(filepath.Join(f.paths.Config, "consumers/runtime.json"))
	testutil.Must(t, err)
	command := notificationCommandLifecycle{
		review: func(_ context.Context, root, hash, mode string) (install.CommandPolicyRequest, bool, error) {
			return install.CommandPolicyRequest{Root: root, ManifestHash: hash, Mode: mode, Operation: "enable-commands", QualificationHash: protocol.Hash("initial qualification")}, true, nil
		},
		receiverReview: func(_ context.Context, _, _, _ string, op string) (backup.CommandReceiverRequest, error) {
			version := 1
			if mode == "mutation" {
				version = 2
			}
			return backup.CommandReceiverRequest{Version: version, Operation: op, Identity: f.o.Manifest.Identity, Producer: f.reservation.Producer, Policy: *runtime.Worker.Interaction}, nil
		},
		receiverInspect: func(_ context.Context, r backup.CommandReceiverRequest) (string, bool, error) {
			f.receiverChecks++
			if f.receiverChanged {
				return "", false, errors.New("receiver authority changed")
			}
			if f.receiver.Operation == "" {
				return "", false, nil
			}
			if protocol.Hash(r) != protocol.Hash(f.receiver) {
				return "", false, errors.New("receiver request changed")
			}
			return protocol.Hash(r), true, nil
		},
		receiverStage: func(_ context.Context, r backup.CommandReceiverRequest) (string, error) {
			f.receiver = r
			return protocol.Hash(r), nil
		},
		inspect: func(_ context.Context, r install.CommandPolicyRequest) (install.ManagedManifest, bool, error) {
			m, ok := f.commandRequests[protocol.Hash(r)]
			return m, ok, nil
		},
		ensure: func(_ context.Context, r install.CommandPolicyRequest) (install.ManagedManifest, error) {
			if r.ManifestHash != protocol.Hash(f.o.Manifest) {
				t.Fatal("unreviewed command change")
			}
			mode := r.Mode
			if r.Operation != "enable-commands" {
				mode = "drain"
			}
			f.o.Manifest.Commands = &install.ManagedCommandAdmission{Mode: mode, Hash: protocol.Hash(r)}
			f.o.Manifest.CommandHistoryHash = protocol.Hash(r)
			f.commandRequests[protocol.Hash(r)] = f.o.Manifest
			return f.o.Manifest, nil
		}, connect: delivery.connect,
	}
	_, err = advanceNotificationCommands(ctx, f.r, f.paths, f.d, f.journal(t), ControlResult{}, command)
	testutil.Must(t, err)
	inspectScope := func(_, op, _, _ string) (string, bool, error) { v, ok := f.scopes[op]; return v, ok, nil }
	f.commands = commandActivityLifecycle{notificationCommandLifecycle: command, inputs: func(ControlRequest, ResolvedPaths, *safefs.Directory, config.DesiredWorkers) error { return nil }, scopeInspect: inspectScope, scope: func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, change func(backup.ProducerReservation) (string, error)) (string, error) {
		if v := f.scopes[r.Operation]; v != "" {
			return v, nil
		}
		v, e := change(f.reservation)
		if e == nil {
			f.scopes[r.Operation] = v
		}
		return v, e
	}}
	f.toggle(t, false)
	_, _, err = reconcileCommandActivity(ctx, f.r, f.paths, f.commands)
	testutil.Must(t, err)
	f.provider = deliveryProviderLifecycle{kind: "command-limits", inspect: f.inspect, delivery: delivery, scopeInspect: inspectScope,
		inputs: func(context.Context, ControlRequest, ResolvedPaths, *safefs.Directory, OperationJournal) error {
			return nil
		},
		commandReceiver: func(ctx context.Context, d *safefs.Directory, j OperationJournal) error {
			_, e := retainedCommandReceiver(ctx, d, j, command)
			return e
		},
		review: func(_ context.Context, root, hash string, raw []byte) (install.DeliveryProviderReview, error) {
			if root != f.paths.Installation || hash != protocol.Hash(f.o.Manifest) {
				t.Fatal("unreviewed budgets")
			}
			before, err := install.ReadRuntime(f.o.Manifest.Paths.Config)
			if f.o.Manifest.Paths.Config == "" {
				before, err = runtime, nil
			}
			if err != nil {
				return install.DeliveryProviderReview{}, err
			}
			var input install.RuntimeInput
			if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
				return install.DeliveryProviderReview{}, err
			}
			after, err := input.Resolve()
			if err != nil {
				return install.DeliveryProviderReview{}, err
			}
			paused := f.o.Manifest
			paused.Mode, paused.DeliveryActivation, paused.Commands = "verification", nil, nil
			if err = install.ValidatePausedCommandDeliveryLimits(paused, before, after); err != nil {
				return install.DeliveryProviderReview{}, err
			}
			paths := []string{runtime.Providers["telegram"].TokenFile, runtime.Providers["telegram"].ChatFile}
			slices.Sort(paths)
			return install.DeliveryProviderReview{Before: f.o.Manifest, Credentials: paths}, nil
		},
		inspectPause: func(_ context.Context, r install.DeliveryPauseRequest) (install.ManagedManifest, bool, error) {
			m, ok := f.paused[r.ManifestHash]
			return m, ok, nil
		},
		pause: func(_ context.Context, r install.DeliveryPauseRequest) (install.ManagedManifest, error) {
			if r.ManifestHash != protocol.Hash(f.o.Manifest) {
				t.Fatal("repeated pause")
			}
			f.o.Manifest.Mode, f.o.Manifest.Commands, f.o.Manifest.DeliveryActivation = "verification", nil, nil
			f.o.Manifest.UnitHash = protocol.Hash("paused unit")
			f.paused[r.ManifestHash] = f.o.Manifest
			f.pauses++
			return f.o.Manifest, f.fail("pause-reply")
		},
		inspectRotate: func(_ context.Context, _ install.ManagedSpec, hash string) (install.ManagedManifest, bool, error) {
			m, ok := f.rotated[hash]
			return m, ok, nil
		},
		rotate: func(_ context.Context, s install.ManagedSpec, hash string) (install.ManagedManifest, error) {
			if hash != protocol.Hash(f.o.Manifest) {
				t.Fatal("repeated migration")
			}
			raw, err := os.ReadFile(s.RuntimeFile)
			if err != nil {
				return f.o.Manifest, err
			}
			after, err := install.ReadRuntime(s.RuntimeFile)
			if err != nil {
				return f.o.Manifest, err
			}
			m := &f.o.Manifest
			m.Revision = testutil.ID(t, "n")
			m.ConfigHash, m.RuntimeHash, m.UnitHash = protocol.Hash(after), inputDigest(raw), protocol.Hash("budget unit")
			if m.DeliveryOwnerConfigHash == "" {
				m.DeliveryOwnerConfigHash = protocol.Hash(runtime.Worker)
			}
			m.Paths.Executable = filepath.Join(m.Root, "releases", m.Revision, "deploy")
			m.Paths.Config = filepath.Join(m.Root, "releases", m.Revision, "runtime.json")
			testutil.Must(t, os.MkdirAll(filepath.Dir(m.Paths.Config), 0700))
			testutil.Must(t, os.WriteFile(m.Paths.Config, raw, 0600))
			f.rotated[hash] = *m
			f.rotations++
			return *m, f.fail("migration-reply")
		},
		scope: func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, _ []string, change func(backup.ProducerReservation) (string, error)) (string, error) {
			if v := f.scopes[r.Operation]; v != "" {
				return v, nil
			}
			v, err := change(f.reservation)
			if err == nil {
				f.scopes[r.Operation] = v
				err = f.fail("scope-reply")
			}
			return v, err
		}, boundary: f.fail,
	}
	f.renewal = commandRequalificationLifecycle{commands: f.commands}
	f.renewal.pause, f.renewal.inspectPause = f.provider.pause, f.provider.inspectPause
	f.renewal.inspect = f.inspect
	f.renewal.inputs = f.provider.inputs
	f.renewal.delivery = delivery
	return f
}
func (f *commandBudgetFixture) toggle(t *testing.T, on bool) {
	t.Helper()
	n := *f.r.Desired.Notification
	f.r.Desired.Notification = &n
	n.Commands.Enabled = on
	var err error
	f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
	testutil.Must(t, err)
}
func (f *commandBudgetFixture) budget(t *testing.T, value int) {
	t.Helper()
	n := *f.r.Desired.Notification
	f.r.Desired.Notification = &n
	n.Limits.MaxMessages = value
	var err error
	f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
	testutil.Must(t, err)
}

func TestCommandBudgetsResumeAndRetainReceiver(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		for _, at := range []string{"delivery-provider-intent", "delivery-provider-paused", "delivery-provider-rotated", "pause-reply", "migration-reply", "scope-reply"} {
			t.Run(mode+"/"+at, func(t *testing.T) {
				ctx := context.Background()
				f := newCommandBudgetFixture(t, mode)
				originalReceiver := protocol.Hash(f.receiver)
				originalOwner := f.o.Manifest.DeliveryOwnerHash
				for cycle := range 2 {
					f.budget(t, 900-cycle*100)
					f.boundary, f.fired = at, false
					_, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
					if !handled || err == nil || !f.fired {
						t.Fatal("missed boundary", handled, err)
					}
					retained := f.journal(t)
					effects := f.pauses + f.rotations
					for _, damage := range []string{"helper", "backup", "limits", "receiver"} {
						r := f.r
						switch damage {
						case "helper":
							r.HelperSHA256 = protocol.Hash("changed helper")
						case "backup":
							b := *r.Desired.Backup
							b.Schedule = "weekly"
							r.Desired.Backup = &b
						case "limits":
							n := *r.Desired.Notification
							n.Limits.MaxMessages++
							r.Desired.Notification = &n
							r.DesiredHash, _ = r.Desired.NotificationFingerprint()
						case "receiver":
							f.receiverChanged = true
						}
						_, _, err = reconcileDeliveryProviders(ctx, r, f.paths, "/helper", f.provider)
						f.receiverChanged = false
						if err == nil || f.journal(t).Digest() != retained.Digest() || effects != f.pauses+f.rotations {
							t.Fatal("changed retry accepted", damage, err)
						}
					}
					f.boundary = ""
					_, handled, err = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
					testutil.Must(t, err)
					if !handled {
						t.Fatal("unhandled budgets")
					}
					testutil.Must(t, verifyDeliveryProviderReceipt(f.d, f.journal(t), f.o.Manifest, f.provider.scopeInspect))
					receiver, err := retainedCommandReceiver(ctx, f.d, f.journal(t), f.commands.notificationCommandLifecycle)
					testutil.Must(t, err)
					if protocol.Hash(receiver) != originalReceiver || f.o.Manifest.DeliveryOwnerHash != originalOwner || f.o.Manifest.Commands != nil {
						t.Fatal("changed ownership")
					}
					f.ready = false
					result, handled, err := reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
					testutil.Must(t, err)
					if !handled || result.State != "needs_qualification" {
						t.Fatal("invented qualification", result)
					}
					f.ready = true
					_, _, err = reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
					testutil.Must(t, err)
					testutil.Must(t, verifyCommandRenewalReceipt(ctx, f.d, f.journal(t), f.o.Manifest, f.renewal))
					f.toggle(t, true)
					_, _, err = reconcileCommandActivity(ctx, f.r, f.paths, f.commands)
					testutil.Must(t, err)
					f.toggle(t, false)
					_, _, err = reconcileCommandActivity(ctx, f.r, f.paths, f.commands)
					testutil.Must(t, err)
				}
				if f.pauses != 2 || f.rotations != 2 {
					t.Fatal("replayed domain effects", f.pauses, f.rotations)
				}
			})
		}
	}
}

func TestCommandBudgetReceiptsAreReadOnlyAndRequireReceiver(t *testing.T) {
	ctx := context.Background()
	f := newCommandBudgetFixture(t, "read")
	f.budget(t, 900)
	_, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
	testutil.Must(t, err)
	if !handled {
		t.Fatal("unhandled budgets")
	}
	j := f.journal(t)
	tree := planTree(t, f.paths.Control)
	for range 2 {
		testutil.Must(t, verifyDeliveryProviderReceipt(f.d, j, f.o.Manifest, f.provider.scopeInspect))
		_, err = retainedCommandReceiver(ctx, f.d, j, f.commands.notificationCommandLifecycle)
		testutil.Must(t, err)
	}
	if !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
		t.Fatal("receipt inspection changed evidence")
	}
	f.receiverChanged = true
	_, _, err = reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
	if err == nil || f.o.Manifest.Mode != "verification" || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
		t.Fatal("qualification accepted changed receiver", err)
	}
}

func TestCommandBudgetsResumeLostDeliveryAndConnectReplies(t *testing.T) {
	for _, boundary := range []string{"delivery-reply", "connect-reply"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			f := newCommandBudgetFixture(t, "mutation")
			f.budget(t, 900)
			_, _, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
			testutil.Must(t, err)
			f.ready = true
			f.boundary, f.fired = boundary, false
			_, handled, err := reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
			if !handled || err == nil || !f.fired {
				t.Fatal("missed lost renewal reply", handled, err)
			}
			after := f.renewals
			f.boundary = ""
			for range 2 {
				result, handled, err := reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
				testutil.Must(t, err)
				if !handled || result.State != "commands_disabled" || f.renewals != after {
					t.Fatal("renewal replayed or enabled commands", result)
				}
				testutil.Must(t, verifyCommandRenewalReceipt(ctx, f.d, f.journal(t), f.o.Manifest, f.renewal))
			}
		})
	}
}

func TestCommandBudgetsRefuseMissingRetainedIntent(t *testing.T) {
	ctx := context.Background()
	f := newCommandBudgetFixture(t, "read")
	f.budget(t, 900)
	f.boundary = "delivery-provider-paused"
	_, _, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
	if err == nil {
		t.Fatal("missed pause")
	}
	j := f.journal(t)
	name := filepath.Join(f.paths.Control, "delivery-providers-"+j.Children[0].Operation+".json")
	testutil.Must(t, os.Remove(name))
	tree := planTree(t, f.paths.Control)
	effects := f.pauses + f.rotations
	f.provider.review = func(context.Context, string, string, []byte) (install.DeliveryProviderReview, error) {
		return install.DeliveryProviderReview{}, errors.New("gateway already paused")
	}
	_, _, err = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
	if err == nil || f.pauses+f.rotations != effects || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
		t.Fatal("lost intent repeated effects", err)
	}
}

func TestCommandBudgetsPreserveEnrollmentAndDerivedProducerLimit(t *testing.T) {
	f := newCommandBudgetFixture(t, "mutation")
	prior := f.journal(t)
	origin, err := deliveryPolicyRuntimeOrigin(f.d, prior, "command-limits")
	testutil.Must(t, err)
	raw, err := retainedEnrolledRuntime(f.d, f.paths, origin)
	testutil.Must(t, err)
	var input install.RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
	before := protocol.Hash(input)
	f.budget(t, 500)
	compiled, _, _, err := compileCommandDeliveryLimits(f.r, f.paths, f.d, prior, input, raw, f.reservation)
	testutil.Must(t, err)
	if protocol.Hash(input) != before || protocol.Hash(input.Worker.Interaction) != protocol.Hash(compiled.Input.Worker.Interaction) || protocol.Hash(input.Telegram) != protocol.Hash(compiled.Input.Telegram) {
		t.Fatal("budget compilation changed enrollment")
	}
	if compiled.Input.Worker.Producers[0].MaxMessages != 500 {
		t.Fatal("derived producer limit did not follow global budget")
	}
}

func TestCommandBudgetsAllowLaterIndependentRenewal(t *testing.T) {
	ctx := context.Background()
	f := newCommandBudgetFixture(t, "read")
	f.budget(t, 900)
	_, _, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
	testutil.Must(t, err)
	f.ready = true
	_, _, err = reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
	testutil.Must(t, err)
	f.r.Action = "qualify-begin"
	result, handled, err := reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
	testutil.Must(t, err)
	if !handled || result.State != "needs_qualification" {
		t.Fatal("independent renewal did not pause")
	}
	f.r.Action = "apply"
	result, handled, err = reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
	testutil.Must(t, err)
	if !handled || result.State != "commands_disabled" {
		t.Fatal("independent renewal changed intake")
	}
	testutil.Must(t, verifyCommandRenewalReceipt(ctx, f.d, f.journal(t), f.o.Manifest, f.renewal))
}
