//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func deliveryReportRouteFixture(t *testing.T) (string, VerificationNotificationScopeChange, WorkerSetupJournal, *providerScopeDependencies, *credentialReportServices, *install.ManagedReportProfile) {
	home, r, before, deps, services, profile := providerScopeFixtureWithDeclaration(t, &config.NotificationDeclaration{Providers: config.ProviderDeclarations{"telegram": {TokenFile: "~/.config/old-token", ChatIDFile: "~/.config/old-chat"}}, Routes: map[string]config.NotificationRoute{"alerts": {Primary: "telegram"}, "audit": {Primary: "telegram"}}}, nil)
	r.Desired.Notification.Providers = before.Declaration.Desired.Notification.Providers
	r.Desired.Backup.Notifications.Route = "audit"
	deps.credentials = slices.Clone(profile.Credentials)
	deps.profile = func(_ string, id notify.Identity, p notify.Producer, route string) (install.ManagedReportProfile, error) {
		if id != profile.Identity || p != before.Producer.Producer || route != before.Declaration.NotificationRoute && route != "audit" {
			t.Fatal("profile lost route or identity")
		}
		return *profile, nil
	}
	return home, r, before, deps, services, profile
}
func TestDeliveryReportRouteRetainsOwnerAcrossEveryPublication(t *testing.T) {
	for _, point := range []string{"scope-intent", "backup-gated", "report-stopped", "report-files", "report-reloaded", "report-route-published", "gateway-complete", "scope-receipt", "scope-complete"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			home, r, before, deps, services, profile := deliveryReportRouteFixture(t)
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			paths, err := declarationPaths(home, before.Declaration)
			testutil.Must(t, err)
			cutover, err := os.ReadFile(filepath.Join(paths.State, backupCutoverFile))
			testutil.Must(t, err)
			gateway := notify.Hash("route policy")
			calls := 0
			migrate := func(p ProducerReservation) (string, error) {
				if p != *before.Producer || services.active {
					t.Fatal("lost producer/report fence")
				}
				calls++
				profile.ManifestHash = gateway
				return gateway, nil
			}
			lost := errors.New("lost route reply")
			_, err = reconcileNotificationScopeReports(ctx, home, r, migrate, 24, deps, func(at string) error {
				if at == point {
					return lost
				}
				return nil
			})
			if !errors.Is(err, lost) {
				t.Fatal("missed route boundary", err)
			}
			changed := r
			b := *r.Desired.Backup
			changed.Desired.Backup = &b
			b.Schedule = "weekly"
			if _, err = reconcileNotificationScopeReports(ctx, home, changed, migrate, 24, deps); err == nil {
				t.Fatal("changed retry accepted")
			}
			for range 2 {
				_, err = reconcileNotificationScopeReports(ctx, home, r, migrate, 24, deps)
				testutil.Must(t, err)
			}
			var after WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
			if after.Declaration.NotificationRoute != "audit" || after.Shared.Owner != before.Shared.Owner || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || *after.Producer != *before.Producer || !services.active {
				t.Fatal("route changed retained ownership")
			}
			testutil.Must(t, requireReportRouteOwner(after, before.Shared.Owner))
			state, err := openInstalledBackupState(home, false)
			testutil.Must(t, err)
			testutil.Must(t, requireCommandScopeOwner(state, after))
			var binding backupNotificationBinding
			testutil.Must(t, backupTransferTx(ctx, state, func(tx *sql.Tx) error {
				var e error
				binding, e = readBackupNotificationBinding(tx)
				if e != nil {
					return e
				}
				route, e := effectiveBackupReportRoute(tx, binding)
				if route != "audit" {
					t.Fatal("new route not selected", route, e)
				}
				return e
			}))
			state.Close()
			origin, err := commandScopeOrigin(root, after, before.Declaration.NotificationDesiredHash)
			testutil.Must(t, err)
			if origin.Declaration.NotificationRoute != before.Declaration.NotificationRoute {
				t.Fatal("original route rewritten")
			}
			retained, err := os.ReadFile(filepath.Join(paths.State, backupCutoverFile))
			testutil.Must(t, err)
			if string(retained) != string(cutover) {
				t.Fatal("cutover rewritten")
			}
			if calls < 1 {
				t.Fatal("migration missing")
			}
		})
	}
}
func freezeRouteReport(t *testing.T, home string, before WorkerSetupJournal, id string) []notify.Message {
	t.Helper()
	state, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	defer state.Close()
	now := time.Now().UTC()
	var messages []notify.Message
	testutil.Must(t, backupTransferTx(context.Background(), state, func(tx *sql.Tx) error {
		if err := enqueueNotification(tx, notification.NotificationEvent{ID: id, Kind: "daily", Message: "current report"}, now); err != nil {
			return err
		}
		e := backupNotificationEvaluation{Snapshot: notify.Snapshot{Producer: before.Producer.Producer, Sequence: 1, BasisRevision: 1, ConfigRevision: "one", EvaluatedAt: now, Subjects: []notify.Subject{{Key: "backup", Condition: "critical", Reason: "fixture", ValidUntil: now.Add(time.Hour)}}}, ReportText: "current report"}
		var err error
		messages, err = freezeBackupNotificationPass(tx, e)
		return err
	}))
	return messages
}
func TestDeliveryReportRouteFreezesNewMessagesAndRefusesPendingExports(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "pending"}[pending], func(t *testing.T) {
			home, r, before, deps, _, profile := deliveryReportRouteFixture(t)
			if pending {
				if len(freezeRouteReport(t, home, before, "before-route")) == 0 {
					t.Fatal("missing frozen report")
				}
			}
			called := false
			_, err := reconcileNotificationScopeReports(context.Background(), home, r, func(ProducerReservation) (string, error) { called = true; return profile.ManifestHash, nil }, 24, deps)
			if pending {
				if err == nil || called {
					t.Fatal("pending report was retargeted", err)
				}
				return
			}
			testutil.Must(t, err)
			messages := freezeRouteReport(t, home, before, "after-route")
			if len(messages) == 0 {
				t.Fatal("missing new report")
			}
			for _, m := range messages {
				if m.Route != "audit" {
					t.Fatal("new message used old route", m.Route)
				}
			}
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			stateForDamage, err := openInstalledBackupState(home, false)
			testutil.Must(t, err)
			var routeBody string
			testutil.Must(t, stateForDamage.db.QueryRow("SELECT body FROM backup_notification_report_route WHERE singleton=1").Scan(&routeBody))
			_, err = stateForDamage.db.Exec("DELETE FROM backup_notification_report_route")
			testutil.Must(t, err)
			err = backupTransferTx(context.Background(), stateForDamage, func(tx *sql.Tx) error {
				b, e := readBackupNotificationBinding(tx)
				if e != nil {
					return e
				}
				_, e = effectiveBackupReportRoute(tx, b)
				return e
			})
			if err == nil {
				t.Fatal("missing route SQL authority fell back to old audience")
			}
			_, err = stateForDamage.db.Exec("INSERT INTO backup_notification_report_route VALUES(1,?)", routeBody)
			testutil.Must(t, err)

			_, err = stateForDamage.db.Exec("DROP TABLE backup_notification_report_route")
			testutil.Must(t, err)
			err = backupTransferTx(context.Background(), stateForDamage, func(tx *sql.Tx) error {
				b, e := readBackupNotificationBinding(tx)
				if e != nil {
					return e
				}
				_, e = effectiveBackupReportRoute(tx, b)
				return e
			})
			if err == nil {
				t.Fatal("missing route table fell back to old audience")
			}
			_, err = stateForDamage.db.Exec("CREATE TABLE backup_notification_report_route(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL)")
			testutil.Must(t, err)
			_, err = stateForDamage.db.Exec("INSERT INTO backup_notification_report_route VALUES(1,?)", routeBody)
			testutil.Must(t, err)
			stateForDamage.Close()
			testutil.Must(t, os.Remove(filepath.Join(home, ".local/state/deploy/backup-worker/notification-scope-"+r.Operation+".json")))
			state, err := openInstalledBackupState(home, false)
			testutil.Must(t, err)
			defer state.Close()
			if err = backupTransferTx(context.Background(), state, func(tx *sql.Tx) error {
				b, e := readBackupNotificationBinding(tx)
				if e != nil {
					return e
				}
				_, e = effectiveBackupReportRoute(tx, b)
				return e
			}); err == nil {
				t.Fatal("missing route proof accepted")
			}
		})
	}
}
