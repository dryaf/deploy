package workerctl

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func reserveUnboundPolicyFixture(t *testing.T, r *ControlRequest, paths ResolvedPaths, d *safefs.Directory) {
	t.Helper()
	withBackup, _ := bootstrapControlFixture(t)
	r.Desired.Backup = withBackup.Desired.Backup
	r.DesiredHash, _ = declarationFingerprint(r.Desired, r.DesiredScope)
	testutil.Must(t, retainDeclaration(d, r.Desired, r.DesiredHash, r.DesiredScope))
	var j OperationJournal
	testutil.Must(t, d.ReadJSON("operation.json", &j))
	j.Pins.DesiredHash = r.DesiredHash
	testutil.Must(t, d.WriteJSON("operation.json", j, true))
	root, err := safefs.Open(r.Facts.Home)
	testutil.Must(t, err)
	defer root.Close()
	testutil.Must(t, root.Mkdir(".local/state/deploy/backup-worker"))
	testutil.Must(t, root.Mkdir(".local/state/deploy/locks"))
	lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	testutil.Must(t, err)
	lease.Close()
	w, a, setup := testutil.ID(t, "w"), testutil.ID(t, "a"), testutil.ID(t, "n")
	reservation := backup.ProducerReservation{Version: 1, Producer: protocol.Producer{ID: w, Epoch: testutil.ID(t, "n")}, Generation: testutil.ID(t, "g")}
	request := protocol.Hash("reserved setup request")
	declared := backup.SetupDeclaration{Version: 1, UID: os.Getuid(), Home: r.Facts.Home, DesiredHash: r.DesiredHash, NotificationDesiredHash: r.DesiredHash, NotificationRoot: paths.Installation, NotificationRoute: r.Desired.Backup.Notifications.Route}
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", backup.WorkerSetupJournal{Schema: 2, ID: setup, WorkerID: w, ArchiveID: a, Stage: "started", RequestHash: request, Declaration: &declared, Producer: &reservation}, false))
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/producer-reservation.json", map[string]any{"version": 1, "setup_id": setup, "worker_id": w, "archive_id": a, "request_hash": request, "reservation": reservation}, false))
}

func TestReservedBackupBlocksUnboundPolicyChangesBeforeEffects(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, kind := range []string{"draft", "limits", "led", "activity", "helper", "catalog"} {
		t.Run(kind, func(t *testing.T) {
			r, paths, observed, d := notificationLimitsFixture(t)
			reserveUnboundPolicyFixture(t, &r, paths, d)
			switch kind {
			case "draft":
				setCommandDraft(&r)
			case "limits":
				r.Desired.Notification.Limits.MaxMessages++
			case "led":
				r.Desired.Notification.StatusLEDs.Enabled = false
			case "activity":
				value := false
				r.Desired.Notification.Enabled = &value
			case "helper":
				r.HelperSHA256 = strings.Repeat("c", 64)
			case "catalog":
				r.Desired.Notification.Artifacts.Catalog = "/operator/another-catalog"
			}
			r.DesiredHash, _ = declarationFingerprint(r.Desired, r.DesiredScope)
			before := planTree(t, r.Facts.Home)
			var handled bool
			var err error
			switch kind {
			case "draft":
				_, handled, err = reconcileNotificationCommandDraft(context.Background(), r, paths, "/helper", notificationLimitsLifecycle{})
			case "limits":
				_, handled, err = reconcileNotificationLimits(context.Background(), r, paths, "/helper", notificationLimitsLifecycle{})
			case "led":
				_, handled, err = reconcileDeclaredLEDOutput(context.Background(), r, paths, notificationLEDLifecycle{})
			case "activity":
				_, handled, err = reconcileNotificationActivity(context.Background(), r, paths, notificationActivityLifecycle{})
			default:
				_, handled, err = reconcileNotificationRelease(context.Background(), r, paths, "/helper", notificationReleaseLifecycle{})
			}
			if !handled || !errors.Is(err, errReservedNotificationConsumer) || controlFailure(err).Code != "producer_migration_required" {
				t.Fatal("reserved consumer did not fence policy change", handled, err)
			}
			plan, err := planNotificationControl(context.Background(), r, paths, func(context.Context, string) (install.InstallationObservation, error) { return *observed, nil })
			testutil.Must(t, err)
			if plan.Code != "producer_migration_required" || plan.State != "needs_action" {
				t.Fatal("plan advertised an unsupported reserved-consumer change", plan)
			}
			if !reflect.DeepEqual(before, planTree(t, r.Facts.Home)) {
				t.Fatal("rejected change published an intent or modified reserved state")
			}
		})
	}
}
