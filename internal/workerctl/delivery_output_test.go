package workerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

type deliveryOutputFixture struct {
	*commandActivityFixture
	output        deliveryOutputLifecycle
	outputEffects int
}

func newDeliveryOutputFixture(t *testing.T, commands bool) *deliveryOutputFixture {
	t.Helper()
	f := newCommandActivityFixture(t, "mutation", func(r *ControlRequest) {
		r.Desired.Notification.StatusLEDs.Enabled = true
		if !commands {
			r.Desired.Notification.Commands.Enabled = false
		}
	})
	if !commands {
		// Begin at the already admitted delivery journal, without a command receiver.
		j := f.journal(t)
		var prior OperationJournal
		testutil.Must(t, f.d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior))
		f.manifest.Commands = nil
		testutil.Must(t, f.d.WriteJSON("operation.json", prior, true))
	}
	reservation := backup.ProducerReservation{Version: 1, Producer: protocol.Producer{ID: testutil.ID(t, "w"), Epoch: testutil.ID(t, "n")}, Generation: testutil.ID(t, "g")}
	testutil.Must(t, f.d.WriteJSON("consumer-enrollment.json", consumerEnrollmentIntent{Version: 1, Reservation: reservation}, false))
	v := &deliveryOutputFixture{commandActivityFixture: f}
	v.output = deliveryOutputLifecycle{
		commands: f.lifecycle, inputs: f.lifecycle.inputs, connect: f.lifecycle.connect, scopeInspect: f.lifecycle.scopeInspect,
		review: func(_ context.Context, root, hash string, disabled bool) (install.ManagedManifest, error) {
			if root != f.paths.Installation || hash != protocol.Hash(f.manifest) {
				t.Fatal("review changed installation")
			}
			if !disabled && !f.ready {
				return f.manifest, errors.New("qualification expired")
			}
			return f.manifest, nil
		},
		output: notificationLEDLifecycle{
			inspect: func(context.Context, string) (install.InstallationObservation, error) {
				return install.InstallationObservation{Manifest: f.manifest, Phase: "complete"}, nil
			},
			set: func(_ context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
				if root != f.paths.Installation || expected != protocol.Hash(f.manifest) {
					t.Fatal("replayed output effect")
				}
				if err := f.fail("intent"); err != nil {
					return f.manifest, err
				}
				v.outputEffects++
				f.manifest.LEDOutput = &install.ManagedLEDOutput{Operation: testutil.ID(t, "n"), Disabled: disabled}
				return f.manifest, f.fail("gateway")
			},
		},
		scope: func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, change func(backup.ProducerReservation) (string, error)) (string, error) {
			if receipt := f.receipts[r.Operation]; receipt != "" {
				return receipt, nil
			}
			receipt, err := change(reservation)
			if err != nil {
				return "", err
			}
			if err = f.fail("scope"); err != nil {
				return "", err
			}
			f.receipts[r.Operation] = receipt
			return receipt, f.fail("scope-reply")
		},
	}
	return v
}

func TestDeliveryLEDOutputCoordinatorResumesAndInterleavesCommands(t *testing.T) {
	for _, commands := range []bool{false, true} {
		name := "delivery"
		if commands {
			name = "commands"
		}
		for _, boundary := range []string{"intent", "gateway", "scope", "scope-reply", "continuation"} {
			t.Run(name+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				f := newDeliveryOutputFixture(t, commands)
				original := f.manifest
				for i, enabled := range []bool{false, true, false} {
					n := *f.r.Desired.Notification
					n.StatusLEDs.Enabled = enabled
					f.r.Desired.Notification = &n
					f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
					f.boundary, f.fired = boundary, false
					_, handled, err := reconcileDeliveryLEDOutput(ctx, f.r, f.paths, f.output)
					if !handled || err == nil || !f.fired {
						t.Fatal("missed boundary", handled, err)
					}
					pending := f.journal(t)
					changed := f.r
					changed.HelperSHA256 = protocol.Hash("other helper")
					if _, _, err = reconcileDeliveryLEDOutput(ctx, changed, f.paths, f.output); err == nil || pending.Digest() != f.journal(t).Digest() {
						t.Fatal("changed helper rewrote pending operation", err)
					}
					for range 2 {
						result, handled, err := reconcileDeliveryLEDOutput(ctx, f.r, f.paths, f.output)
						if err != nil || !handled || result.LEDOutput == nil || result.LEDOutput.Disabled == enabled {
							t.Fatal(result, handled, err)
						}
					}
					if f.outputEffects != i+1 {
						t.Fatal("repeated output mutation", f.outputEffects)
					}
					testutil.Must(t, verifyDeliveryOutputReceipt(f.d, f.journal(t), f.manifest, f.output.scopeInspect))
					if commands {
						f.boundary = ""
						f.toggle(i%2 != 0)
						_, handled, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
						if !handled || err != nil {
							t.Fatal("command change after LED transition", handled, err)
						}
						testutil.Must(t, verifyCommandActivityReceipt(ctx, f.d, f.journal(t), f.manifest, f.lifecycle))
					}
					want := original
					want.Commands, want.LEDOutput = f.manifest.Commands, f.manifest.LEDOutput
					if protocol.Hash(want) != protocol.Hash(f.manifest) {
						t.Fatal("delivery identity or authority changed")
					}
				}
				if commands {
					testutil.Must(t, os.Remove(filepath.Join(f.paths.Control, "command-receiver-"+f.receiver.Operation+".json")))
					f.r.Desired.Notification.StatusLEDs.Enabled = true
					f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
					if _, _, err := reconcileDeliveryLEDOutput(ctx, f.r, f.paths, f.output); err == nil {
						t.Fatal("lost original receiver was ignored")
					}
				}
			})
		}
	}
}

func TestDeliveryLEDOutputCoordinatorRejectsMixedPolicyAndUnqualifiedEnable(t *testing.T) {
	f := newDeliveryOutputFixture(t, true)
	ctx := context.Background()
	f.r.Desired.Notification.StatusLEDs.Enabled = false
	f.r.Desired.Notification.Commands.Enabled = false
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	before := f.journal(t).Digest()
	if _, _, err := reconcileDeliveryLEDOutput(ctx, f.r, f.paths, f.output); err == nil || f.journal(t).Digest() != before || f.outputEffects != 0 {
		t.Fatal("mixed policy changed installation", err)
	}
	f.r.Desired.Notification.Commands.Enabled = true
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	_, _, err := reconcileDeliveryLEDOutput(ctx, f.r, f.paths, f.output)
	testutil.Must(t, err)
	f.ready = false
	f.r.Desired.Notification.StatusLEDs.Enabled = true
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	before = f.journal(t).Digest()
	if _, _, err = reconcileDeliveryLEDOutput(ctx, f.r, f.paths, f.output); err == nil || f.journal(t).Digest() != before || f.outputEffects != 1 {
		t.Fatal("unqualified enable retained operation", err)
	}
}
