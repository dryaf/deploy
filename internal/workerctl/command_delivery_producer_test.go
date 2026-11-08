package workerctl

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func (f *commandBudgetFixture) producerPolicy(t *testing.T, change func(*config.ProducerDeclaration)) {
	t.Helper()
	n := *f.r.Desired.Notification
	f.r.Desired.Notification = &n
	n.Producers = maps.Clone(n.Producers)
	if n.Producers == nil {
		n.Producers = map[string]config.ProducerDeclaration{}
	}
	p, ok := n.Producers["backup-worker"]
	if !ok {
		origin, err := deliveryPolicyRuntimeOrigin(f.d, f.journal(t), "command-producer")
		testutil.Must(t, err)
		raw, err := retainedEnrolledRuntime(f.d, f.paths, origin)
		testutil.Must(t, err)
		var input install.RuntimeInput
		testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
		e := input.Worker.Producers[0]
		required := e.Required
		p = config.ProducerDeclaration{Required: &required, Routes: []string{f.r.Desired.Backup.Notifications.Route}, MaxMessages: e.MaxMessages, MaxSilenceSeconds: e.MaxSilenceSeconds, MaxEvidenceSeconds: e.MaxEvidenceSeconds, PolicyHealth: e.PolicyHealth, InteractiveContext: e.InteractiveContext, ContextKinds: e.ContextKinds}
	}
	change(&p)
	n.Producers["backup-worker"] = p
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
}

func TestCommandProducerEquivalentDeclarationsResumeWithoutDeliveryEffects(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		for _, at := range []string{"delivery-provider-intent", "scope-reply"} {
			t.Run(mode+"/"+at, func(t *testing.T) {
				ctx := context.Background()
				f := newCommandPolicyFixture(t, mode, "command-declaration")
				manifest, receiver, renewals := protocol.Hash(f.o.Manifest), protocol.Hash(f.receiver), f.renewals
				for direction := range 2 {
					if direction == 0 {
						f.producerPolicy(t, func(*config.ProducerDeclaration) {})
					} else {
						n := *f.r.Desired.Notification
						f.r.Desired.Notification = &n
						n.Producers = nil
						f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
					}
					f.boundary, f.fired = at, false
					_, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
					if !handled || err == nil || !f.fired {
						t.Fatal("missed declaration boundary", handled, err)
					}
					retained := f.journal(t)
					files := planTree(t, f.paths.Control)
					changed := f.r
					b := *changed.Desired.Backup
					changed.Desired.Backup = &b
					b.Schedule = "weekly"
					if _, _, err = reconcileDeliveryProviders(ctx, changed, f.paths, "/helper", f.provider); err == nil {
						t.Fatal("declaration accepted changed full policy")
					}
					f.receiverChanged = true
					if _, _, err = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider); err == nil {
						t.Fatal("declaration accepted changed receiver")
					}
					f.receiverChanged = false
					if retained.Digest() != f.journal(t).Digest() || !reflect.DeepEqual(files, planTree(t, f.paths.Control)) {
						t.Fatal("rejected retry changed declaration history")
					}
					f.boundary = ""
					for range 2 {
						result, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
						testutil.Must(t, err)
						if !handled || result.State != "commands_disabled" {
							t.Fatal("equivalent declaration requires renewal", result)
						}
						testutil.Must(t, verifyDeliveryProviderReceipt(f.d, f.journal(t), f.o.Manifest, f.provider.scopeInspect))
						got, err := retainedCommandReceiver(ctx, f.d, f.journal(t), f.commands.notificationCommandLifecycle)
						testutil.Must(t, err)
						if protocol.Hash(got) != receiver || protocol.Hash(f.o.Manifest) != manifest || f.pauses != 0 || f.rotations != 0 || f.renewals != renewals {
							t.Fatal("declaration changed runtime, receiver or delivery")
						}
					}
				}
				f.producerPolicy(t, func(p *config.ProducerDeclaration) { p.MaxMessages-- })
				f.provider.kind = "command-producer"
				_, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
				testutil.Must(t, err)
				if !handled || f.pauses != 1 || f.rotations != 1 {
					t.Fatal("effective policy did not migrate")
				}
				f.ready = true
				_, _, err = reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
				testutil.Must(t, err)
				f.toggle(t, true)
				_, _, err = reconcileCommandActivity(ctx, f.r, f.paths, f.commands)
				testutil.Must(t, err)
				if protocol.Hash(f.receiver) != receiver {
					t.Fatal("producer migration replaced receiver")
				}
			})
		}
	}
}

