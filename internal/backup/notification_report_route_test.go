//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func reportRouteFixture(t *testing.T) (string, VerificationNotificationScopeChange, WorkerSetupJournal, WorkerSetupRequest) {
	t.Helper()
	home, request, j := verificationScopeFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	c, err := loadInstalledWorker(home)
	testutil.Must(t, err)
	j.Declaration.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "telegram"}, "audit": {Primary: "telegram"}}
	j.Declaration.DesiredHash, err = j.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
	j.Declaration.NotificationDesiredHash, err = j.Declaration.Desired.NotificationFingerprint()
	testutil.Must(t, err)
	r := WorkerSetupRequest{Schema: 1, Operation: "apply", Policy: c.Policy, Image: c.Image, RelayImage: c.RelayImage, Declaration: j.Declaration}
	paths, err := declarationPaths(home, j.Declaration)
	testutil.Must(t, err)
	for _, s := range c.Sources {
		known, e := readBackupOwnedFile(root, paths.OwnedPath(s.KnownHosts), 16384)
		testutil.Must(t, e)
		r.Sources = append(r.Sources, SetupSource{s.Alias, s.Host, s.User, s.Port, string(known)})
	}
	j.RequestHash = workerSetupRequestHash(r)
	testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
	raw, err := json.Marshal(j.Declaration.Desired)
	testutil.Must(t, err)
	testutil.Must(t, json.Unmarshal(raw, &request.Desired))
	request.BeforeScope = j.Declaration.NotificationDesiredHash
	request.Desired.Backup.Notifications.Route = "audit"
	d := *r.Declaration
	d.Desired = &request.Desired
	d.DesiredHash, err = request.Desired.Fingerprint()
	testutil.Must(t, err)
	d.NotificationDesiredHash, err = request.Desired.NotificationFingerprint()
	testutil.Must(t, err)
	d.NotificationRoute = "audit"
	r.Declaration = &d
	return home, request, j, r
}

func TestSharedReportRouteScopeRetainsRuntimeAcrossLostReply(t *testing.T) {
	home, r, before, backupRequest := reportRouteFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	d, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer d.Close()
	handled, err := inspectCoordinatedNotificationReservation(root, d, before, backupRequest)
	if !handled || err != nil {
		t.Fatal("backup command cannot route selection", err)
	}
	stop := errors.New("lost gateway reply")
	_, err = ReconcileVerificationReportRouteScope(context.Background(), home, r, func(ProducerReservation) (string, error) { return "", stop })
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	var pending WorkerSetupJournal
	testutil.Must(t, d.ReadJSON("setup.json", &pending))
	handled, err = inspectCoordinatedNotificationReservation(root, d, pending, backupRequest)
	if !handled || err != nil {
		t.Fatal("backup command cannot resume selection", err)
	}
	called := 0
	migrate := func(p ProducerReservation) (string, error) {
		called++
		if p != *before.Producer {
			t.Fatal("producer replaced")
		}
		return notify.Hash("gateway policy"), nil
	}
	_, err = ReconcileVerificationReportRouteScope(context.Background(), home, r, migrate)
	testutil.Must(t, err)
	var after WorkerSetupJournal
	testutil.Must(t, d.ReadJSON("setup.json", &after))
	if !reflect.DeepEqual(before.OwnedFiles, after.OwnedFiles) || !reflect.DeepEqual(before.SourceKeys, after.SourceKeys) || after.RequestHash == before.RequestHash || after.Declaration.NotificationRoute != "audit" || *after.Producer != *before.Producer {
		t.Fatal("report route changed runtime, source or producer authority")
	}
	_, present, err := InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, "audit")
	if err != nil || !present {
		t.Fatal("new route lost reservation", err)
	}
	if _, _, err = InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, "alerts"); err == nil {
		t.Fatal("old route still admitted")
	}
	tree := policyReviewTree(t, home)
	_, err = ReconcileVerificationReportRouteScope(context.Background(), home, r, migrate)
	testutil.Must(t, err)
	if called != 1 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("completed retry replayed migration")
	}
}

func TestSharedReportRouteRejectsMixedAndUnknownDestinations(t *testing.T) {
	for _, change := range []string{"unknown", "provider", "freshness", "explicit-producer", "active"} {
		t.Run(change, func(t *testing.T) {
			home, r, _, _ := reportRouteFixture(t)
			switch change {
			case "unknown":
				r.Desired.Backup.Notifications.Route = "unknown"
			case "provider":
				r.Desired.Notification.Routes["audit"] = config.NotificationRoute{Primary: "signal"}
			case "freshness":
				r.Desired.Backup.Freshness = "48h"
			case "explicit-producer":
				r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"audit"}}}
			case "active":
				s, e := openInstalledBackupState(home, false)
				testutil.Must(t, e)
				testutil.Must(t, s.put("installation", "active"))
				s.Close()
			}
			before := policyReviewTree(t, home)
			called := false
			_, err := ReconcileVerificationReportRouteScope(context.Background(), home, r, func(ProducerReservation) (string, error) { called = true; return notify.Hash("bad"), nil })
			if err == nil || called || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
				t.Fatal("invalid report route changed state", err)
			}
		})
	}
}
