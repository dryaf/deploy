package workerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

type commandActivityFixture struct {
	r                          ControlRequest
	paths                      ResolvedPaths
	d                          *safefs.Directory
	receiver                   backup.CommandReceiverRequest
	manifest                   install.ManagedManifest
	lifecycle                  commandActivityLifecycle
	requests                   map[string]install.ManagedManifest
	receipts                   map[string]string
	ready                      bool
	boundary                   string
	fired                      bool
	effects, reviews, connects int
}

func newCommandActivityFixture(t *testing.T, mode string, configure ...func(*ControlRequest)) *commandActivityFixture {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths := bootstrapControlFixture(t)
	declareFixtureReadCommands(t, &r)
	r.Desired.Notification.Commands.Mode = mode
	if mode == "mutation" {
		r.Desired.Notification.Commands.Permissions[0].Permissions = []string{"read", "incident.manage"}
	}
	for _, apply := range configure {
		apply(&r)
	}
	r.DesiredScope = 1
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	d, err := ensureConfigDirectory(paths.Control)
	testutil.Must(t, err)
	t.Cleanup(func() { d.Close() })
	lock, err := d.Lock("operation.lock", true, true)
	testutil.Must(t, err)
	lock.Close()
	testutil.Must(t, retainDeclaration(d, r.Desired, r.DesiredHash, 1))
	before := install.ManagedManifest{Version: 1, Mode: "delivery", Root: paths.Installation, BinaryHash: r.HelperSHA256, Identity: protocol.Identity{Worker: testutil.ID(t, "w"), Recovery: testutil.ID(t, "g")}, DeliveryActivation: &install.ManagedDeliveryActivation{Operation: "delivery", Hash: protocol.Hash("delivery")}}
	prior := OperationJournal{Version: 1, ID: testutil.ID(t, "n"), Complete: true, Pins: OperationPins{Worker: r.Worker, Action: "apply", DesiredScope: 1, DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}}, Children: []ChildReference{{Name: "notification-delivery", Operation: testutil.ID(t, "n"), Complete: true, Receipt: protocol.Hash(before)}}}
	f := &commandActivityFixture{r: r, paths: paths, d: d, manifest: before, requests: map[string]install.ManagedManifest{}, receipts: map[string]string{}, ready: true}
	version, runtimeMode := 1, "read"
	if mode == "mutation" {
		version, runtimeMode = 2, "all"
	}
	f.receiver = backup.CommandReceiverRequest{Version: version, Operation: testutil.ID(t, "n"), Identity: before.Identity, Producer: protocol.Producer{ID: "backup", Epoch: "one"}}
	descriptors := backup.BackupCommandDescriptors()
	if mode == "mutation" {
		descriptors = backup.BackupIncidentCommandDescriptors()
	}
	bindings := NotificationBindings{Producers: map[string]ProducerBinding{"backup-worker": {Producer: f.receiver.Producer, Generation: "one", Commands: descriptors}}, Conversations: map[string]interaction.Destination{}}
	for name, c := range r.Desired.Notification.Commands.Conversations {
		bindings.Conversations[name] = interaction.Destination{ID: name, Generation: "one", Provider: c.Provider, Account: c.Account, NativeID: c.NativeID, Thread: c.Thread}
	}
	f.receiver.Policy, err = compileCommandPolicy(r.Desired.Notification.Commands, bindings)
	testutil.Must(t, err)
	f.manifest.Commands = &install.ManagedCommandAdmission{Mode: runtimeMode}
	full, _ := r.Desired.Fingerprint()
	pins := prior.Pins
	pins.PreviousOperation, pins.Previous = prior.ID, prior.Digest()
	pins.Artifacts = map[string]string{"helper": r.HelperSHA256, "command-qualification": protocol.Hash("original qualification"), "command-backup-policy": full}
	initial := OperationJournal{Version: 1, ID: testutil.ID(t, "n"), Pins: pins, Complete: true, Children: []ChildReference{{Name: "backup-command-receiver", Operation: f.receiver.Operation, Complete: true, Receipt: protocol.Hash(f.receiver)}, {Name: "notification-commands", Operation: testutil.ID(t, "n"), Complete: true, Receipt: protocol.Hash(f.manifest)}}}
	testutil.Must(t, d.Mkdir("history"))
	testutil.Must(t, d.WriteJSON("history/"+prior.ID+".json", prior, false))
	testutil.Must(t, d.WriteJSON("operation.json", initial, false))
	testutil.Must(t, d.WriteJSON("command-receiver-"+f.receiver.Operation+".json", f.receiver, false))
	f.lifecycle = commandActivityLifecycle{
		notificationCommandLifecycle: notificationCommandLifecycle{
			receiverInspect: func(_ context.Context, r backup.CommandReceiverRequest) (string, bool, error) {
				if protocol.Hash(r) != protocol.Hash(f.receiver) {
					return "", false, errors.New("receiver changed")
				}
				return protocol.Hash(r), true, nil
			},
			review: func(_ context.Context, root, hash, mode string) (install.CommandPolicyRequest, bool, error) {
				f.reviews++
				if hash != protocol.Hash(f.manifest) || root != f.paths.Installation || mode != runtimeMode {
					t.Fatal("review lost original mode or gateway")
				}
				return install.CommandPolicyRequest{Root: root, ManifestHash: hash, Mode: mode, Operation: "enable-commands", QualificationHash: protocol.Hash("current qualification")}, f.ready, nil
			},
			inspect: func(_ context.Context, r install.CommandPolicyRequest) (install.ManagedManifest, bool, error) {
				if m, ok := f.requests[protocol.Hash(r)]; ok {
					return m, true, nil
				}
				if r.ManifestHash != protocol.Hash(f.manifest) {
					return f.manifest, false, errors.New("gateway request changed")
				}
				return f.manifest, false, nil
			},
			ensure: func(_ context.Context, r install.CommandPolicyRequest) (install.ManagedManifest, error) {
				if r.ManifestHash != protocol.Hash(f.manifest) {
					t.Fatal("repeated gateway mutation")
				}
				if err := f.fail("intent"); err != nil {
					return f.manifest, err
				}
				f.effects++
				mode := "drain"
				if r.Operation == "enable-commands" {
					mode = r.Mode
				}
				f.manifest.Commands = &install.ManagedCommandAdmission{Mode: mode, Hash: protocol.Hash(r)}
				f.requests[protocol.Hash(r)] = f.manifest
				return f.manifest, f.fail("gateway")
			},
			connect: func(_ context.Context, home, desired, hash string) error {
				full, _ := f.r.Desired.Fingerprint()
				if home != f.r.Facts.Home || desired != full || hash != protocol.Hash(f.manifest) {
					t.Fatal("changed reporting continuation")
				}
				f.connects++
				return f.fail("continuation")
			},
		},
		inputs:       func(ControlRequest, ResolvedPaths, *safefs.Directory, config.DesiredWorkers) error { return nil },
		scopeInspect: func(_, op, _, _ string) (string, bool, error) { v := f.receipts[op]; return v, v != "", nil },
		scope: func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, change func(backup.ProducerReservation) (string, error)) (string, error) {
			if v := f.receipts[r.Operation]; v != "" {
				return v, nil
			}
			v, err := change(backup.ProducerReservation{Producer: f.receiver.Producer})
			if err != nil {
				return "", err
			}
			if err = f.fail("scope"); err != nil {
				return "", err
			}
			f.receipts[r.Operation] = v
			return v, f.fail("scope-reply")
		},
	}
	return f
}

