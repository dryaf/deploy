package workerctl

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSharedLEDReviewRefusesUnappliedBackupBeforeIntent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, kind := range []string{"mapping", "output"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newMappingFixture(t, true, true)
			_, err := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
			testutil.Must(t, err)
			before := f.r.Desired
			n := *before.Notification
			f.r.Desired.Notification = &n
			b := *before.Backup
			if b.Schedule == "daily" {
				b.Schedule = "hourly"
			} else {
				b.Schedule = "daily"
			}
			f.r.Desired.Backup = &b
			if kind == "mapping" {
				binding := n.StatusLEDs.Bindings["backup"]
				binding.Subjects = []string{"source"}
				n.StatusLEDs.Bindings = map[string]config.LEDBindingDeclaration{"backup": binding}
			} else {
				n.StatusLEDs.Enabled = false
			}
			f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
			testutil.Must(t, err)
			l := notificationRemappingLifecycle{notificationMappingLifecycle: notificationMappingLifecycle{inspect: f.inspect}, scopeReview: func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
				return backup.SetupObservation{Stage: "complete"}, &before, nil
			}}
			tree := planTree(t, f.paths.Control)
			var handled bool
			if kind == "mapping" {
				_, handled, err = reconcileDeclaredLEDMappings(ctx, f.r, f.paths, "/helper", l)
			} else {
				_, handled, err = reconcileEnrolledLEDOutput(ctx, f.r, f.paths, "/helper", l, notificationLEDLifecycle{inspect: f.inspect, set: func(context.Context, string, string, bool) (install.ManagedManifest, error) {
					t.Fatal("changed output before backup review")
					return install.ManagedManifest{}, nil
				}})
			}
			if !handled || err == nil || !strings.Contains(err.Error(), "backup-only changes") {
				t.Fatal(handled, err)
			}
			if !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
				t.Fatal("refusal retained a stranded gateway intent")
			}
		})
	}
}
