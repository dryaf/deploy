package workerctl

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestDeclarativeDeliveryFreezesAdmissionAndResumesReportConnection(t *testing.T) {
	testDeclarativeDelivery(t, false)
}
func TestScopedDeliveryPreservesAdmissionAcrossBackupScheduleChange(t *testing.T) {
	testDeclarativeDelivery(t, true)
}
func testDeclarativeDelivery(t *testing.T, scoped bool) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths := bootstrapControlFixture(t)
	appliedBackupHash := r.DesiredHash
	if scoped {
		r.DesiredScope = 1
		r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	}
	currentBackupHash := func() string {
		if !scoped {
			return r.DesiredHash
		}
		hash, e := r.Desired.Fingerprint()
		if e != nil {
			t.Fatal(e)
		}
		return hash
	}
	d, err := ensureConfigDirectory(paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id, _ := safefs.NewID("n")
	child, _ := safefs.NewID("n")
	w, _ := safefs.NewID("w")
	g, _ := safefs.NewID("g")
	before := install.ManagedManifest{Version: 1, Mode: "verification", Root: paths.Installation, Identity: protocol.Identity{Worker: w, Recovery: g}}
	old := OperationJournal{Version: 1, ID: id, Complete: true, Pins: OperationPins{DesiredScope: r.DesiredScope, Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}}, Children: []ChildReference{{Name: "notification-enrollment", Operation: child, Complete: true, Receipt: protocol.Hash(before)}}}
	lock, err := d.Lock("operation.lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if err = d.WriteJSON("operation.json", old, false); err != nil {
		t.Fatal(err)
	}
	ready, admitted, connected := false, false, false
	reviews, activations, connections := 0, 0, 0
	request := install.DeliveryAdmissionRequest{Root: paths.Installation, ManifestHash: protocol.Hash(before), QualificationHash: protocol.Hash("qualification-one")}
	after := before
	after.Mode = "delivery"
	after.DeliveryOwnerHash = protocol.Hash("delivery-owner")
	after.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: "activation", Hash: protocol.Hash("proof")}
	lifecycle := notificationDeliveryLifecycle{
		review: func(_ context.Context, root, hash string) (install.DeliveryAdmissionRequest, bool, error) {
			reviews++
			if root != request.Root || hash != request.ManifestHash {
				t.Fatal("review changed pins")
			}
			return request, ready, nil
		},
		inspect: func(_ context.Context, got install.DeliveryAdmissionRequest) (install.ManagedManifest, bool, error) {
			if got != request {
				t.Fatal("retry changed admission")
			}
			return after, admitted, nil
		},
		ensure: func(_ context.Context, got install.DeliveryAdmissionRequest) (install.ManagedManifest, error) {
			activations++
			if got != request {
				t.Fatal("activation changed admission")
			}
			admitted = true
			return after, errors.New("lost activation reply")
		},
		backupStatus: func(context.Context, string) (backup.SetupObservation, error) {
			return backup.SetupObservation{Stage: "complete", DesiredHash: appliedBackupHash}, nil
		},
		connect: func(_ context.Context, home, desired, hash string) error {
			connections++
			if home != r.Facts.Home || desired != currentBackupHash() || hash != protocol.Hash(after) {
				t.Fatal("report connection changed identity")
			}
			if !connected {
				connected = true
				return errors.New("lost report completion reply")
			}
			return nil
		},
	}
	ctx := context.Background()
	result, err := advanceNotificationDelivery(ctx, r, paths, d, old, ControlResult{Version: 1, State: "verification", Code: "ok"}, lifecycle)
	if err != nil || result.State != "verification" || activations != 0 {
		t.Fatal("unqualified apply activated", result, err)
	}
	var current OperationJournal
	if err = d.ReadJSON("operation.json", &current); err != nil || current.Digest() != old.Digest() {
		t.Fatal("unqualified apply changed outer journal", err)
	}
	ready = true
	if _, err = advanceNotificationDelivery(ctx, r, paths, d, old, result, lifecycle); err == nil {
		t.Fatal("lost activation reply hidden")
	}
	current = OperationJournal{}
	if err = d.ReadJSON("operation.json", &current); err != nil {
		t.Fatal(err)
	}
	if _, err = advanceNotificationDelivery(ctx, r, paths, d, current, result, lifecycle); err == nil {
		t.Fatal("lost connection reply hidden")
	}
	current = OperationJournal{}
	if err = d.ReadJSON("operation.json", &current); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err = advanceNotificationDelivery(ctx, r, paths, d, current, result, lifecycle)
		if err != nil || result.State != "reporting_connected" || result.Identity == nil || *result.Identity != before.Identity || activations != 1 || reviews != 2 {
			t.Fatal("replayed admission or chose new evidence", result, reviews, activations, err, current)
		}
	}
	if connections != 3 {
		t.Fatal(connections)
	}
	if scoped {
		previousJournal := current.Digest()
		priorHash := r.DesiredHash
		if r.Desired.Backup.Schedule == "hourly" {
			r.Desired.Backup.Schedule = "daily"
		} else {
			r.Desired.Backup.Schedule = "hourly"
		}
		r.DesiredHash, _ = r.Desired.NotificationFingerprint()
		if r.DesiredHash != priorHash {
			t.Fatal("schedule edit changed gateway scope")
		}
		result, err = advanceNotificationDelivery(ctx, r, paths, d, current, result, lifecycle)
		if err != nil || result.State != "delivery_admitted" || connections != 3 || activations != 1 {
			t.Fatal("pending specialist edit repeated delivery", result, err)
		}
		appliedBackupHash = currentBackupHash()
		result, err = advanceNotificationDelivery(ctx, r, paths, d, current, result, lifecycle)
		if err != nil || result.State != "reporting_connected" || connections != 4 || activations != 1 {
			t.Fatal("updated specialist changed gateway admission", result, err)
		}
		var retained OperationJournal
		if e := d.ReadJSON("operation.json", &retained); e != nil || retained.Digest() != previousJournal {
			t.Fatal("schedule edit changed gateway operation", e)
		}
	}
	changed := r
	changed.DesiredHash = protocol.Hash("different desired policy")
	if _, err = advanceNotificationDelivery(ctx, changed, paths, d, current, result, lifecycle); err == nil {
		t.Fatal("retargeted active admission")
	}
}