func (f *commandActivityFixture) fail(boundary string) error {
	if f.boundary == boundary && !f.fired {
		f.fired = true
		return errors.New("lost " + boundary + " reply")
	}
	return nil
}
func (f *commandActivityFixture) toggle(enabled bool) {
	n := *f.r.Desired.Notification
	n.Commands.Enabled = enabled
	f.r.Desired.Notification = &n
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
}
func (f *commandActivityFixture) journal(t *testing.T) OperationJournal {
	t.Helper()
	var j OperationJournal
	testutil.Must(t, f.d.ReadJSON("operation.json", &j))
	return j
}

func TestCommandActivityResumesBothModesWithoutReplacingReceiver(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		for _, boundary := range []string{"intent", "gateway", "scope", "scope-reply", "continuation"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				f := newCommandActivityFixture(t, mode)
				original := f.manifest
				for i, enabled := range []bool{false, true, false} {
					f.toggle(enabled)
					f.boundary, f.fired = boundary, false
					_, handled, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
					if !handled || err == nil || !f.fired {
						t.Fatal("missed interruption", handled, err)
					}
					pending := f.journal(t)
					changed := f.r
					changed.HelperSHA256 = protocol.Hash("changed helper")
					if _, _, err = reconcileCommandActivity(context.Background(), changed, f.paths, f.lifecycle); err == nil {
						t.Fatal("retargeted pending operation")
					}
					if pending.Digest() != f.journal(t).Digest() {
						t.Fatal("changed helper rewrote journal")
					}
					for range 2 {
						result, handled, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
						want := "commands_disabled"
						if enabled {
							want = "commands_admitted"
						}
						if err != nil || !handled || result.State != want {
							t.Fatal(result, handled, err)
						}
					}
					if f.effects != i+1 {
						t.Fatal("replayed gateway effect", f.effects)
					}
					j := f.journal(t)
					testutil.Must(t, verifyCommandActivityReceipt(context.Background(), f.d, j, f.manifest, f.lifecycle))
					want := original
					want.Commands = f.manifest.Commands
					if protocol.Hash(want) != protocol.Hash(f.manifest) {
						t.Fatal("toggle changed delivery authority")
					}
				}
				if f.reviews != 1 {
					t.Fatal("qualification reselected on retry", f.reviews)
				}
				testutil.Must(t, os.Remove(filepath.Join(f.paths.Control, "command-receiver-"+f.receiver.Operation+".json")))
				if _, _, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle); err == nil {
					t.Fatal("lost receiver authority recreated")
				}
			})
		}
	}
}

