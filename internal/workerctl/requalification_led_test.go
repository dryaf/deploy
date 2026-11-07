package workerctl

import (
	"context"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func toggleRequalificationLEDOutput(t *testing.T, f *mappingFixture, enabled bool, receipts map[string]string) {
	t.Helper()
	n := *f.r.Desired.Notification
	n.StatusLEDs.Enabled = enabled
	f.r.Desired.Notification = &n
	f.r.DesiredHash, _ = f.r.Desired.NotificationFingerprint()
	l := deliveryOutputLifecycle{
		output: notificationLEDLifecycle{inspect: f.inspect, set: func(_ context.Context, root, hash string, disabled bool) (install.ManagedManifest, error) {
			if root != f.paths.Installation || hash != protocol.Hash(f.o.Manifest) {
				t.Fatal("unreviewed LED output")
			}
			f.o.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: testutil.ID(t, "n"), Disabled: disabled}
			return f.o.Manifest, nil
		}},
		review: func(_ context.Context, _, hash string, _ bool) (install.ManagedManifest, error) {
			if hash != protocol.Hash(f.o.Manifest) {
				t.Fatal("unreviewed output review")
			}
			return f.o.Manifest, nil
		},
		inputs: func(r ControlRequest, p ResolvedPaths, d *safefs.Directory, b config.DesiredWorkers) error {
			return verifyLEDOutputInputs(r, p, d, b)
		},
		scope: func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, change func(backup.ProducerReservation) (string, error)) (string, error) {
			receipt, err := change(f.reservation)
			if err == nil {
				receipts[r.Operation] = receipt
			}
			return receipt, err
		},
		scopeInspect: func(_, op, _, _ string) (string, bool, error) { receipt, ok := receipts[op]; return receipt, ok, nil },
		connect:      func(context.Context, string, string, string) error { return nil },
	}
	result, handled, err := reconcileDeliveryLEDOutput(context.Background(), f.r, f.paths, l)
	testutil.Must(t, err)
	if !handled || result.State != "delivery_admitted" {
		t.Fatal("LED output did not complete", result)
	}
	j := f.journal(t)
	testutil.Must(t, verifyMappingScope(f.d, j, l.scopeInspect))
	saved := receipts[j.Children[0].Operation]
	delete(receipts, j.Children[0].Operation)
	if verifyMappingScope(f.d, j, l.scopeInspect) == nil {
		t.Fatal("missing backup output receipt ignored")
	}
	receipts[j.Children[0].Operation] = saved
}
