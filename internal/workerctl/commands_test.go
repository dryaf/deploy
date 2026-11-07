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
	"github.com/dryaf/deploy/internal/safefs"
)

func TestCommandCoordinatorStagesReceiverBeforeAdmissionAndResumesLostReplies(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		t.Run(mode, func(t *testing.T) { testCommandCoordinatorMode(t, mode) })
	}
}

func testCommandCoordinatorMode(t *testing.T, declaredMode string) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths := bootstrapControlFixture(t)
	r.Desired.Notification.Commands.Enabled = true
	r.Desired.Notification.Commands.Mode = declaredMode
	mode, version := "read", 1
	if declaredMode == "mutation" {
		mode, version = "all", 2
	}
	r.DesiredHash, _ = r.Desired.Fingerprint()
	d, err := ensureConfigDirectory(paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id, _ := safefs.NewID("n")
	child, _ := safefs.NewID("n")
	w, _ := safefs.NewID("w")
	g, _ := safefs.NewID("g")
	before := install.ManagedManifest{Version: 1, Mode: "delivery", Root: paths.Installation, Identity: protocol.Identity{Worker: w, Recovery: g}, DeliveryActivation: &install.ManagedDeliveryActivation{Operation: "delivery", Hash: protocol.Hash("delivery")}}
	old := OperationJournal{Version: 1, ID: id, Complete: true, Pins: OperationPins{Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}}, Children: []ChildReference{{Name: "notification-delivery", Operation: child, Complete: true, Receipt: protocol.Hash(before)}}}
	lock, err := d.Lock("operation.lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if err = d.WriteJSON("operation.json", old, false); err != nil {
		t.Fatal(err)
	}
	after := before
	after.Commands = &install.ManagedCommandAdmission{Mode: mode}
	request := install.CommandPolicyRequest{Root: paths.Installation, ManifestHash: protocol.Hash(before), Operation: "enable-commands", Mode: mode, QualificationHash: protocol.Hash("qualification")}
	ready, staged, admitted, connected := false, false, false, false
	reviews, stages, admissions := 0, 0, 0
	var receiver backup.CommandReceiverRequest
	l := notificationCommandLifecycle{
		review: func(_ context.Context, root, hash, mode string) (install.CommandPolicyRequest, bool, error) {
			reviews++
			if root != request.Root || hash != request.ManifestHash || mode != request.Mode {
				t.Fatal("changed review pins")
			}
			return request, ready, nil
		},
		receiverReview: func(_ context.Context, home, desired, manifest, operation string) (backup.CommandReceiverRequest, error) {
			if home != r.Facts.Home || desired != r.DesiredHash || manifest != request.ManifestHash {
				t.Fatal("changed receiver review pins")
			}
			return backup.CommandReceiverRequest{Version: version, Operation: operation, Identity: before.Identity, Producer: protocol.Producer{ID: "backup", Epoch: "one"}}, nil
		},
		receiverInspect: func(_ context.Context, got backup.CommandReceiverRequest) (string, bool, error) {
			if !staged {
				return "", false, nil
			}
			if protocol.Hash(got) != protocol.Hash(receiver) {
				return "", false, errors.New("changed receiver")
			}
			return protocol.Hash(receiver), true, nil
		},
		receiverStage: func(_ context.Context, got backup.CommandReceiverRequest) (string, error) {
			stages++
			if admitted {
				t.Fatal("admitted commands before receiver")
			}
			receiver, staged = got, true
			return "", errors.New("lost receiver reply")
		},
		inspect: func(_ context.Context, got install.CommandPolicyRequest) (install.ManagedManifest, bool, error) {
			if !staged || got != request {
				t.Fatal("admission inspected without exact staged receiver")
			}
			return after, admitted, nil
		},
		ensure: func(_ context.Context, got install.CommandPolicyRequest) (install.ManagedManifest, error) {
			admissions++
			if !staged || got != request {
				t.Fatal("admitted before receiver")
			}
			admitted = true
			return after, errors.New("lost admission reply")
		},
		connect: func(_ context.Context, home, desired, manifest string) error {
			if !admitted || home != r.Facts.Home || desired != r.DesiredHash || manifest != protocol.Hash(after) {
				t.Fatal("changed reporting continuation")
			}
			if !connected {
				connected = true
				return errors.New("lost continuation reply")
			}
			return nil
		},
	}
	ctx := context.Background()
	result, err := advanceNotificationCommands(ctx, r, paths, d, old, ControlResult{Version: 1}, l)
	if err != nil || result.Code != "command_qualification_required" || stages != 0 {
		t.Fatal("unqualified command admission", result, err)
	}
	var current OperationJournal
	if err = d.ReadJSON("operation.json", &current); err != nil || current.Digest() != old.Digest() {
		t.Fatal("unqualified review changed journal", err)
	}
	ready = true
	for range 3 {
		if _, err = advanceNotificationCommands(ctx, r, paths, d, current, result, l); err == nil {
			t.Fatal("lost reply hidden")
		}
		current = OperationJournal{}
		if err = d.ReadJSON("operation.json", &current); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		result, err = advanceNotificationCommands(ctx, r, paths, d, current, result, l)
		if err != nil || result.State != "commands_admitted" || !current.Complete || result.Identity == nil || *result.Identity != before.Identity {
			t.Fatal("failed exact retry", result, err)
		}
	}
	if reviews != 2 || stages != 1 || admissions != 1 {
		t.Fatal("repeated effects or qualification selection", reviews, stages, admissions)
	}
	changed := r
	changed.HelperSHA256 = protocol.Hash("new helper")
	if _, err = advanceNotificationCommands(ctx, changed, paths, d, current, result, l); err == nil {
		t.Fatal("retargeted artifact")
	}
	if err = os.Remove(filepath.Join(paths.Control, "command-receiver-"+current.Children[0].Operation+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err = advanceNotificationCommands(ctx, r, paths, d, current, result, l); err == nil {
		t.Fatal("recreated lost receiver authority")
	}
	if stages != 1 || admissions != 1 {
		t.Fatal("replayed effects after lost authority")
	}
}
