package workerctl

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func commandDraftFixture(t *testing.T) (ControlRequest, ResolvedPaths, *install.InstallationObservation, *safefs.Directory) {
	t.Helper()
	r, paths, o, d := notificationLimitsFixture(t)
	r.Desired.Backup = &config.BackupDeclaration{Freshness: "26h", Notifications: config.DesiredBackupNotifications{Worker: r.Server, Route: "alerts"}}
	r.DesiredScope = 1
	var err error
	r.DesiredHash, err = r.Desired.NotificationFingerprint()
	testutil.Must(t, err)
	testutil.Must(t, retainDeclaration(d, r.Desired, r.DesiredHash, r.DesiredScope))
	var j OperationJournal
	testutil.Must(t, d.ReadJSON("operation.json", &j))
	j.Pins.DesiredHash, j.Pins.DesiredScope = r.DesiredHash, r.DesiredScope
	testutil.Must(t, d.WriteJSON("operation.json", j, true))
	return r, paths, o, d
}

func setCommandDraft(r *ControlRequest) {
	p := r.Desired.Notification.Providers["telegram"]
	p.Account, p.BotName = "123", "fixture_bot"
	r.Desired.Notification.Providers = config.ProviderDeclarations{"telegram": p}
	r.Desired.Notification.Commands = config.CommandDeclaration{Enabled: true, Mode: "read", Workers: map[string]string{"backup": "backup-worker"}, Conversations: map[string]config.ConversationDeclaration{"ops": {Provider: "telegram", Account: "123", NativeID: "456"}}, Permissions: []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}}
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
}

func TestCommandDraftRetainsRuntimeAcrossLostPublication(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, boundary := range []string{"limits-intent", "command-draft-published"} {
		t.Run(boundary, func(t *testing.T) {
			r, paths, o, d := commandDraftFixture(t)
			original := protocol.Hash(o.Manifest)
			raw, err := readLimitsRuntime(paths.Runtime)
			testutil.Must(t, err)
			setCommandDraft(&r)
			stop := errors.New("lost declaration reply")
			l := notificationLimitsLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }, migrate: func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error) {
				t.Fatal("draft restarted gateway")
				return install.ManagedManifest{}, nil
			}, boundary: func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}}
			_, handled, err := reconcileNotificationCommandDraft(context.Background(), r, paths, "/helper", l)
			if !handled || !errors.Is(err, stop) {
				t.Fatal(handled, err)
			}
			l.boundary = nil
			for range 2 {
				result, handled, err := reconcileNotificationCommandDraft(context.Background(), r, paths, "/helper", l)
				if err != nil || !handled || result.State != "waiting_for_producers" {
					t.Fatal(result, handled, err)
				}
			}
			saved, err := readLimitsRuntime(paths.Runtime)
			testutil.Must(t, err)
			if string(saved) != string(raw) || protocol.Hash(o.Manifest) != original {
				t.Fatal("draft changed running installation")
			}
			var j OperationJournal
			testutil.Must(t, d.ReadJSON("operation.json", &j))
			testutil.Must(t, verifyReleaseReceipt(d, j, o.Manifest))
			result, err := planNotificationControl(context.Background(), r, paths, l.inspect)
			if err != nil || result.State != "up_to_date" {
				t.Fatal(result, err)
			}
			status, err := inspectNotificationControl(context.Background(), r, paths, l.inspect, nil)
			if err != nil || status.State != "waiting_for_producers" {
				t.Fatal(status, err)
			}
			// A subsequent draft can toggle or revise policy without discarding
			// its preceding immutable declaration or runtime proof.
			r.Desired.Notification.Commands.Enabled = false
			r.Desired.Notification.Commands.Permissions[0].Actor = "987"
			r.DesiredHash, err = r.Desired.NotificationFingerprint()
			testutil.Must(t, err)
			_, handled, err = reconcileNotificationCommandDraft(context.Background(), r, paths, "/helper", l)
			if err != nil || !handled {
				t.Fatal(handled, err)
			}
			testutil.Must(t, d.ReadJSON("operation.json", &j))
			testutil.Must(t, verifyReleaseReceipt(d, j, o.Manifest))
			before := planTree(t, paths.Control)
			_, _, err = reconcileNotificationCommandDraft(context.Background(), r, paths, "/helper", l)
			testutil.Must(t, err)
			if !reflect.DeepEqual(before, planTree(t, paths.Control)) {
				t.Fatal("completed draft mutated history")
			}
		})
	}
}

func TestCommandDraftRejectsWrongBotAndInstalledDriftBeforeJournal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, kind := range []string{"bot", "runtime", "installed", "reserved"} {
		t.Run(kind, func(t *testing.T) {
			r, paths, o, _ := commandDraftFixture(t)
			setCommandDraft(&r)
			switch kind {
			case "bot":
				p := r.Desired.Notification.Providers["telegram"]
				p.Account = "999"
				r.Desired.Notification.Providers["telegram"] = p
				c := r.Desired.Notification.Commands.Conversations["ops"]
				c.Account = "999"
				r.Desired.Notification.Commands.Conversations["ops"] = c
			case "runtime":
				testutil.Must(t, os.WriteFile(paths.Runtime, []byte("{}"), 0600))
			case "installed":
				o.Unbound = false
			case "reserved":
				testutil.Must(t, os.MkdirAll(r.Facts.Home+"/.local/state/deploy/backup-worker", 0700))
				testutil.Must(t, os.WriteFile(r.Facts.Home+"/.local/state/deploy/backup-worker/setup.json", []byte("{}"), 0600))
			}
			r.DesiredHash, _ = r.Desired.NotificationFingerprint()
			before := planTree(t, paths.Control)
			_, _, err := reconcileNotificationCommandDraft(context.Background(), r, paths, "/helper", notificationLimitsLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }})
			if err == nil {
				t.Fatal("invalid draft accepted")
			}
			if !reflect.DeepEqual(before, planTree(t, paths.Control)) {
				t.Fatal("refused draft retained journal")
			}
		})
	}
}