func TestCommandProducerDeclarationSupportsActivityAndIndependentRenewal(t *testing.T) {
	for _, next := range []string{"activity", "qualification"} {
		t.Run(next, func(t *testing.T) {
			ctx := context.Background()
			f := newCommandPolicyFixture(t, "mutation", "command-declaration")
			f.producerPolicy(t, func(*config.ProducerDeclaration) {})
			_, _, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
			testutil.Must(t, err)
			if next == "activity" {
				f.toggle(t, true)
				for _, kind := range []string{"providers", "routes", "producer", "command-activity", "commands", "report-route", "derived-freshness", "freshness", ""} {
					_, handled, err := reconcileSharedPolicy(ctx, f.r, f.paths, "/helper", defaultSharedLimits(), kind)
					testutil.Must(t, err)
					if handled {
						t.Fatalf("staged %s handler intercepted admitted command activity", kind)
					}
				}
				_, _, err = reconcileCommandActivity(ctx, f.r, f.paths, f.commands)
				testutil.Must(t, err)
			} else {
				f.r.Action = "qualify-begin"
				result, handled, err := reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
				testutil.Must(t, err)
				if !handled || result.State != "needs_qualification" {
					t.Fatal(result)
				}
				f.r.Action = "apply"
				f.ready = true
				_, _, err = reconcileCommandRequalification(ctx, f.r, f.paths, f.renewal)
				testutil.Must(t, err)
				testutil.Must(t, verifyCommandRenewalReceipt(ctx, f.d, f.journal(t), f.o.Manifest, f.renewal))
			}
		})
	}
}

func TestCommandProducerCompilerRetainsReceiverWhileChangingAccess(t *testing.T) {
	f := newCommandPolicyFixture(t, "mutation", "command-producer")
	prior := f.journal(t)
	origin, err := deliveryPolicyRuntimeOrigin(f.d, prior, "command-producer")
	testutil.Must(t, err)
	raw, err := retainedEnrolledRuntime(f.d, f.paths, origin)
	testutil.Must(t, err)
	var input install.RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
	original := protocol.Hash(input)
	f.producerPolicy(t, func(p *config.ProducerDeclaration) {
		p.InteractiveContext = true
		p.ContextKinds = []string{"incident"}
		p.PolicyHealth = !p.PolicyHealth
	})
	compiled, _, _, err := compileCommandDeliveryProducer(f.r, f.paths, f.d, prior, input, raw, f.reservation)
	testutil.Must(t, err)
	if protocol.Hash(input) != original || protocol.Hash(compiled.Input.Worker.Interaction) != protocol.Hash(input.Worker.Interaction) || protocol.Hash(compiled.Input.Telegram) != protocol.Hash(input.Telegram) {
		t.Fatal("compiler changed retained receiver/provider")
	}
	if !compiled.Input.Worker.Producers[0].InteractiveContext || compiled.Input.Worker.Producers[0].Producer != input.Worker.Producers[0].Producer {
		t.Fatal("producer policy lost identity or context capability")
	}
}

func TestCommandProducerDeclarationRefusesLostIntentAfterRetainedRuntime(t *testing.T) {
	ctx := context.Background()
	f := newCommandPolicyFixture(t, "read", "command-declaration")
	f.producerPolicy(t, func(*config.ProducerDeclaration) {})
	f.boundary = "delivery-provider-intent"
	_, _, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
	if err == nil {
		t.Fatal("missed interruption")
	}
	j := f.journal(t)
	testutil.Must(t, os.Remove(filepath.Join(f.paths.Control, "delivery-providers-"+j.Children[0].Operation+".json")))
	tree := planTree(t, f.paths.Control)
	if _, _, err = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider); err == nil {
		t.Fatal("lost declaration intent reconstructed after retained evidence")
	}
	if !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
		t.Fatal("rejected declaration wrote replacement history")
	}
}

func TestCommandProducerCompletedDeclarationDoesNotReplayForLaterChanges(t *testing.T) {
	for _, change := range []string{"backup-only", "command-enable"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			f := newCommandPolicyFixture(t, "read", "command-declaration")
			f.producerPolicy(t, func(*config.ProducerDeclaration) {})
			_, _, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
			testutil.Must(t, err)
			completed, manifest := f.journal(t).Digest(), protocol.Hash(f.o.Manifest)
			if change == "backup-only" {
				b := *f.r.Desired.Backup
				f.r.Desired.Backup = &b
				b.Schedule = "weekly"
			} else {
				n := *f.r.Desired.Notification
				f.r.Desired.Notification = &n
				n.Commands.Enabled = true
				f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
			}
			_, handled, err := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", f.provider)
			testutil.Must(t, err)
			if handled || completed != f.journal(t).Digest() || manifest != protocol.Hash(f.o.Manifest) {
				t.Fatal("completed declaration replayed or captured later edit")
			}
		})
	}
}
