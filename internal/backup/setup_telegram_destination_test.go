//go:build linux

package backup

import (
	"database/sql"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupTelegramDestinationReviewRequiresSettledWork(t *testing.T) {
	for _, kind := range []string{"empty", "accepted", "queued", "covered", "reserved", "other-bot"} {
		t.Run(kind, func(t *testing.T) {
			home, r := backupTokenRotationFixture(t)
			moveBackupChatReference(t, home, &r, true)
			testutil.Must(t, os.WriteFile(r.Policy.Notifications.Telegram.ChatIDFile, []byte("-100987654321"), 0600))
			if kind == "other-bot" {
				testutil.Must(t, os.WriteFile(r.Policy.Notifications.Telegram.TokenFile, []byte("999:another-bot"), 0600))
			}
			state, err := openInstalledBackupState(home, false)
			testutil.Must(t, err)
			if kind == "reserved" {
				c, e := loadInstalledWorker(home)
				testutil.Must(t, e)
				rotationDeliveryHistory(t, state, c)
			} else if kind == "queued" || kind == "covered" || kind == "accepted" {
				ledgerTx(t, state, func(tx *sql.Tx) error {
					return enqueueNotification(tx, notification.NotificationEvent{ID: "previous-alert", Kind: "failure", Message: "old destination"}, time.Now().UTC())
				})
				if kind == "accepted" {
					_, err = state.db.Exec("UPDATE outbox SET accepted='2026-11-07T12:00:00Z'")
					testutil.Must(t, err)
				}
				if kind == "covered" {
					_, err = state.db.Exec("UPDATE outbox SET covered_by='unresolved-batch'")
					testutil.Must(t, err)
				}
			}
			state.Close()
			root, stateRoot, _, _ := expansionRoots(t, home)
			var j WorkerSetupJournal
			testutil.Must(t, stateRoot.ReadJSON("setup.json", &j))
			before := policyReviewTree(t, home)
			err = beginWorkerSetupUpdate(root, stateRoot, &j, r)
			allowed := kind == "empty" || kind == "accepted"
			if (err == nil) != allowed {
				t.Fatal("unexpected destination review", err)
			}
			if !allowed {
				if !reflect.DeepEqual(before, policyReviewTree(t, home)) {
					t.Fatal("rejected destination retained state")
				}
				return
			}
			if j.TelegramRotation == nil || j.TelegramRotation.Version != 3 {
				t.Fatal("new audience lacks typed evidence")
			}
			testutil.Must(t, checkSetupTelegramRotation(j, &r, false))
			j.TelegramRotation.Version = 2
			if err = checkSetupTelegramRotation(j, &r, false); err == nil {
				t.Fatal("audience migration accepted a version downgrade")
			}
			j.TelegramRotation.Version = 3
			testutil.Must(t, os.WriteFile(r.Policy.Notifications.Telegram.ChatIDFile, []byte("888"), 0600))
			if err = checkSetupTelegramRotation(j, &r, false); err == nil {
				t.Fatal("candidate destination drift ignored")
			}
			testutil.Must(t, checkSetupTelegramRotation(j, nil, true))
		})
	}
}

func TestBackupTelegramDestinationRechecksQueueBeforePublication(t *testing.T) {
	home, r := backupTokenRotationFixture(t)
	moveBackupChatReference(t, home, &r, false)
	testutil.Must(t, os.WriteFile(r.Policy.Notifications.Telegram.ChatIDFile, []byte("987654"), 0600))
	root, stateRoot, _, _ := expansionRoots(t, home)
	var j WorkerSetupJournal
	testutil.Must(t, stateRoot.ReadJSON("setup.json", &j))
	testutil.Must(t, beginWorkerSetupUpdate(root, stateRoot, &j, r))
	state, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	defer state.Close()
	ledgerTx(t, state, func(tx *sql.Tx) error {
		return enqueueNotification(tx, notification.NotificationEvent{ID: "arrived-after-review", Kind: "failure", Message: "retained old audience"}, time.Now().UTC())
	})
	if err = requireSettledBackupDestination(state); err == nil {
		t.Fatal("new pending work ignored after preflight")
	}
	testutil.Must(t, checkSetupTelegramRotation(j, nil, true))
}

func TestBackupTelegramDestinationInstallsAndRecoversPublication(t *testing.T) {
	testBackupPolicyPublication(t, "telegram-destination")
}