func TestCommandActivityRefusesUnqualifiedEnableAndMixedPolicy(t *testing.T) {
	f := newCommandActivityFixture(t, "read")
	f.toggle(false)
	_, _, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
	testutil.Must(t, err)
	f.toggle(true)
	f.ready = false
	prior := f.journal(t)
	r, handled, err := reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle)
	if err != nil || !handled || r.Code != "command_qualification_required" || f.effects != 1 || prior.Digest() != f.journal(t).Digest() {
		t.Fatal("unqualified enable changed authority", r, err)
	}
	f.r.Desired.Notification.Commands.Mode = "mutation"
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	if _, _, err = reconcileCommandActivity(context.Background(), f.r, f.paths, f.lifecycle); err == nil || f.effects != 1 {
		t.Fatal("toggle expanded mode", err)
	}
}

func TestDisabledCommandPolicyKeepsBotSettingsWithoutAdmittingIntake(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	declareFixtureReadCommands(t, &r)
	r.Desired.Notification.Commands.Enabled = false
	testutil.Must(t, directNotificationSupport(r.Desired))
	input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	testutil.Must(t, err)
	if !input.Bootstrap || input.Telegram != nil || input.Worker.Interaction != nil {
		t.Fatal("disabled policy admitted command intake")
	}
	r.Desired.Notification.Commands.Workers = nil
	if directNotificationSupport(r.Desired) == nil {
		t.Fatal("incomplete interactive policy was silently dropped")
	}
}
